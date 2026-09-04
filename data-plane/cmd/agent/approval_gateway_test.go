package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGateway is the decision-controlling stand-in docs/test/approval-gated-workload.md
// asks for in scenarios 3 and 4 ("결정을 제어할 수 있는 승인 게이트웨이 ... 테스트 대역").
// It records what was asked so the tests can assert on the approval context a
// human would have seen.
type fakeGateway struct {
	server *httptest.Server
	// statuses is handed out one per poll, and the last one repeats. A run of
	// "PENDING" is how a test keeps a request undecided.
	statuses []string
	polls    atomic.Int32
	created  atomic.Int32

	lastAPIKey   string
	lastBody     map[string]any
	createStatus int
	pollStatus   int
}

func newFakeGateway(t *testing.T, statuses ...string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{statuses: statuses, createStatus: http.StatusOK, pollStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+approvalRequestsPath, func(w http.ResponseWriter, r *http.Request) {
		g.created.Add(1)
		g.lastAPIKey = r.Header.Get(approvalAPIKeyHeader)
		_ = json.NewDecoder(r.Body).Decode(&g.lastBody)
		if g.createStatus != http.StatusOK {
			w.WriteHeader(g.createStatus)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"gw-req-1"}`))
	})
	mux.HandleFunc("GET "+approvalRequestsPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		n := int(g.polls.Add(1)) - 1
		if g.pollStatus != http.StatusOK {
			w.WriteHeader(g.pollStatus)
			return
		}
		status := "PENDING"
		if len(g.statuses) > 0 {
			if n >= len(g.statuses) {
				n = len(g.statuses) - 1
			}
			status = g.statuses[n]
		}
		_, _ = w.Write([]byte(`{"id":"gw-req-1","status":"` + status + `"}`))
	})
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) gate(t *testing.T) *approvalGateway {
	t.Helper()
	gate, err := newApprovalGateway(g.server.URL, "gateway-key", "user-1", "sess-abc")
	if err != nil {
		t.Fatalf("build gate: %v", err)
	}
	// No real waiting: the tests drive the poll loop, not the clock.
	gate.pollInterval = 0
	gate.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return gate
}

// newApprovalGateway's every-field-required rule, field by field.
func TestApprovalGatewayRefusesIncompleteConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, url, key, user, session string }{
		{"no url", "", "k", "u", "s"},
		{"no api key", "http://gw", "", "u", "s"},
		{"no user id", "http://gw", "k", "", "s"},
		{"no session id", "http://gw", "k", "u", ""},
		{"blank session id", "http://gw", "k", "u", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newApprovalGateway(tc.url, tc.key, tc.user, tc.session); err == nil {
				t.Fatal("incomplete gateway configuration accepted")
			}
		})
	}
}

func TestApprovalGatewayReportsEachDecision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		want     approvalDecision
	}{
		{"approved", []string{"APPROVED"}, approvalApproved},
		{"rejected", []string{"REJECTED"}, approvalRejected},
		{"expired", []string{"EXPIRED"}, approvalExpired},
		{"pending then approved", []string{"PENDING", "PENDING", "APPROVED"}, approvalApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newFakeGateway(t, tc.statuses...)
			gate := gw.gate(t)
			gate.sleep = func(context.Context, time.Duration) error { return nil }

			outcome, err := gate.await(context.Background(), "req-0001", `{"url":"https://example.test/"}`)
			if err != nil {
				t.Fatalf("await: %v", err)
			}
			if outcome.Decision != tc.want {
				t.Fatalf("decision = %q, want %q", outcome.Decision, tc.want)
			}
			if outcome.RequestID != "gw-req-1" {
				t.Errorf("request id = %q, want the gateway's own id", outcome.RequestID)
			}
			if got := gw.created.Load(); got != 1 {
				t.Errorf("created %d requests, want exactly 1 per tool call", got)
			}
		})
	}
}

// A gateway that never decides must not hold the queue slot forever.
func TestApprovalGatewayTimesOutAnUndecidedRequest(t *testing.T) {
	gw := newFakeGateway(t, "PENDING")
	gate := gw.gate(t)
	// A clock that jumps a minute per read runs the timeout out in a handful of
	// polls without any real waiting.
	clock := time.Now()
	gate.now = func() time.Time {
		clock = clock.Add(time.Minute)
		return clock
	}
	gate.sleep = func(context.Context, time.Duration) error { return nil }

	outcome, err := gate.await(context.Background(), "req-0002", "{}")
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if outcome.Decision != approvalTimeout {
		t.Fatalf("decision = %q, want %q", outcome.Decision, approvalTimeout)
	}
}

// Freeze and delete tear the pod down mid-wait (AC-F3). The wait has to end on
// cancellation rather than run to its own timeout.
func TestApprovalGatewayStopsWaitingWhenTheContextEnds(t *testing.T) {
	gw := newFakeGateway(t, "PENDING")
	gate := gw.gate(t)
	ctx, cancel := context.WithCancel(context.Background())
	gate.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if _, err := gate.await(ctx, "req-0003", "{}"); err == nil {
		t.Fatal("cancelled wait returned no error; the caller would read it as a decision")
	}
}

// approvalGateway.do's rule, on both calls.
func TestApprovalGatewayFailuresAreErrorsNotDecisions(t *testing.T) {
	t.Run("create fails", func(t *testing.T) {
		gw := newFakeGateway(t, "APPROVED")
		gw.createStatus = http.StatusUnauthorized
		if _, err := gw.gate(t).await(context.Background(), "req-0004", "{}"); err == nil {
			t.Fatal("gateway rejection of the create call was not reported")
		}
	})
	t.Run("poll fails", func(t *testing.T) {
		gw := newFakeGateway(t, "PENDING")
		gw.pollStatus = http.StatusInternalServerError
		if _, err := gw.gate(t).await(context.Background(), "req-0005", "{}"); err == nil {
			t.Fatal("gateway failure during polling was not reported")
		}
	})
}

// AC-F3's uniqueness clause and AC-F6's "no credential in the approval context",
// asserted on the bytes that actually left the container.
func TestApprovalGatewaySendsIdentityWithoutSecrets(t *testing.T) {
	gw := newFakeGateway(t, "APPROVED")
	gate := gw.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }

	approvalContext := `{"url":"https://rates.vendor.example/v1/latest","method":"GET"}`
	if _, err := gate.await(context.Background(), "req-4f2a", approvalContext); err != nil {
		t.Fatalf("await: %v", err)
	}

	if gw.lastAPIKey != "gateway-key" {
		t.Errorf("api key header = %q, want the configured key", gw.lastAPIKey)
	}
	if got := gw.lastBody["externalId"]; got != "sess-abc:req-4f2a" {
		t.Errorf("externalId = %v, want {sessionID}:{requestID} (AC-F3)", got)
	}
	if got := gw.lastBody["userId"]; got != "user-1" {
		t.Errorf("userId = %v, want the platform-wide notification target", got)
	}
	if got := gw.lastBody["context"]; got != approvalContext {
		t.Errorf("context = %v, want the caller's description verbatim", got)
	}
	// The key travels in a header and must not also appear anywhere in the body
	// a human reads (AC-F6).
	encoded, err := json.Marshal(gw.lastBody)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "gateway-key") {
		t.Errorf("approval request body carries the gateway key: %s", encoded)
	}
}

// Two calls in one session, and the same call in another session, must produce
// three different external identifiers (AC-F3).
func TestApprovalGatewayExternalIDsSeparateCallsAndSessions(t *testing.T) {
	one, err := newApprovalGateway("http://gw", "k", "u", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	two, err := newApprovalGateway("http://gw", "k", "u", "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range []string{
		one.externalID("req-a"), one.externalID("req-b"), two.externalID("req-a"),
	} {
		if seen[id] {
			t.Fatalf("external id %q repeated; the gateway would refuse the duplicate", id)
		}
		seen[id] = true
	}
}
