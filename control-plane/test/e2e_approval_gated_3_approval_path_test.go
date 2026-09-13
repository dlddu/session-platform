//go:build e2e

// 검증 시나리오: approval-gated-workload.md#시나리오 3
//
// AC-F3 (docs/prd/approval-gated-workload.md), asserted on the deployed SUT.
// What this file buys — and the two branches it deliberately leaves unbought,
// both held by the blocker `FETCH-ORIGIN` — is docs/test/e2e.md: the mapping row
// the line above names, and its §「남은 미검증 분기」.
package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// The marker texts are session_mcp_notice_tail.go's renderApprovalNotice,
	// written out rather than imported for the reason e2e_f1 gives.
	f3AwaitingPrefix = "[session-platform: awaiting approval — "
	f3ApprovedPrefix = "[session-platform: approval approved — "
	// The directive grammar is deploy/e2e-anthropic-fake.yaml's.
	f3Prompt         = "e2e-tool:" + providerToolName + `:{"url":"https://example.test/doc"}`
	f3MarkerToolName = "web_fetch_get"
	// data-plane/cmd/agent/session_mcp_tools.go requestIDPrefix, same reason.
	f3RequestIDPrefix = "req-"
	// 60s is the CLI's ceiling on one MCP tool call — it cancels there rather
	// than waiting longer, and that number lives only in its behaviour.
	f3MarkerBudget = 90 * time.Second
)

type f3Session struct {
	typedSession
	cs  kubernetes.Interface
	cfg *rest.Config
	ns  string
}

func newF3Session(t *testing.T) f3Session {
	t.Helper()
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; the gate is driven through the helper pod")
	}
	status, s := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "approval-gated",
	})
	if status != http.StatusCreated {
		t.Fatalf("create approval-gated session: status=%d", status)
	}
	t.Cleanup(func() {
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil); resp.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", s.ID, resp.StatusCode, raw)
		}
	})
	return f3Session{typedSession: s, cs: cs, cfg: cfg, ns: sessionNamespace()}
}

func (f f3Session) operator(t *testing.T, method, route, body string) string {
	t.Helper()
	helpers := helperPodsFor(t, f.cs, f.ns, f.ID)
	if len(helpers) != 1 {
		t.Fatalf("approval-gated session %s has %d helper pods, want exactly 1", f.ID, len(helpers))
	}
	script := `curl -sS --fail-with-body --max-time 20 -X "$1" ` +
		`-H 'Content-Type: application/json' --data "$3" "$APPROVAL_GATEWAY_URL$2"`
	return f4Sh(t, f.cs, f.cfg, f.ns, helpers[0].Name, sessionMCPContainer, script, method, route, body)
}

