// The approval notice feed (AC-F3's "대기 표시"). The two halves of that
// requirement live in different pods: the external identifier is minted here, in
// the helper pod's MCP container, while the byte stream belongs to the workload
// pod's agent.
//
// This file is the helper-pod half: a sequenced, in-memory feed the gate
// publishes to and the workload pod tails. Why pull rather than push, and why
// memory only: docs/doc-tracker.md's AC-F3·F5 entry.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// noticesPath is the feed's endpoint on the same server as /mcp. It is
	// reachable only from this session's workload pod (AC-F2 ingress).
	noticesPath = "/notices"
	// noticeFeedCapacity bounds the ring. A session's approvals are a human
	// round trip apart, so this is a guard against an unbounded buffer rather
	// than a limit anyone reaches.
	noticeFeedCapacity = 1024
	// noticeFeedPollTimeout bounds one long poll. It ends an idle wait with an
	// empty answer so the tailer reconnects on a cursor it still owns, rather
	// than holding a connection open indefinitely.
	noticeFeedPollTimeout = 25 * time.Second
)

// The notice kinds. They are three because a wait ends in exactly three ways
// that a reader cares about: it was decided, or the gateway could not be asked
// at all, and before either of those it started.
const (
	noticeAwaiting    = "awaiting"
	noticeDecided     = "decided"
	noticeUnavailable = "unavailable"
)

// approvalNotice is one entry. It carries the tool and the external identifier
// AC-F3 puts in the marker and nothing else — no gateway URL, no API key, no
// approval context (AC-F6). The URL a human approved is already in the
// agent's own output; repeating it here would only widen what crosses the pod
// boundary.
type approvalNotice struct {
	Seq        int    `json:"seq"`
	Kind       string `json:"kind"`
	Tool       string `json:"tool"`
	ExternalID string `json:"externalId"`
	// Decision is set on a decided notice (approvalDecision).
	Decision string `json:"decision,omitempty"`
}

// noticeFeedResponse is one long poll's answer. Dropped is how many notices
// were evicted before the requested cursor, so a tailer that fell behind can
// say so in-band instead of leaving a silent hole in the byte stream.
type noticeFeedResponse struct {
	Notices []approvalNotice `json:"notices"`
	NextSeq int              `json:"nextSeq"`
	Dropped int              `json:"dropped,omitempty"`
}

// noticeFeed is the sequenced ring. Sequence numbers are monotonic for the life
// of the container and never reused, which is what makes `after` a safe cursor.
type noticeFeed struct {
	mu      sync.Mutex
	seq     int
	kept    []approvalNotice
	changed chan struct{}
}

func newNoticeFeed() *noticeFeed { return &noticeFeed{} }

// publish records one notice and wakes every waiting poll. A nil feed accepts
// publishes and drops them, so a server assembled without one (a test, or a
// container whose feed is not wired) still runs its gate unchanged.
func (f *noticeFeed) publish(n approvalNotice) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	n.Seq = f.seq
	f.kept = append(f.kept, n)
	if len(f.kept) > noticeFeedCapacity {
		f.kept = append([]approvalNotice(nil), f.kept[len(f.kept)-noticeFeedCapacity:]...)
	}
	if f.changed != nil {
		close(f.changed)
		f.changed = nil
	}
}

// since returns everything after the cursor, the cursor to ask with next, how
// many entries were evicted before the cursor, and the channel that closes on
// the next publish. The channel is handed back with the read so a caller cannot
// miss a publish that lands between the two.
func (f *noticeFeed) since(after int) (out []approvalNotice, next, dropped int, changed <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.changed == nil {
		f.changed = make(chan struct{})
	}
	changed = f.changed
	next = f.seq
	if len(f.kept) == 0 {
		return nil, next, 0, changed
	}
	if oldest := f.kept[0].Seq; after < oldest-1 {
		dropped = oldest - 1 - after
	}
	for _, n := range f.kept {
		if n.Seq > after {
			out = append(out, n)
		}
	}
	return out, next, dropped, changed
}

// serveApprovalNotices is the tail endpoint. It long-polls: an empty feed holds
// the request until something is published or the poll window closes, so the
// wait marker reaches the output stream as soon as the wait begins rather than
// at the next poll tick.
func serveApprovalNotices(w http.ResponseWriter, r *http.Request, feed *noticeFeed) {
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			http.Error(w, "after must be a non-negative integer", http.StatusBadRequest)
			return
		}
		after = parsed
	}

	window := time.NewTimer(noticeFeedPollTimeout)
	defer window.Stop()
	for {
		notices, next, dropped, changed := feed.since(after)
		if len(notices) > 0 || dropped > 0 {
			writeNoticeFeedResponse(w, noticeFeedResponse{Notices: notices, NextSeq: next, Dropped: dropped})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-window.C:
			writeNoticeFeedResponse(w, noticeFeedResponse{Notices: []approvalNotice{}, NextSeq: next})
			return
		}
	}
}

func writeNoticeFeedResponse(w http.ResponseWriter, body noticeFeedResponse) {
	if body.Notices == nil {
		body.Notices = []approvalNotice{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
