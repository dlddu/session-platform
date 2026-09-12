//go:build e2e

// 검증 시나리오: approval-gated-workload.md#시나리오 3
//
// AC-F3 (docs/prd/approval-gated-workload.md), asserted on the deployed SUT.
//
// 이 파일이 사는 것: write 가 승인을 기다리지 않고 반환한다 · 대기 표시가 세션 출력에
// in-band 로 투영되고 SSE 와 **같은 커서의** read 가 같은 바이트를 본다 · 승인 요청의 외부
// 식별자가 `{세션ID}:{요청ID}` 다 · 승인 뒤 결정 마커가 같은 바이트열에 이어 붙는다 · 그
// 마커를 실어 나르는 커서 계약(event id = `nextOffset`, decoded 길이 = 커서 이동량, 커서가
// UTF-8 code-point 경계).
//
// What this file deliberately does NOT assert, and why:
//
//   - 승인 후 **실제 아웃바운드 GET 이 정확히 1회** 일어나고 그 응답이 같은 바이트열에 이어
//     붙는 것. 시나리오의 사전 조건이 요구하는 「호출을 관찰할 수 있는 외부 테스트 upstream」
//     이 이 SUT 에 없다 — 도구는 https 만 받는데(`validateFetchTarget`) 그 호출을 내는 MCP
//     컨테이너의 fetch 는 시스템 루트만 신뢰하는 기본 클라이언트라(`http.DefaultClient`)
//     인클러스터 origin 의 사설 인증서를 검증할 수 없고, 그 컨테이너엔 CA 를 넣을 자리도
//     없다. docs/test/e2e.md 의 차단 요인 `FETCH-ORIGIN` 이 그 판정을 갖는다.
//   - 같은 이유로 「결정 전 테스트 upstream 무도달」의 **음성** 단언도 없다. 닿을 수 있는
//     upstream 이 애초에 없으므로 그 단언은 결정 전이든 후든 공허하다. 「아직 실행되지
//     않았다」를 이 SUT 에서 공허하지 않게 사는 형태는 따로 있고 그것이 아래 두 관측이다 —
//     게이트웨이 대역이 그 요청을 **PENDING 으로 들고 있는 것**(대역에 물어서 확인한다)과
//     출력에 결정 마커가 **아직 없는 것**. 둘 다 우리가 결정을 내리는 순간 바뀌므로 공허하지
//     않다.
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
	// The marker texts are data-plane/cmd/agent/session_mcp_notice_tail.go's
	// renderApprovalNotice, written out rather than imported for the reason
	// e2e_f1 gives. Only the prefix is a constant: the tool and the external
	// identifier that follow are what the assertions below read out of it.
	f3AwaitingPrefix = "[session-platform: awaiting approval — "
	f3ApprovedPrefix = "[session-platform: approval approved — "
	// The provider stand-in turns this directive into a tool_use block
	// (deploy/e2e-anthropic-fake.yaml); providerToolName is how the CLI
	// namespaces the session MCP's one tool. The URL is never fetched here —
	// see the FETCH-ORIGIN note in the header — but it still has to be one the
	// gate will accept, so it is https.
	f3Prompt = "e2e-tool:" + providerToolName + `:{"url":"https://example.test/doc"}`
	// The marker names the tool the *session MCP* registers, not the namespaced
	// name the CLI hands the model: renderApprovalNotice prints the gate's own
	// webFetchGetTool (data-plane/cmd/agent/session_mcp_tools.go). The two names
	// differ on purpose, and that is what makes this assertion worth making —
	// the stand-in is instructed with providerToolName, so a marker carrying the
	// bare name is proof the CLI *resolved* the namespaced name onto this
	// server's registered tool. Asserting providerToolName here instead would
	// assert the CLI's own spelling back at itself; asserting this one buys the
	// cross-check the sibling ledger's CLAUDE-PROVIDER entry says is missing
	// ("대역이 내는 도구 이름이 세션 MCP 가 실제로 등재한 것과 같은지").
	f3MarkerToolName = "web_fetch_get"
	// data-plane/cmd/agent/session_mcp_tools.go requestIDPrefix, same reason.
	f3RequestIDPrefix = "req-"
	// The CLI gives an MCP tool call 60s before it cancels it, so the decision
	// has to land inside that window; everything this file waits for is sized
	// well under it.
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

// The stand-in's operator surface stands in for the human at the real approval
// UI. It is a ClusterIP Service, so it is driven from the one container that is
// meant to reach the gateway at all — the helper pod's session MCP, the
// gateway's own client — rather than from the test process. The container
// already holds the address as APPROVAL_GATEWAY_URL, so the route is the only
// thing this has to know.
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

// f3GatewayStatus is what the stand-in says it is holding for this external
// identifier. Absent is reported as "" rather than as a failure: before the CLI
// has reached the tool there is legitimately no record yet, and a caller that
// polls needs to tell "not yet" from "decided".
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

// f3Marker reads one rendered notice out of the session buffer and returns its
// two variable halves. Parsing rather than matching a whole literal is what
// lets the assertions below say *which* tool and *which* request the marker
// named, which is the half of AC-F3 the text alone does not carry.
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

