// The work records outside the session: one page of this slot's tasks, one page
// of a task's turns, and the one write a reader out there has — a person's
// request, which becomes the next turn of a task.
//
// These are the whole surface now. The ledger-shaped views that used to sit
// beside them are gone with the ledger (AC-N10), and so is the projection that
// answered them from the work records: a shape with nowhere to put a task's
// description and completion criteria, or a turn's instruction, answer,
// explanation, question and artifacts, could only ever show that work existed.
//
// A task's history is its turns. There is no separate history route any more —
// a past management event was never a turn, and dressing one up as a turn would
// put words in a person's mouth, so the events the ledger kept were not carried
// (session_work_adopt.go).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// sessionDBWorkTasksPath and sessionDBWorkTurnsPath are these views' paths
	// on the sidecar. Keep in sync with data-plane/cmd/agent (agentWorkTasksPath
	// derivation) and control-plane/internal/adapter/k8s.
	sessionDBWorkTasksPath = "/session-db/work/tasks"
	sessionDBWorkTurnsPath = "/session-db/work/turns"
	// sessionDBWorkRequestsPath is where a request from outside the session is
	// stored. It is the one write on this path, and what it writes is a turn:
	// a person's words are an exchange on the task, not a row beside it.
	sessionDBWorkRequestsPath = "/session-db/work/requests"
)

// workRequestBody is what the control plane sends. The author is the caller's
// own derivation from the boundary that authenticated the person — the app
// never gets to name it — and it is bookkeeping, exactly like a task's owner:
// no tool, approval or merge is granted on the strength of it.
type workRequestBody struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Text   string `json:"text"`
	Author string `json:"author"`
	// ChoiceTurnID, when given, is the turn whose offered choices the person
	// picked from. The platform refuses a pick from a turn that is no longer
	// the latest, so a screen left open while the task moved on cannot act on
	// it. Free text omits it and is never refused for staleness.
	ChoiceTurnID *int64 `json:"choice_turn_id,omitempty"`
}

// serveWorkRequests stores one person's request as the next turn of a task.
//
// It starts no turn of the session's own, wakes nothing and restores no
// snapshot: the parent finds it the next time it lists the task's open turns.
// Idempotency is the caller's own id — the same id with the same words answers
// with the turn already appended.
func (s *sessionDB) serveWorkRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "the work request view accepts POST", http.StatusMethodNotAllowed)
		return
	}
	if !fromLoopback(r) {
		http.Error(w, "the work request view is only for this pod", http.StatusForbidden)
		return
	}
	var body workRequestBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", "body must be {id, task_id, text, author}")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionDBCallTimeout)
	defer cancel()
	stored, replayed, err := s.storeWorkRequest(ctx, body, workClock())
	if err != nil {
		var known workError
		if errors.As(err, &known) {
			writeWorkViewError(w, workViewStatus(known.Code), known.Code, known.ErrorText)
			return
		}
		// A SQLite message can name the database file; the reader outside the
		// session gets the class only, and the detail stays in the log.
		s.logger.Info("work request could not be stored", "err", err)
		writeWorkViewError(w, http.StatusInternalServerError, "storage_error", "the request could not be stored")
		return
	}
	// A replay of a request already stored answers 200 with the same turn; a
	// first store answers 201. Either way the caller learns the request is on
	// the slot, which is what it has to tell the person.
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]any{"turn": stored, "replayed": replayed}); err != nil {
		s.logger.Info("work request reply could not be written", "err", err)
	}
}

// storeWorkRequest validates the request and appends it in one transaction, so
// the turn and the record of where it came from commit together.
func (s *sessionDB) storeWorkRequest(
	ctx context.Context, body workRequestBody, now time.Time,
) (workTurnView, bool, error) {
	if !workID.MatchString(body.ID) {
		return workTurnView{}, false, workFail("invalid_argument", "id must be 1..128 letters, digits, _ or -")
	}
	if !workID.MatchString(body.TaskID) {
		return workTurnView{}, false, workFail("invalid_argument", "task_id must be 1..128 letters, digits, _ or -")
	}
	text := strings.TrimSpace(body.Text)
	if text == "" || len(body.Text) > workTurnTextMax {
		return workTurnView{}, false, workFail("invalid_argument",
			"text must be a person's words, at most 65536 characters")
	}
	if len(body.Author) > workTitleMax {
		return workTurnView{}, false, workFail("invalid_argument", "author too long")
	}
	if body.ChoiceTurnID != nil && (*body.ChoiceTurnID < 1 || *body.ChoiceTurnID > requestSafeInt) {
		return workTurnView{}, false, workFail("invalid_argument", "choice_turn_id must be a turn id")
	}
	db, err := s.handle(ctx)
	if err != nil {
		return workTurnView{}, false, err
	}
	var stored workTurnView
	replayed := false
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		var storeErr error
		stored, replayed, storeErr = storeRequestAsTurn(
			ctx, tx, body.ID, body.TaskID, body.Text, body.Author, body.ChoiceTurnID, now)
		return storeErr
	})
	if err != nil {
		return workTurnView{}, false, err
	}
	return stored, replayed, nil
}

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
	limit, err := workViewLimit(query.Get("limit"))
	if err != nil {
		writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", err.Error())
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
	limit, err := workViewLimit(query.Get("limit"))
	if err != nil {
		writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	args := workArgs{TaskID: strings.TrimSpace(query.Get("task_id")), Limit: limit}
	if args.TaskID == "" {
		writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", "task_id is required")
		return
	}
	if raw := strings.TrimSpace(query.Get("after_turn_id")); raw != "" {
		after, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil || after < 0 {
			writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", "after_turn_id must be a nonnegative number")
			return
		}
		args.AfterTurnID = &after
	}
	if raw := strings.TrimSpace(query.Get("open")); raw != "" {
		open, convErr := strconv.ParseBool(raw)
		if convErr != nil {
			writeWorkViewError(w, http.StatusBadRequest, "invalid_argument", "open must be true or false")
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
		var known workError
		if errors.As(err, &known) {
			status := http.StatusBadRequest
			switch known.Code {
			case "not_found":
				status = http.StatusNotFound
			case "schema_version", "conflict":
				status = http.StatusConflict
			}
			writeWorkViewError(w, status, known.Code, known.ErrorText)
			return
		}
		// A SQLite message can name the database file; the reader outside the
		// session gets the class only, and the detail stays in the log.
		s.logger.Info(what+" failed", "err", err)
		writeWorkViewError(w, http.StatusInternalServerError, "storage_error", "the work records could not be read")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		s.logger.Info(what+" could not be written", "err", err)
	}
}
