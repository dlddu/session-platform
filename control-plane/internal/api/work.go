// The HTTP surface of the agreed work records (AC-N9).
//
// Both routes are reads, scoped like every other session-id route, and passive
// in the same way the ledger reads are: they neither restore a snapshot nor
// count as an access, so opening a task never wakes a session.
package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// listWorkTasks reads one keyset page of the slot's work tasks: the agreed task
// columns, how many turns each has, its latest turn, and whether it came from
// the ledger.
func (a *API) listWorkTasks(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	limit, err := workLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, err)
		return
	}
	query := r.URL.Query()
	page, err := a.mgr.WorkTasks(r.Context(), r.PathValue("id"), limit,
		strings.TrimSpace(query.Get("after_id")),
		strings.TrimSpace(query.Get("status")),
		strings.TrimSpace(query.Get("session_id")))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// listWorkTurns reads one keyset page of one task's turns, oldest first: what a
// person said, what went to the session doing the work, that session's own
// answer, the parent's explanation, its question and choices, the artifacts and
// the times. open=true narrows it to the turns that still need the parent.
func (a *API) listWorkTurns(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	limit, err := workLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, err)
		return
	}
	after := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after_turn_id")); raw != "" {
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			writeErr(w, fmt.Errorf("%w: after_turn_id must be a nonnegative number", session.ErrInvalidInput))
			return
		}
	}
	openOnly := false
	if raw := strings.TrimSpace(r.URL.Query().Get("open")); raw != "" {
		openOnly, err = strconv.ParseBool(raw)
		if err != nil {
			writeErr(w, fmt.Errorf("%w: open must be true or false", session.ErrInvalidInput))
			return
		}
	}
	page, err := a.mgr.WorkTurns(r.Context(), r.PathValue("id"), r.PathValue("taskID"), after, limit, openOnly)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// createWorkRequestReq is one request to store. The task is in the path, so the
// body cannot name a different one than the URL it was sent to.
type createWorkRequestReq struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Author string `json:"author"`
	// ChoiceTurnID, when set, is the turn whose offered choices the person
	// picked from. The slot refuses a pick from a turn the task has moved past,
	// so a screen left open cannot act on a question that is no longer being
	// asked. Free text omits it and is never refused for staleness.
	ChoiceTurnID *int64 `json:"choice_turn_id"`
}

// createWorkRequest stores one person's request as the next turn of a task
// (AC-N10, the one write on the work path).
//
// It writes no task field: the slot appends a turn, so a request arriving while
// the session is updating that same task cannot take the update out from under
// it. It starts no turn of the person's own conversation and restores no
// snapshot. The answer never says the request has been dealt with — only the
// turn ending says that — and it distinguishes a second thing from the first:
// whether the slot was able to hand the task and turn to its monitor lane,
// which it does when the task was waiting for a person's judgement.
//
// The author is the caller's own derivation from whatever boundary
// authenticated the person; like a task's owner it is bookkeeping, and nothing
// here grants a tool, an approval or a merge on the strength of it. Scoped like
// every other session-id route, so another tenant's session answers exactly
// like a missing one.
func (a *API) createWorkRequest(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	var body createWorkRequestReq
	if err := decodeRequestBody(r.Body, &body, true); err != nil {
		writeErr(w, err)
		return
	}
	stored, err := a.mgr.CreateWorkRequest(r.Context(), r.PathValue("id"), session.WorkRequestInput{
		ID: body.ID, TaskID: r.PathValue("taskID"), Text: body.Text, Author: body.Author,
		ChoiceTurnID: body.ChoiceTurnID,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// 200 for a request this id already stored with the same words, 201 for a
	// first store: a caller retrying an answer it never saw learns which of the
	// two happened without having to compare the turn itself.
	status := http.StatusCreated
	if stored.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, stored)
}

// workLimit reads a work page size; out of range is refused rather than
// clamped, so a caller is never handed a different page than it asked for.
func workLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > session.WorkPageLimitMax {
		return 0, fmt.Errorf("%w: limit must be a number from 1 to %d",
			session.ErrInvalidInput, session.WorkPageLimitMax)
	}
	return limit, nil
}
