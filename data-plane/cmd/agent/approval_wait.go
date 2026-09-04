// The workload pod's record of what AC-F3 is currently waiting on, and the
// endpoint the control plane reads it through.
//
// AC-F3 carves an exception out of AC-B1's idle rule for this workload type:
// while an approval wait is in progress the platform refreshes the session's
// lastAccess so the idle count does not advance, because freezing mid-wait
// leaves no pod to run the call the human just approved. Acting on that needs
// one fact the control plane does not otherwise hold — whether this session is
// waiting right now.
//
// It is answered from here, and pulled rather than pushed, for the same reason
// the notice feed itself is pulled (session_mcp_notices.go): AC-F2's egress
// allowlist gives the workload pod kube-dns and its own helper pod and nothing
// else, so this pod cannot reach the control plane at all. The control plane
// can reach this pod — that direction carries no ingress policy and already
// carries every read and write — so the question travels back along the path
// the answers already take, and no NetworkPolicy changes.
package main

import (
	"encoding/json"
	"net/http"
	"sync"
)

// approvalWaitPath is where the control plane asks. It is on the workload
// agent's own server, not the helper pod's, because the helper is exactly what
// the control plane cannot reach.
const approvalWaitPath = "/approval"

// approvalWaits is the set of approval requests this session is blocked on,
// keyed by the external identifier AC-F3 mints. A set rather than a counter:
// a retried poll can deliver the same notice twice, and a counter would drift.
//
// It is derived state, persisted nowhere. That is correct rather than a
// shortcut — the truth is the external gateway's, the helper pod keeps no
// state across a freeze (AC-F4), and a restored session gets a new agent
// process and a new helper pod whose feed also starts empty.
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
// session's lastAccess forever and its pod with it. Clearing errs toward
// freezing a session that is in fact still waiting, which costs one restore on
// next access; the other direction costs a pod that is never reclaimed (V2).
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
		// An unknown kind is ignored, matching renderApprovalNotice: a newer
		// helper pod publishing something this agent does not understand must
		// not be able to strand the session's idle count.
	}
}

// state reports whether a human is being waited on, and on how many requests.
func (w *approvalWaits) state() (awaiting bool, pending int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) > 0, len(w.pending)
}

// approvalWaitResponse is the whole answer. It carries no tool name, no
// external identifier and nothing about the gateway (AC-F6): the control plane
// only needs to know whether to hold the idle count, and any value beyond that
// would widen what crosses the pod boundary for no gain.
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
