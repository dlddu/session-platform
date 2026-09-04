package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// awaiting builds the notice the gate publishes when a wait starts.
func awaitingNotice(externalID string) approvalNotice {
	return approvalNotice{Kind: noticeAwaiting, Tool: webFetchGetTool, ExternalID: externalID}
}

func decidedNotice(externalID, decision string) approvalNotice {
	return approvalNotice{Kind: noticeDecided, Tool: webFetchGetTool, ExternalID: externalID, Decision: decision}
}

// The state machine AC-F3's idle exception is decided from: a wait is open
// from its "awaiting" until whatever ends it, and every way it can end has to
// close it. A wait that never closes holds a pod forever.
func TestApprovalWaitsTrackWhatTheFeedSays(t *testing.T) {
	for _, tc := range []struct {
		name    string
		notices []approvalNotice
		pending int
	}{
		{"a wait that started is open", []approvalNotice{awaitingNotice("s:1")}, 1},
		{"approval closes it", []approvalNotice{awaitingNotice("s:1"), decidedNotice("s:1", "APPROVED")}, 0},
		{"rejection closes it", []approvalNotice{awaitingNotice("s:1"), decidedNotice("s:1", "REJECTED")}, 0},
		{"expiry closes it", []approvalNotice{awaitingNotice("s:1"), decidedNotice("s:1", "EXPIRED")}, 0},
		{"timeout closes it", []approvalNotice{awaitingNotice("s:1"), decidedNotice("s:1", "TIMEOUT")}, 0},
		{
			"an unreachable gateway closes it too — nobody is being waited on",
			[]approvalNotice{awaitingNotice("s:1"), {Kind: noticeUnavailable, ExternalID: "s:1"}},
			0,
		},
		{
			"waits are counted per external identifier",
			[]approvalNotice{awaitingNotice("s:1"), awaitingNotice("s:2"), decidedNotice("s:1", "APPROVED")},
			1,
		},
		{
			"a redelivered notice does not double-count",
			[]approvalNotice{awaitingNotice("s:1"), awaitingNotice("s:1"), decidedNotice("s:1", "APPROVED")},
			0,
		},
		{
			"a kind this agent does not know cannot strand the count",
			[]approvalNotice{awaitingNotice("s:1"), {Kind: "some-newer-kind", ExternalID: "s:1"}},
			1,
		},
		{
			"a notice without an identifier is ignored rather than tracked under an empty key",
			[]approvalNotice{{Kind: noticeAwaiting}},
			0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w approvalWaits
			w.observe(tc.notices, 0)
			awaiting, pending := w.state()
			if pending != tc.pending {
				t.Fatalf("pending = %d, want %d", pending, tc.pending)
			}
			if awaiting != (tc.pending > 0) {
				t.Fatalf("awaiting = %v with %d pending", awaiting, pending)
			}
		})
	}
}

// Falling behind the helper's ring means a decision may have been evicted
// unseen. The set is cleared rather than kept, because a wait that can never
// be closed would hold this session's lastAccess — and its pod — forever.
func TestApprovalWaitsClearWhenNoticesWereDropped(t *testing.T) {
	var w approvalWaits
	w.observe([]approvalNotice{awaitingNotice("s:1"), awaitingNotice("s:2")}, 0)
	if _, pending := w.state(); pending != 2 {
		t.Fatalf("pending = %d before the drop, want 2", pending)
	}

	w.observe(nil, 3)
	if awaiting, pending := w.state(); awaiting || pending != 0 {
		t.Fatalf("awaiting = %v pending = %d after a drop, want false/0", awaiting, pending)
	}

	// A drop reported alongside notices still records the notices that did
	// arrive: only what came before the cursor is unaccounted for.
	w.observe([]approvalNotice{awaitingNotice("s:9")}, 2)
	if awaiting, pending := w.state(); !awaiting || pending != 1 {
		t.Fatalf("awaiting = %v pending = %d, want true/1", awaiting, pending)
	}
}

