// The HTTP surface of a manager session's two conversations (AC-N11).
//
// There is no second session id and no second set of routes. A manager session
// is one session whose pod runs the person's chat and, when the deployment
// turns the pair on, the monitor that moves the work beside it — so the lane is
// a parameter of the session's own read, write and stream, and it defaults to
// the chat. A caller that predates the pair sends exactly what it sent before
// and reaches exactly what it reached before.
//
// The inventory route is the one addition, and it is a read: it answers which
// conversations the pod runs and which pod each is in, which is how a reader
// outside the session checks that the pair is in one pod rather than taking the
// control plane's word for it.
package api

import (
	"net/http"
	"strings"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// laneParam is the lane a request names. It is a query parameter on stream and
// a body field on read and write, matching how each of those already carries
// its cursor — not a path segment, because a lane is not a resource of its own.
const laneParam = "lane"

// requestLane reads the lane from a query string. An absent lane is the
// person's chat; a lane that is not one of a manager's is refused rather than
// defaulted (session.NormalizeLane).
func requestLane(r *http.Request) (session.Lane, error) {
	return session.NormalizeLane(strings.TrimSpace(r.URL.Query().Get(laneParam)))
}

// listSessionLanes answers which conversations this session's pod runs.
//
// Like the work reads it is passive: it does not restore a snapshot and does
// not count as an access. A frozen session answers 422 rather than being woken
// to be asked, because a reader checking the pair's shape must not be the thing
// that starts a pod.
func (a *API) listSessionLanes(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSession(w, r) {
		return
	}
	page, err := a.mgr.SessionLanes(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