// Absent is "" rather than a failure: until the CLI reaches the tool there is
// legitimately no record, and that is not the same answer as "decided".
func (f f3Session) gatewayStatus(t *testing.T, externalID string) string {
	t.Helper()
	raw := f.operator(t, http.MethodGet, "/operator/requests", "")
	var records []struct {
		ExternalID string `json:"externalId"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		t.Fatalf("decode the gateway stand-in's request list: %v raw=%s", err, raw)
	}
	for _, r := range records {
		if r.ExternalID == externalID {
			return r.Status
		}
	}
	return ""
}

func f3Marker(t *testing.T, buf, prefix string) (tool, externalID string) {
	t.Helper()
	i := strings.Index(buf, prefix)
	if i < 0 {
		t.Fatalf("no %q marker in the session buffer: %q", prefix, buf)
	}
	rest := buf[i+len(prefix):]
	j := strings.Index(rest, "]")
	if j < 0 {
		t.Fatalf("marker %q is never closed in the session buffer: %q", prefix, buf)
	}
	tool, externalID, ok := strings.Cut(rest[:j], " · ")
	if !ok {
		t.Fatalf("marker body %q is not `<tool> · <external id>`", rest[:j])
	}
	return tool, externalID
}

func TestApprovalGatedApprovalPath_WriteReturnsBeforeTheGateAndTheWaitIsAnnounced(t *testing.T) {
	f := newF3Session(t)

	// One stand-in serves the whole cluster and defaultStatus is mutable state
	// on it, so this is what makes the wait below independent of what ran before.
	f.operator(t, http.MethodPost, "/operator/policy", `{"defaultStatus":"PENDING"}`)

	// Opened before the write, not after: the stream has to be attached before
	// the round trip starts or the bytes it misses are the ones compared below.
	stream := s6Open(t, f.ID, "?offset=0", "", f3MarkerBudget+30*time.Second)

	started := time.Now()
	writeShell(t, f.ID, f3Prompt)
	writeTook := time.Since(started)

	waiting := f3EventuallyMarker(t, f.ID, f3AwaitingPrefix)
	tool, externalID := f3Marker(t, waiting.Payload, f3AwaitingPrefix)
	if tool != f3MarkerToolName {
		t.Fatalf("the wait marker names tool %q, want %q (the name the session MCP "+
			"registers; the stand-in was instructed with %q, so this marker is where "+
			"the CLI's resolution of that namespaced name becomes observable)",
			tool, f3MarkerToolName, providerToolName)
	}

	sessionID, requestID, ok := strings.Cut(externalID, ":")
	if !ok {
		t.Fatalf("external id %q is not `{세션ID}:{요청ID}`", externalID)
	}
	if sessionID != f.ID {
		t.Fatalf("external id names session %q, want this session %q", sessionID, f.ID)
	}
	if !strings.HasPrefix(requestID, f3RequestIDPrefix) || len(requestID) == len(f3RequestIDPrefix) {
		t.Fatalf("request half %q of the external id, want %q followed by the minted id",
			requestID, f3RequestIDPrefix)
	}

	if decided := strings.Contains(waiting.Payload, f3ApprovedPrefix); decided {
		t.Fatalf("the buffer already carries an approval decision before anyone decided: %q", waiting.Payload)
	}
	if writeTook > 30*time.Second {
		t.Fatalf("write took %s — that is long enough to have waited on the gate", writeTook)
	}
	t.Logf("write returned in %s; the gate was still waiting at %s", writeTook, time.Since(started))

	if got := f.gatewayStatus(t, externalID); got != "PENDING" {
		t.Fatalf("the gateway holds request %s as %q, want PENDING before anyone decides", externalID, got)
	}

	streamed, bounds := f3DrainUntil(t, stream, f3AwaitingPrefix)
	if !strings.Contains(streamed, f3AwaitingPrefix) {
		t.Fatalf("the stream never carried the wait marker; it ended with %q", streamed)
	}
	atCursor := readShellAt(t, f.ID, 0)
	if !strings.HasPrefix(atCursor.Payload, streamed) {
		t.Fatalf("read at offset 0 and the stream from offset 0 disagree:\n read=%q\nstream=%q",
			atCursor.Payload, streamed)
	}

	if !utf8.ValidString(streamed) {
		t.Fatal("the streamed bytes are not valid UTF-8 — a cursor landed inside a rune")
	}
	if strings.IndexRune(streamed, '—') < 0 || strings.IndexRune(streamed, '·') < 0 {
		t.Fatalf("the streamed bytes carry no multi-byte rune, so the boundary assertion would be vacuous: %q", streamed)
	}
	for _, at := range bounds {
		if at == 0 || at == int64(len(atCursor.Payload)) {
			continue
		}
		if at > int64(len(atCursor.Payload)) {
			t.Fatalf("stream cursor %d is past the %d bytes read at the same moment", at, len(atCursor.Payload))
		}
		if !utf8.RuneStart(atCursor.Payload[at]) {
			t.Fatalf("stream cursor %d falls inside a rune of the session buffer", at)
		}
	}

	f.operator(t, http.MethodPost, "/operator/decide",
		`{"externalId":"`+externalID+`","status":"APPROVED"}`)

	approved := f3EventuallyMarker(t, f.ID, f3ApprovedPrefix)
	decidedTool, decidedID := f3Marker(t, approved.Payload, f3ApprovedPrefix)
	if decidedTool != tool || decidedID != externalID {
		t.Fatalf("the decision marker names %s · %s, want the waiting request %s · %s",
			decidedTool, decidedID, tool, externalID)
	}
	if !strings.HasPrefix(approved.Payload, waiting.Payload) {
		t.Fatalf("the buffer at the decision is not an extension of the buffer at the wait:\nwait=%q\nafter=%q",
			waiting.Payload, approved.Payload)
	}
	if i, j := strings.Index(approved.Payload, f3AwaitingPrefix), strings.Index(approved.Payload, f3ApprovedPrefix); i >= j {
		t.Fatalf("the decision marker is at %d and the wait marker at %d — the wait must come first", j, i)
	}
	if n := strings.Count(approved.Payload, f3ApprovedPrefix); n != 1 {
		t.Fatalf("the buffer carries %d approval decisions, want exactly 1", n)
	}
	if got := f.gatewayStatus(t, externalID); got != "APPROVED" {
		t.Fatalf("the gateway holds request %s as %q after the decision, want APPROVED", externalID, got)
	}
}

// Longer than the shared eventuallyShellRead budget: this one waits on a CLI
// start-up, an MCP round trip and a gateway poll rather than on shell output.
func f3EventuallyMarker(t *testing.T, id, prefix string) readResp {
	t.Helper()
	deadline := time.Now().Add(f3MarkerBudget)
	var r readResp
	for {
		r = readShellAt(t, id, 0)
		if strings.Contains(r.Payload, prefix) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never reached the session buffer within %s; buffer=%q", prefix, f3MarkerBudget, r.Payload)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Reading only as far as the marker keeps the stream from outliving the
// assertion it serves — the session goes on producing bytes after the tool result.
func f3DrainUntil(t *testing.T, s *s6Stream, prefix string) (string, []int64) {
	t.Helper()
	var buf strings.Builder
	bounds := []int64{}
	for {
		f, _ := s.nextEvent(t)
		if f.Event != "output" {
			t.Fatalf("unexpected %q frame while waiting for the wait marker (data=%s)", f.Event, f.Data)
		}
		d, payload := s6Output(t, f)
		buf.Write(payload)
		bounds = append(bounds, d.Offset, d.NextOffset)
		if strings.Contains(buf.String(), prefix) {
			return buf.String(), bounds
		}
	}
}
