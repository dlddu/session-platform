package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fetchNotices reads one poll off a session MCP's feed. It is what the workload
// pod's tailer does, spelled out so a test can assert on the wire shape.
func fetchNotices(t *testing.T, baseURL string, after int) noticeFeedResponse {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s%s?after=%d", baseURL, noticesPath, after))
	if err != nil {
		t.Fatalf("poll notices: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("notices = %d, want 200", resp.StatusCode)
	}
	var body noticeFeedResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode notices: %v", err)
	}
	return body
}

// AC-F3's 대기 표시, at its source: the gate announces the wait *before* it
// blocks and the decision after it settles, so a reader learns that the session
// is waiting on a human while it still is.
func TestSessionMCPPublishesTheWaitAndTheDecision(t *testing.T) {
	for _, decision := range []string{"APPROVED", "REJECTED", "EXPIRED"} {
		t.Run(decision, func(t *testing.T) {
			g := newGatedMCP(t, decision)
			g.call(t, "https://rates.vendor.example/v1/latest")

			body := fetchNotices(t, g.server.URL, 0)
			if len(body.Notices) != 2 {
				t.Fatalf("notices = %v, want the wait and its decision", body.Notices)
			}
			wait, decided := body.Notices[0], body.Notices[1]
			if wait.Kind != noticeAwaiting || decided.Kind != noticeDecided {
				t.Fatalf("kinds = %q,%q; want the wait first and the decision second",
					wait.Kind, decided.Kind)
			}
			if wait.Seq >= decided.Seq {
				t.Errorf("seq %d,%d: the wait must be sequenced before its decision",
					wait.Seq, decided.Seq)
			}
			if decided.Decision != decision {
				t.Errorf("decision = %q, want %q", decided.Decision, decision)
			}
			for _, n := range body.Notices {
				if n.Tool != webFetchGetTool {
					t.Errorf("tool = %q, want %q", n.Tool, webFetchGetTool)
				}
				// AC-F3's uniqueness rule, as the marker will render it.
				if !strings.HasPrefix(n.ExternalID, "sess-abc:"+requestIDPrefix) {
					t.Errorf("externalId = %q, want {sessionID}:{requestID}", n.ExternalID)
				}
			}
			if body.NextSeq != decided.Seq {
				t.Errorf("nextSeq = %d, want %d", body.NextSeq, decided.Seq)
			}
		})
	}
}

// A gateway that cannot be reached is not a decision, and the feed says so with
// its own kind — the agent got a tool failure, not a refusal (AC-F3).
func TestSessionMCPPublishesGatewayOutageAsItsOwnKind(t *testing.T) {
	g := newGatedMCP(t)
	g.gateway.createStatus = http.StatusInternalServerError
	g.call(t, "https://rates.vendor.example/v1/latest")

	body := fetchNotices(t, g.server.URL, 0)
	if len(body.Notices) != 2 {
		t.Fatalf("notices = %v, want the wait and the outage", body.Notices)
	}
	if got := body.Notices[1].Kind; got != noticeUnavailable {
		t.Fatalf("kind = %q, want %q", got, noticeUnavailable)
	}
	if got := body.Notices[1].Decision; got != "" {
		t.Errorf("decision = %q; an outage is not a decision", got)
	}
}

// The cursor is what makes the feed tailable: reconnecting with the last seq
// returns only what came after it, and an idle feed ends the poll empty instead
// of replaying.
func TestSessionMCPNoticeFeedIsTailable(t *testing.T) {
	g := newGatedMCP(t, "APPROVED")
	g.call(t, "https://rates.vendor.example/v1/latest")

	first := fetchNotices(t, g.server.URL, 0)
	if len(first.Notices) == 0 {
		t.Fatal("first poll returned nothing")
	}
	g.call(t, "https://other.vendor.example/v1/latest")

	second := fetchNotices(t, g.server.URL, first.NextSeq)
	if len(second.Notices) != 2 {
		t.Fatalf("second poll = %v, want only the second call's two notices", second.Notices)
	}
	for _, n := range second.Notices {
		if n.Seq <= first.NextSeq {
			t.Errorf("seq %d was already delivered at cursor %d", n.Seq, first.NextSeq)
		}
	}
	if second.Dropped != 0 {
		t.Errorf("dropped = %d, want 0 for a cursor that never fell behind", second.Dropped)
	}
}

