// The read-only work views outside the session: one page of this slot's work
// tasks, and one page of a task's turns.
//
// Why these exist beside the projected ledger view (session_work_bridge.go):
// the projection answers a caller that speaks the ledger's shape, and that
// shape has nowhere to put what the agreed model actually carries — a task's
// description and completion criteria, and a turn's instruction, the session's
// own final answer, the parent's explanation, its question and the artifacts.
// A reader of the projection alone can therefore never show the work; it can
// only show the fields the ledger already had. These views carry the agreed
// records themselves.
//
// A turn is the timeline of new work, so this is also where the app's history
// of a task created after the cutover comes from. The ledger's own history
// stays where it is and keeps its own route: a past management event is not a
// turn, and dressing one up as a turn would put words in a person's mouth.
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
	// sessionDBWorkTasksPath and sessionDBWorkTurnsPath are these views' paths
	// on the sidecar. Keep in sync with data-plane/cmd/agent (agentWorkTasksPath
	// derivation) and control-plane/internal/adapter/k8s.
	sessionDBWorkTasksPath = "/session-db/work/tasks"
	sessionDBWorkTurnsPath = "/session-db/work/turns"
)

// serveWorkTasks answers one keyset page of work tasks, reusing the same
// listWorkTasks the session's own `db` MCP calls — one implementation, so a
// reader outside the session can never see a different task than the session
// does. Each record carries the agreed task columns plus the two facts a
// reader would otherwise need a call per task for: how many turns it has and
// what its latest turn is.
func (s *sessionDB) serveWorkTasks(w http.ResponseWriter, r *http.Request) {
	if !workViewPreamble(w, r, "the work task view") {
		return
	}
	query := r.URL.Query()
	limit, err := ledgerViewLimit(query.Get("limit"))
	if err != nil {
		writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	result, err := s.workTasksView(ctx, workArgs{
		Limit:     limit,
		AfterID:   strings.TrimSpace(query.Get("after_id")),
		Status:    strings.TrimSpace(query.Get("status")),
		SessionID: strings.TrimSpace(query.Get("session_id")),
	})
	writeWorkView(w, s, "work task view", result, err)
}

// serveWorkTurns answers one keyset page of one task's turns, with every agreed
// field on each turn. open=true narrows it to the turns that still need the
// parent, which is what a reader showing "waiting on somebody" asks for.
func (s *sessionDB) serveWorkTurns(w http.ResponseWriter, r *http.Request) {
	if !workViewPreamble(w, r, "the work turn view") {
		return
	}
	query := r.URL.Query()
	limit, err := ledgerViewLimit(query.Get("limit"))
	if err != nil {
		writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	args := workArgs{TaskID: strings.TrimSpace(query.Get("task_id")), Limit: limit}
	if args.TaskID == "" {
		writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", "task_id is required")
		return
	}
	if raw := strings.TrimSpace(query.Get("after_turn_id")); raw != "" {
		after, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil || after < 0 {
			writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", "after_turn_id must be a nonnegative number")
			return
		}
		args.AfterTurnID = &after
	}
	if raw := strings.TrimSpace(query.Get("open")); raw != "" {
		open, convErr := strconv.ParseBool(raw)
		if convErr != nil {
			writeLedgerViewError(w, http.StatusBadRequest, "invalid_argument", "open must be true or false")
			return
		}
		args.Open = &open
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	result, err := s.workTurnsView(ctx, args)
	writeWorkView(w, s, "work turn view", result, err)
}

// workViewPreamble is the method and origin check both views share. The sidecar
// binds loopback, so in this deployment every request already comes from inside
// the pod; the check is here so that a future wider bind cannot quietly turn the
// slot's work records into something the pod network can read.
func workViewPreamble(w http.ResponseWriter, r *http.Request, what string) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, what+" accepts GET", http.StatusMethodNotAllowed)
		return false
	}
	if !fromLoopback(r) {
		http.Error(w, what+" is only for this pod", http.StatusForbidden)
		return false
	}
	return true
}

// writeWorkView is the one answer path for both views, so a work error is
// classified the same way the ledger view classifies its own.
func writeWorkView(w http.ResponseWriter, s *sessionDB, what string, result any, err error) {
	if err != nil {
		var known ledgerError
		if errors.As(err, &known) {
			status := http.StatusBadRequest
			switch known.Code {
			case "not_found":
				status = http.StatusNotFound
			case "schema_version", "conflict":
				status = http.StatusConflict
			}
			writeLedgerViewError(w, status, known.Code, known.ErrorText)
			return
		}
		// A SQLite message can name the database file; the reader outside the
		// session gets the class only, and the detail stays in the log.
		s.logger.Info(what+" failed", "err", err)
		writeLedgerViewError(w, http.StatusInternalServerError, "storage_error", "the work records could not be read")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.Info(what+" could not be written", "err", err)
	}
}