// The scenario names the approval path as one round trip, so it is bought as
// one: a second session would double the SUT cost and buy nothing, since every
// clause below is about the ordering *within* a single wait.
func TestApprovalGatedApprovalPath_WriteReturnsBeforeTheGateAndTheWaitIsAnnounced(t *testing.T) {
	f := newF3Session(t)

	// The stand-in decides nothing on its own — that is what makes the wait
	// below real. PENDING is already its default; setting it makes this test
	// independent of whatever ran before it in the same cluster.
	f.operator(t, http.MethodPost, "/operator/policy", `{"defaultStatus":"PENDING"}`)

	// Opened before the write so the stream carries the whole round trip from
	// offset 0, which is what lets the SSE and read halves be compared at the
	// same cursor further down.
	stream := s6Open(t, f.ID, "?offset=0", "", f3MarkerBudget+30*time.Second)

	started := time.Now()
	writeShell(t, f.ID, f3Prompt)
	writeTook := time.Since(started)

	// ---------------------------------------------------------------- 대기 표시
	waiting := f3EventuallyMarker(t, f.ID, f3AwaitingPrefix)
	tool, externalID := f3Marker(t, waiting.Payload, f3AwaitingPrefix)
	if tool != f3MarkerToolName {
		t.Fatalf("the wait marker names tool %q, want %q (the name the session MCP "+
			"registers; the stand-in was instructed with %q, so this marker is where "+
			"the CLI's resolution of that namespaced name becomes observable)",
			tool, f3MarkerToolName, providerToolName)
	}

	// ------------------------------------------------------- 외부 식별자의 모양
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

	// ------------------------------------------------- write did not wait here
	//
	// The marker is published when the gate *enters* the wait and nothing can
	// leave that wait until the decision below, which has not happened yet — so
	// observing it now proves the invocation was still in flight after write
	// had already returned. That ordering, not the duration, is the assertion;
	// the duration only says how far from the wall it landed, and it is checked
	// against a ceiling well under the CLI's own 60s tool budget because a
	// write that really did block on the gate could not come in under it.
	if decided := strings.Contains(waiting.Payload, f3ApprovedPrefix); decided {
		t.Fatalf("the buffer already carries an approval decision before anyone decided: %q", waiting.Payload)
	}
	if writeTook > 30*time.Second {
		t.Fatalf("write took %s — that is long enough to have waited on the gate", writeTook)
	}
	t.Logf("write returned in %s; the gate was still waiting at %s", writeTook, time.Since(started))

	// ------------------------------------------------- the wait is not a story
	//
	// The marker says a wait began; this says the gateway is the one holding
	// it. Without this the marker could be printed into the void and every
	// assertion above would still pass.
	if got := f.gatewayStatus(t, externalID); got != "PENDING" {
		t.Fatalf("the gateway holds request %s as %q, want PENDING before anyone decides", externalID, got)
	}

	// ------------------------------------ SSE 와 같은 커서의 read 가 같은 바이트
	//
	// s6Output carries the per-event half of the cursor contract (event id =
	// nextOffset, decoded length = cursor movement); what is added here is that
	// the stream and the read surface agree on the bytes *and* that the wait
	// marker is among them.
	streamed, bounds := f3DrainUntil(t, stream, f3AwaitingPrefix)
	if !strings.Contains(streamed, f3AwaitingPrefix) {
		t.Fatalf("the stream never carried the wait marker; it ended with %q", streamed)
	}
	atCursor := readShellAt(t, f.ID, 0)
	if !strings.HasPrefix(atCursor.Payload, streamed) {
		t.Fatalf("read at offset 0 and the stream from offset 0 disagree:\n read=%q\nstream=%q",
			atCursor.Payload, streamed)
	}

	// ------------------------------------------- 커서는 UTF-8 code-point 경계다
	//
	// Non-vacuous because of the marker itself: renderApprovalNotice spells it
	// with an em dash and a middle dot, so the bytes the cursors move across
	// genuinely contain multi-byte runes for a naive chunker to split.
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

	// --------------------------------------------------- 승인 후에야 결정이 온다
	f.operator(t, http.MethodPost, "/operator/decide",
		`{"externalId":"`+externalID+`","status":"APPROVED"}`)

	approved := f3EventuallyMarker(t, f.ID, f3ApprovedPrefix)
	decidedTool, decidedID := f3Marker(t, approved.Payload, f3ApprovedPrefix)
	if decidedTool != tool || decidedID != externalID {
		t.Fatalf("the decision marker names %s · %s, want the waiting request %s · %s",
			decidedTool, decidedID, tool, externalID)
	}
	// 같은 바이트열에 이어 붙는다: the decision did not open a second buffer, and
	// it did not rewrite the wait that preceded it.
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

// f3DrainUntil accumulates output events until the marker has arrived, and
// returns the bytes together with every cursor the stream stopped at. Reading
// only as far as the marker keeps the stream from outliving the assertion it
// serves — the session goes on producing bytes after the tool result.
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