// An idle feed must not hold a poll open forever, and must not answer a cursor
// it has already passed with a replay.
func TestSessionMCPNoticeFeedEndsAnIdlePoll(t *testing.T) {
	feed := newNoticeFeed()
	feed.publish(approvalNotice{Kind: noticeAwaiting, Tool: webFetchGetTool, ExternalID: "sess:req-1"})

	notices, next, dropped, changed := feed.since(1)
	if len(notices) != 0 || dropped != 0 {
		t.Fatalf("since(1) = %v (dropped %d), want nothing new", notices, dropped)
	}
	if next != 1 {
		t.Fatalf("nextSeq = %d, want 1", next)
	}
	select {
	case <-changed:
		t.Fatal("the change channel closed before anything was published")
	default:
	}
	go feed.publish(approvalNotice{Kind: noticeDecided, Decision: "APPROVED"})
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("a publish did not wake the waiting poll")
	}
}

// A tailer that fell behind the ring is told how many it missed. A silent hole
// in the output byte stream would be worse than an admitted one.
func TestSessionMCPNoticeFeedReportsWhatItEvicted(t *testing.T) {
	feed := newNoticeFeed()
	for i := 0; i < noticeFeedCapacity+5; i++ {
		feed.publish(approvalNotice{Kind: noticeAwaiting, Tool: webFetchGetTool})
	}
	notices, next, dropped, _ := feed.since(0)
	if len(notices) != noticeFeedCapacity {
		t.Fatalf("kept %d notices, want the ring capacity %d", len(notices), noticeFeedCapacity)
	}
	if dropped != 5 {
		t.Fatalf("dropped = %d, want 5", dropped)
	}
	if next != noticeFeedCapacity+5 {
		t.Fatalf("nextSeq = %d, want %d", next, noticeFeedCapacity+5)
	}
}

// AC-F6: the notice crosses the pod boundary, so nothing secret may ride on it.
// The gateway key and URL live in this container and stay here.
func TestSessionMCPNoticesCarryNoSecrets(t *testing.T) {
	g := newGatedMCP(t, "APPROVED")
	g.call(t, "https://rates.vendor.example/v1/latest")

	resp, err := http.Get(g.server.URL + noticesPath + "?after=0")
	if err != nil {
		t.Fatalf("poll notices: %v", err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 64<<10)
	n, _ := resp.Body.Read(raw)
	body := string(raw[:n])
	for _, secret := range []string{"gateway-key", g.gateway.server.URL, "user-1"} {
		if strings.Contains(body, secret) {
			t.Errorf("notice feed leaked %q", secret)
		}
	}
}

// The feed is advisory. A gate assembled without one still refuses an
// unapproved call and still keeps the network untouched: losing the marker must
// never mean losing the gate.
func TestSessionMCPGateRunsWithoutANoticeFeed(t *testing.T) {
	gate := newFakeGateway(t, "REJECTED").gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }
	reached := 0
	config := sessionMCPConfig{
		gateway: gate,
		fetch: func(context.Context, string) (*http.Response, error) {
			reached++
			return nil, errors.New("the network should not have been reached")
		},
		notices: nil,
	}
	params := json.RawMessage(
		`{"name":"` + webFetchGetTool + `","arguments":{"url":"https://rates.vendor.example/v1/latest"}}`)
	result, rpcErr := config.callTool(context.Background(), testLogger(), params)
	if rpcErr != nil {
		t.Fatalf("callTool returned a JSON-RPC error: %v", rpcErr)
	}
	if isError, _ := result.(map[string]any)["isError"].(bool); !isError {
		t.Fatalf("result = %v, want a tool failure", result)
	}
	if reached != 0 {
		t.Fatalf("upstream reached %d times without an approval", reached)
	}
}
