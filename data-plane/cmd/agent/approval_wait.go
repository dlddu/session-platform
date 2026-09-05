// The workload pod's record of what AC-F3 is currently waiting on, and the
// endpoint the control plane reads it through.
//
// The control plane pulls the answer rather than this pod pushing it, for the
// same reason the notice feed itself is pulled (session_mcp_notices.go) —
// AC-F2's egress allowlist.
package main

import (
	"encoding/json"
	"net/http"
	"sync"
)

// approvalWaitPath is where the control plane asks.
const approvalWaitPath = "/approval"

// approvalWaits is the set of approval requests this session is blocked on,
// keyed by the external identifier AC-F3 mints. A set rather than a counter:
// a retried poll can deliver the same notice twice, and a counter would drift.
// It is derived state, persisted nowhere (AC-F4).
type approvalWaits struct {
	mu      sync.Mutex
	pending map[string]struct{}
}

// observe folds one poll's answer into the set: a wait that started is added,
// and a wait that ended — by decision, or because the gateway could not be
// asked at all — is removed.
//
// dropped is this tailer having fallen behind the helper's ring. The set is
// cleared in that case, because a lost "decided" would otherwise hold this
// session's lastAccess forever and its pod with it.
func (w *approvalWaits) observe(notices []approvalNotice, dropped int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if dropped > 0 {
		w.pending = nil
	}
	for _, n := range notices {
		if n.ExternalID == "" {
			continue
		}
		switch n.Kind {
		case noticeAwaiting:
			if w.pending == nil {
				w.pending = make(map[string]struct{})
			}
			w.pending[n.ExternalID] = struct{}{}
		case noticeDecided, noticeUnavailable:
			delete(w.pending, n.ExternalID)
		}
		// An unknown kind is ignored, matching renderApprovalNotice: what this
		// agent cannot read must not strand the session's idle count.
	}
}

// state reports whether a human is being waited on, and on how many requests.
func (w *approvalWaits) state() (awaiting bool, pending int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) > 0, len(w.pending)
}

// approvalWaitResponse is the whole answer: whether to hold the idle count, and
// nothing else that would widen what crosses the pod boundary (AC-F6).
type approvalWaitResponse struct {
	Awaiting bool `json:"awaiting"`
	Pending  int  `json:"pending"`
}

func serveApprovalWait(w http.ResponseWriter, waits *approvalWaits) {
	awaiting, pending := waits.state()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(approvalWaitResponse{Awaiting: awaiting, Pending: pending})
}

// observeApprovalNotices makes the workload the tailer's sink for wait state.
// A claude-code workload has the method too and always answers "not waiting":
// it has no gate and no helper pod to tail, so nothing ever publishes to it.
func (c *claudeWorkload) observeApprovalNotices(notices []approvalNotice, dropped int) {
	c.approvals.observe(notices, dropped)
}