// noticeFeedServer serves one feed over HTTP, the way the helper pod's MCP
// container does, so a test can drive the tailer from the publisher side.
func noticeFeedServer(t *testing.T) (*noticeFeed, string) {
	t.Helper()
	feed := newNoticeFeed()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+noticesPath, func(w http.ResponseWriter, r *http.Request) {
		serveApprovalNotices(w, r, feed)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return feed, srv.URL
}

func readApprovalWait(t *testing.T, base string) approvalWaitResponse {
	t.Helper()
	resp, err := http.Get(base + approvalWaitPath)
	if err != nil {
		t.Fatalf("GET %s: %v", approvalWaitPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", approvalWaitPath, resp.StatusCode)
	}
	var out approvalWaitResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode approval wait: %v", err)
	}
	return out
}

func awaitApprovalWait(t *testing.T, base string, want bool) approvalWaitResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got approvalWaitResponse
	for time.Now().Before(deadline) {
		if got = readApprovalWait(t, base); got.Awaiting == want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("approval wait stayed awaiting=%v, want %v", got.Awaiting, want)
	return got
}

// The whole workload-pod chain AC-F3's idle exception rests on: the helper pod
// publishes, the tailer follows, and the control plane — which can reach this
// pod and not the helper — reads the answer off the workload agent.
func TestApprovalWaitTravelsFromTheFeedToTheControlPlane(t *testing.T) {
	feed, feedURL := noticeFeedServer(t)
	c, srv := newClaudeTestServer(t, &fakeClaudeRunner{}, "", false, nil)

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	tailer := newNoticeTailer(feedURL, c, testLogger())
	tailer.retry = 10 * time.Millisecond
	go tailer.run(ctx)

	if got := readApprovalWait(t, srv.URL); got.Awaiting {
		t.Fatalf("a session with no approvals reported awaiting=%v", got.Awaiting)
	}

	feed.publish(awaitingNotice("sess-abc:req-1"))
	if got := awaitApprovalWait(t, srv.URL, true); got.Pending != 1 {
		t.Fatalf("pending = %d while one approval is open, want 1", got.Pending)
	}

	feed.publish(decidedNotice("sess-abc:req-1", "APPROVED"))
	if got := awaitApprovalWait(t, srv.URL, false); got.Pending != 0 {
		t.Fatalf("pending = %d after the decision, want 0", got.Pending)
	}
}

// AC-F6: the answer crosses a pod boundary, so it carries only what the idle
// decision needs. The tool, the external identifier and anything about the
// gateway stay on the side that already has them.
func TestApprovalWaitAnswerCarriesNothingElse(t *testing.T) {
	feed, feedURL := noticeFeedServer(t)
	c, srv := newClaudeTestServer(t, &fakeClaudeRunner{}, "", false, nil)

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	tailer := newNoticeTailer(feedURL, c, testLogger())
	tailer.retry = 10 * time.Millisecond
	go tailer.run(ctx)

	feed.publish(awaitingNotice("sess-abc:req-secret"))
	awaitApprovalWait(t, srv.URL, true)

	resp, err := http.Get(srv.URL + approvalWaitPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for key := range body {
		if key != "awaiting" && key != "pending" {
			t.Errorf("the approval answer carries %q, which the idle decision does not need", key)
		}
	}
	raw, _ := json.Marshal(body)
	for _, leaked := range []string{"req-secret", webFetchGetTool, "sess-abc"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("the approval answer leaked %q across the pod boundary: %s", leaked, raw)
		}
	}
}

// The same chain driven by the real gate rather than a hand-published feed: a
// call that goes all the way through leaves no wait behind, so an idle session
// whose approvals are all decided freezes on the ordinary schedule.
func TestApprovalWaitClosesWhenTheRealGateDecides(t *testing.T) {
	g := newGatedMCP(t, "APPROVED")
	c, srv := newClaudeTestServer(t, &fakeClaudeRunner{}, "", false, nil)

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	tailer := newNoticeTailer(g.server.URL, c, testLogger())
	tailer.retry = 10 * time.Millisecond
	go tailer.run(ctx)

	g.call(t, "https://rates.vendor.example/v1/latest")
	if got := awaitApprovalWait(t, srv.URL, false); got.Pending != 0 {
		t.Fatalf("pending = %d after an approved call, want 0", got.Pending)
	}
}
