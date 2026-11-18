// A read-only HTTP view of the chat slot's task ledger (docs/prd/agent-auth-pool.md
// AC-N6), for a reader outside the session — the control plane, and through it
// an app's task panel.
//
// It is deliberately narrower than the `db` MCP beside it: GET only, and the
// only inputs are a page size and a cursor. There is no SQL on this path, so
// this endpoint cannot read any part of the session database but the ledger's
// own task rows, and nothing it serves can change them. The model is not
// involved: the page comes straight from SQLite, so a read never costs a turn
// and never waits on one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const (
	// sessionDBLedgerPath is this view's path on the sidecar. Keep in sync with
	// data-plane/cmd/agent (agentLedgerPath derivation) and
	// control-plane/internal/adapter/k8s (sessionDBLedgerPath).
	sessionDBLedgerPath = "/session-db/ledger/tasks"
)

// serveLedgerTasks answers one keyset page of the ledger, reusing the same
// ledger_list the session's own `db` MCP calls — one implementation, so the
// panel can never see a different ledger than the session does.
func (s *sessionDB) serveLedgerTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "the task ledger view accepts GET", http.StatusMethodNotAllowed)
		return
	}
	// The sidecar binds loopback, so in this deployment every request already
	// comes from inside the pod. The check is here so that a future wider bind
	// cannot quietly turn the slot's ledger into something the pod network can
	// read.
	if !fromLoopback(r) {
		http.Error(w, "the task ledger view is only for this pod", http.StatusForbidden)
		return
	}
	limit, err := ledgerViewLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	result, err := s.ledger(ctx, "ledger_list", ledgerArgs{
		Limit:   limit,
		AfterID: strings.TrimSpace(r.URL.Query().Get("after_id")),
	})
	if err != nil {
		var ledgerErr ledgerError
		if errors.As(err, &ledgerErr) {
			status := http.StatusBadRequest
			if ledgerErr.Code == "schema_version" {
				status = http.StatusConflict
			}
			writeLedgerViewError(w, status, ledgerErr.Code, ledgerErr.ErrorText)
			return
		}
		// A SQLite message can name the database file; the reader outside the
		// session gets the class only, and the detail stays in the log.
		s.logger.Info("task ledger view failed", "err", err)
		writeLedgerViewError(w, http.StatusInternalServerError, "storage_error", "the task ledger could not be read")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.Info("task ledger view could not be written", "err", err)
	}
}

// ledgerViewLimit reads the page size. An absent limit leaves the ledger's own
// default; a limit that is not a number in range is refused rather than
// clamped, so a caller is never silently given a different page than it asked
// for.
func ledgerViewLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 100 {
		return 0, errors.New("limit must be a number from 1 to 100")
	}
	return limit, nil
}

func writeLedgerViewError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": message})
}
