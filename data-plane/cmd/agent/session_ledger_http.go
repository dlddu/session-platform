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

// sessionDBLedgerRequestsPath is where a request from outside the session is
// stored. Keep in sync with data-plane/cmd/agent (agentLedgerRequestsPath
// derivation) and control-plane/internal/adapter/agent.
const sessionDBLedgerRequestsPath = "/session-db/ledger/requests"

// ledgerRequestBody is what the control plane sends. The author is the caller's
// own derivation from the boundary that authenticated the person — the app
// never gets to name it — and it is bookkeeping here, exactly like a task's
// owner: no tool, approval or merge is granted on the strength of it.
type ledgerRequestBody struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
	Author string `json:"author"`
	// ChoiceID, when given, is the session's own offered choice the person
	// picked. Empty means they typed their own words.
	ChoiceID string `json:"choice_id"`
}

// serveLedgerRequests stores one user request on a task of this slot's ledger.
//
// It is the only write this view has, and it is deliberately not a ledger
// write: it appends a row of its own and leaves every task field — details,
// approval, cursors — as the session wrote them. It starts no turn and wakes
// nothing; the parent finds the request the next time it looks at its ledger.
func (s *sessionDB) serveLedgerRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "the task ledger request view accepts POST", http.StatusMethodNotAllowed)
		return
	}
	if !fromLoopback(r) {
		http.Error(w, "the task ledger request view is only for this pod", http.StatusForbidden)
		return
	}
	var body ledgerRequestBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", "body must be {id, task_id, text, author}")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	stored, replayed, err := s.appendLedgerRequest(ctx, body.ID, body.TaskID, body.Text, body.Author, body.ChoiceID, ledgerNow())
	if err != nil {
		var ledgerErr ledgerError
		if errors.As(err, &ledgerErr) {
			status := http.StatusBadRequest
			switch ledgerErr.Code {
			case "not_found":
				status = http.StatusNotFound
			case "conflict", "schema_version":
				status = http.StatusConflict
			}
			writeLedgerViewError(w, status, ledgerErr.Code, ledgerErr.ErrorText)
			return
		}
		s.logger.Info("task ledger request could not be stored", "err", err)
		writeLedgerViewError(w, http.StatusInternalServerError, "storage_error", "the request could not be stored")
		return
	}
	// A replay of a request already stored answers 200 with the same row; a
	// first store answers 201. Either way the caller learns the request is on
	// the slot, which is what it has to tell the person.
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"request": stored, "replayed": replayed}); err != nil {
		s.logger.Info("task ledger request reply could not be written", "err", err)
	}
}

// sessionDBLedgerEventsPath is the task-history view's path on the sidecar.
// Keep in sync with data-plane/cmd/agent (agentLedgerEventsPath derivation) and
// control-plane/internal/adapter/agent.
const sessionDBLedgerEventsPath = "/session-db/ledger/events"

// serveLedgerEvents answers one page of one task's history. GET only, loopback
// only, and keyset by the row's own seq — the same shape as the task view, so a
// reader pages it the same way. There is no SQL on this path and nothing it
// serves changes a row.
func (s *sessionDB) serveLedgerEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "the task history view accepts GET", http.StatusMethodNotAllowed)
		return
	}
	if !fromLoopback(r) {
		http.Error(w, "the task history view is only for this pod", http.StatusForbidden)
		return
	}
	query := r.URL.Query()
	limit, err := ledgerViewLimit(query.Get("limit"))
	if err != nil {
		writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	args := ledgerArgs{ID: strings.TrimSpace(query.Get("task_id")), Limit: limit}
	if raw := strings.TrimSpace(query.Get("after_seq")); raw != "" {
		after, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil || after < 0 {
			writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", "after_seq must be a nonnegative number")
			return
		}
		args.AfterSeq = &after
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	result, err := s.ledger(ctx, "ledger_events_list", args)
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
		s.logger.Info("task history view failed", "err", err)
		writeLedgerViewError(w, http.StatusInternalServerError, "storage_error", "the task history could not be read")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.Info("task history view could not be written", "err", err)
	}
}
