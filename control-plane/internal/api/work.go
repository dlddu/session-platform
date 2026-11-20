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
