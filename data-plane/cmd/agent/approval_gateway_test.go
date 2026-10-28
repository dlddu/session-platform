package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	mu           sync.Mutex
	authHeaders  []string
	pollPaths    []string
	lastBody     map[string]any
	createStatus int
	createState  string
	pollStatus   int
}

func newFakeGateway(t *testing.T, statuses ...string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{
		statuses:     statuses,
		createStatus: http.StatusCreated,
		createState:  "PENDING",
		pollStatus:   http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+approvalRequestsPath, func(w http.ResponseWriter, r *http.Request) {
		g.created.Add(1)
		g.mu.Lock()
		g.authHeaders = append(g.authHeaders, r.Header.Get("Authorization"))
		_ = json.NewDecoder(r.Body).Decode(&g.lastBody)
		g.mu.Unlock()
		if g.createStatus != http.StatusCreated {
			w.WriteHeader(g.createStatus)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"gw/req 1","status":"` + g.createState + `"}`))
	})
	mux.HandleFunc("GET "+approvalRequestsPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		n := int(g.polls.Add(1)) - 1
		g.mu.Lock()
		g.authHeaders = append(g.authHeaders, r.Header.Get("Authorization"))
		g.pollPaths = append(g.pollPaths, r.URL.EscapedPath())
		g.mu.Unlock()
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
		_, _ = w.Write([]byte(`{"id":"gw/req 1","status":"` + status + `"}`))
	})
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func writeToken(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (g *fakeGateway) gate(t *testing.T) *approvalGateway {
	t.Helper()
	gate, err := newApprovalGateway(g.server.URL, writeToken(t, "sa-token-1"), "sess-abc")
	if err != nil {
		t.Fatalf("build gate: %v", err)
	}
	// No real waiting: the tests drive the poll loop, not the clock.
	gate.pollInterval = 0
	gate.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return gate
}

func TestApprovalGatewayRefusesIncompleteConfiguration(t *testing.T) {
	token := writeToken(t, "sa-token")
	empty := writeToken(t, "   ")
	for _, tc := range []struct{ name, url, tokenFile, session string }{
		{"no url", "", token, "s"},
		{"no session id", "http://gw", token, ""},
		{"blank session id", "http://gw", token, "   "},
		{"token file missing", "http://gw", filepath.Join(t.TempDir(), "absent"), "s"},
		{"token file empty", "http://gw", empty, "s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newApprovalGateway(tc.url, tc.tokenFile, tc.session); err == nil {
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
		{"unknown interim status keeps waiting", []string{"QUEUED", "APPROVED"}, approvalApproved},
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
			if outcome.RequestID != "gw/req 1" {
				t.Errorf("request id = %q, want the gateway's own id", outcome.RequestID)
			}
			if got := gw.created.Load(); got != 1 {
				t.Errorf("created %d requests, want exactly 1 per tool call", got)
			}
		})
	}
}

// The approver's auto-response mode decides at creation and the request never
// changes after, so polling it would only burn the timeout on an APPROVED that
// is already known.
func TestApprovalGatewayAcceptsADecisionMadeAtCreation(t *testing.T) {
	for _, status := range []string{"APPROVED", "REJECTED"} {
		t.Run(status, func(t *testing.T) {
			gw := newFakeGateway(t, "PENDING")
			gw.createState = status
			outcome, err := gw.gate(t).await(context.Background(), "req-auto", "{}")
			if err != nil {
				t.Fatalf("await: %v", err)
			}
			if string(outcome.Decision) != status {
				t.Fatalf("decision = %q, want %q", outcome.Decision, status)
			}
			if got := gw.polls.Load(); got != 0 {
				t.Errorf("polled %d times after a decision at creation", got)
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

func TestApprovalGatewayFailuresAreErrorsNotDecisions(t *testing.T) {
	t.Run("caller not registered", func(t *testing.T) {
		gw := newFakeGateway(t, "APPROVED")
		gw.createStatus = http.StatusUnauthorized
		_, err := gw.gate(t).await(context.Background(), "req-0004", "{}")
		if err == nil {
			t.Fatal("gateway rejection of the create call was not reported")
		}
		if !strings.Contains(err.Error(), "registered as a caller") {
			t.Errorf("401 error %q does not point at the caller registration", err)
		}
	})
	t.Run("external id collision", func(t *testing.T) {
		gw := newFakeGateway(t, "APPROVED")
		gw.createStatus = http.StatusConflict
		if _, err := gw.gate(t).await(context.Background(), "req-0006", "{}"); err == nil {
			t.Fatal("409 on create was not reported")
		}
	})
	t.Run("poll fails", func(t *testing.T) {
		gw := newFakeGateway(t, "PENDING")
		gw.pollStatus = http.StatusInternalServerError
		if _, err := gw.gate(t).await(context.Background(), "req-0005", "{}"); err == nil {
			t.Fatal("gateway failure during polling was not reported")
		}
	})
	t.Run("token disappears after start", func(t *testing.T) {
		gw := newFakeGateway(t, "APPROVED")
		gate := gw.gate(t)
		if err := os.Remove(gate.tokenFile); err != nil {
			t.Fatal(err)
		}
		if _, err := gate.await(context.Background(), "req-0007", "{}"); err == nil {
			t.Fatal("unreadable token did not fail the call")
		}
		if got := gw.created.Load(); got != 0 {
			t.Errorf("sent %d unauthenticated requests", got)
		}
	})
}

// AC-F3's uniqueness clause and AC-F6's "no credential in the approval context",
// asserted on the bytes that actually left the container.
func TestApprovalGatewaySendsIdentityWithoutSecrets(t *testing.T) {
	gw := newFakeGateway(t, "PENDING", "APPROVED")
	gate := gw.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }

	approvalContext := `{"url":"https://rates.vendor.example/v1/latest","method":"GET"}`
	if _, err := gate.await(context.Background(), "req-4f2a", approvalContext); err != nil {
		t.Fatalf("await: %v", err)
	}

	for i, h := range gw.authHeaders {
		if h != "Bearer sa-token-1" {
			t.Errorf("request %d authorization = %q, want the service account bearer", i, h)
		}
	}
	if got := gw.lastBody["externalId"]; got != "sess-abc:req-4f2a" {
		t.Errorf("externalId = %v, want {sessionID}:{requestID} (AC-F3)", got)
	}
	if got := gw.lastBody["context"]; got != approvalContext {
		t.Errorf("context = %v, want the caller's description verbatim", got)
	}
	if got := gw.lastBody["requesterName"]; got != approvalRequesterName {
		t.Errorf("requesterName = %v, want %q", got, approvalRequesterName)
	}
	if got := gw.lastBody["timeoutSeconds"]; got != float64(defaultApprovalTimeout/time.Second) {
		t.Errorf("timeoutSeconds = %v, want the client's own wait", got)
	}
	// PocketAide rejects unknown fields with 400, so the body is exactly these.
	if len(gw.lastBody) != 4 {
		t.Errorf("body has %d fields, want 4: %v", len(gw.lastBody), gw.lastBody)
	}
	encoded, err := json.Marshal(gw.lastBody)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sa-token-1") {
		t.Errorf("approval request body carries the service account token: %s", encoded)
	}
	if len(gw.pollPaths) == 0 || gw.pollPaths[0] != approvalRequestsPath+"/gw%2Freq%201" {
		t.Errorf("poll paths = %v, want the gateway id path-escaped", gw.pollPaths)
	}
}

// The kubelet rewrites the projected token in place; a request after rotation
// must carry the new one.
func TestApprovalGatewayRereadsTheTokenPerRequest(t *testing.T) {
	gw := newFakeGateway(t, "PENDING", "APPROVED")
	gate := gw.gate(t)
	gate.sleep = func(context.Context, time.Duration) error {
		return os.WriteFile(gate.tokenFile, []byte("sa-token-2"), 0o600)
	}
	if _, err := gate.await(context.Background(), "req-rot", "{}"); err != nil {
		t.Fatalf("await: %v", err)
	}
	if last := gw.authHeaders[len(gw.authHeaders)-1]; last != "Bearer sa-token-2" {
		t.Errorf("authorization after rotation = %q, want the rotated token", last)
	}
}

// Two calls in one session, and the same call in another session, must produce
// three different external identifiers (AC-F3).
func TestApprovalGatewayExternalIDsSeparateCallsAndSessions(t *testing.T) {
	token := writeToken(t, "t")
	one, err := newApprovalGateway("http://gw", token, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	two, err := newApprovalGateway("http://gw", token, "sess-2")
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
