//go:build e2e

// 검증 시나리오: state-api.md#시나리오 5
//
// docs/prd/state-api.md AC-C2·AC-C3·AC-C4 의 wire validation 절, asserted on the
// deployed SUT.
//
// 이 파일이 사는 것은 **요청 본문이 세션에 닿기 전에 무엇이 걸러지는가**다. 갈래별로 무엇을
// 단언하는지는 docs/test/e2e.md 의 매핑 행에 있고, 여기 적어 둘 것은 그 행이 담지 못하는
// **경계**다.
//
// immutable metadata 를 read/write/switch 로 바꾸려는 시도가 400 이라는 것은
// approval-gated 타입 축의 일부로 e2e_f1_workload_type_test.go 가 이미 산다. 여기서
// 다시 사는 것은 그 **기전**이다 — 그 400 은 타입별 특수 검사가 아니라 세 route 가
// 공통으로 거는 `DisallowUnknownFields` 이고(control-plane/internal/api 의
// decodeRequestBody), 그래서 기본 타입 세션에서도 unknown field 와 **같은 표에서** 같은
// 이유로 떨어진다. 그리고 f1 이 사지 않는 절반, **agent side effect 부재**를 여기서 산다.
// 마찬가지로 "snapshot 세션에 write 하면 복원된다"는 state-api.md#시나리오 3 의 성질이라
// (e2e_c3_write_branches_test.go) 다시 사지 않고 인용만 한다 — (e) 가 사는 것은 그 복원이
// **일어나지 않았다**는 쪽이다.
//
// 범위 밖: "서버 body read 는 30초로 제한됨"(시나리오 5 기대 결과의 마지막 문장)은 느린
// 업로드를 흉내 내야 관측되는 서버 타임아웃이라 이 파일의 단언에 없다.
package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	// api.maxRequestBodyBytes / session.MaxClaudePromptBytes as the product
	// spells them. Written out rather than imported so that relaxing either limit
	// fails this file instead of travelling into it.
	a5MaxWireBytes     = 8 << 20
	a5MaxPromptBytes   = 1 << 20
	a5InvalidInput     = "invalid input"
	a5BodyTooLarge     = "request body exceeds size limit"
	a5PromptTooLarge   = "workload prompt exceeds size limit"
	a5ProbeCommand     = "echo a5-probe-$((20+2))"
	a5ProbeOutputToken = "a5-probe-22"
)

// a5Raw sends bytes the API DTOs cannot express — malformed JSON, trailing
// input, explicit null — which `do` cannot, since it marshals a Go value.
func a5Raw(t *testing.T, path string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL()+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s (%d bytes): %v", path, len(body), err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s: %v", path, err)
	}
	return resp, out
}

func a5ErrorField(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, body)
	}
	return e.Error
}

// a5Settled polls until two consecutive full reads agree, so a later "the output
// did not move" assertion is comparing against a quiet shell rather than one
// still flushing its prompt.
func a5Settled(t *testing.T, id string) readResp {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	prev := readShellAt(t, id, 0)
	for {
		time.Sleep(500 * time.Millisecond)
		cur := readShellAt(t, id, 0)
		if cur.Payload == prev.Payload && cur.NextOffset == prev.NextOffset {
			return cur
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s output never settled; last two reads ended at %d and %d", id, prev.NextOffset, cur.NextOffset)
		}
		prev = cur
	}
}

// a5JSONOfSize builds `{"unexpected":"aaa…"}` of exactly n bytes. The field is
// deliberately one the DTOs do not declare, so the *only* thing that can make
// two such bodies answer differently is their length.
func a5JSONOfSize(t *testing.T, n int) []byte {
	t.Helper()
	const prefix, suffix = `{"unexpected":"`, `"}`
	if n < len(prefix)+len(suffix) {
		t.Fatalf("cannot build a %d-byte body; the envelope alone is %d", n, len(prefix)+len(suffix))
	}
	body := make([]byte, 0, n)
	body = append(body, prefix...)
	for len(body) < n-len(suffix) {
		body = append(body, 'a')
	}
	body = append(body, suffix...)
	if len(body) != n {
		t.Fatalf("built a %d-byte body, want %d", len(body), n)
	}
	return body
}

// (a) An omitted body is not a missing field — each route has a documented
// default for it.
func TestWireValidation_OmittedBodyTakesTheDocumentedDefaults(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "echo a5-history-$((40+1))\n")
	eventuallyShellRead(t, s.ID, 0, func(p string) bool { return strings.Contains(p, "a5-history-41") })

	// read with no body must mean offset 0 — the whole history, including what
	// was written before this call. A default of "wherever the cursor is now"
	// would come back without that marker.
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/read", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read with no body: status=%d body=%s (AC-C2 optional body)", resp.StatusCode, body)
	}
	var r readResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode read: %v body=%s", err, body)
	}
	if !strings.Contains(r.Payload, "a5-history-41") {
		t.Fatalf("read with no body returned %q, which does not contain output produced before the call — its offset was not 0 (AC-C2)", r.Payload)
	}
	if r.Session.State != "active" {
		t.Fatalf("state after a body-less read = %q, want active", r.Session.State)
	}

	// write with no body must mean an empty payload: nothing reaches the shell.
	// The probe is typed *without* a newline, so its computed token can only
	// appear if something submits the pending line — an empty payload must not.
	writeShell(t, s.ID, a5ProbeCommand)
	before := a5Settled(t, s.ID)
	resp, body = do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/write", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write with no body: status=%d body=%s (AC-C3 optional body)", resp.StatusCode, body)
	}
	var w writeResp
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode write: %v body=%s", err, body)
	}
	if w.Path != "active" {
		t.Fatalf("body-less write path=%q want active", w.Path)
	}
	after := a5Settled(t, s.ID)
	if after.Payload != before.Payload {
		t.Fatalf("a body-less write moved the shell output (%d -> %d bytes); an empty payload must inject nothing (AC-C3)",
			before.NextOffset, after.NextOffset)
	}
	if strings.Contains(after.Payload, a5ProbeOutputToken) {
		t.Fatalf("the probe command ran after a body-less write — the empty payload submitted the pending line (AC-C3)")
	}

	// switch with no body must mean "no fields", and on an active session that is
	// the AC-C4 no-op: same pod, still active.
	resp, body = do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/switch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch with no body: status=%d body=%s (AC-C4 optional body)", resp.StatusCode, body)
	}
	var switched session
	if err := json.Unmarshal(body, &switched); err != nil {
		t.Fatalf("decode switch: %v body=%s", err, body)
	}
	if switched.State != "active" || switched.Pod != s.Pod {
		t.Fatalf("body-less switch gave state=%q pod=%q, want the unchanged active/%s (AC-C4 no-op)",
			switched.State, switched.Pod, s.Pod)
	}

	// The negative above is only worth something if the token *can* appear.
	writeShell(t, s.ID, "\n")
	eventuallyShellRead(t, s.ID, 0, func(p string) bool { return strings.Contains(p, a5ProbeOutputToken) })
}

// (b)(c) Everything strict JSON validation rejects, on every route that decodes
// a body, with the session's own state as the ground truth for "nothing
// happened".
func TestWireValidation_RejectedBodiesLeaveNoTrace(t *testing.T) {
	status, s := createTyped(t, map[string]any{"name": uniqueName(t), "workloadType": "shell"})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d", status)
	}
	writeShell(t, s.ID, "echo a5-reject-$((30+3))\n")
	eventuallyShellRead(t, s.ID, 0, func(p string) bool { return strings.Contains(p, "a5-reject-33") })
	before := a5Settled(t, s.ID)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"top-level null", `null`},
		{"null field value", `{"offset":null}`},
		{"null nested in an array", `{"offset":[null]}`},
		{"unknown field", `{"nope":1}`},
		{"malformed JSON", `{"offset":`},
		{"trailing input", `{} {}`},
		{"non-object top level", `[]`},
		// (c) — immutable metadata reaches the same wire gate as any other
		// undeclared field; read/write/switch declare none of these.
		{"immutable workloadType", `{"workloadType":"claude-code"}`},
		{"immutable model", `{"model":"platform-default"}`},
	} {
		for _, route := range []string{"/read", "/write", "/switch"} {
			t.Run(tc.name+" on "+strings.TrimPrefix(route, "/"), func(t *testing.T) {
				resp, body := a5Raw(t, "/api/v1/sessions/"+s.ID+route, []byte(tc.body))
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("post %s %s: status=%d body=%s, want 400", route, tc.body, resp.StatusCode, body)
				}
				if got := a5ErrorField(t, body); got != a5InvalidInput {
					t.Fatalf("post %s %s: error=%q want %q — a 400 for some other reason would not say the wire gate caught it",
						route, tc.body, got, a5InvalidInput)
				}
			})
		}
	}

	// A field switch declares nothing of is undeclared even when another route
	// does declare it; without this the table above would also pass against a
	// switch handler that quietly accepted read's DTO.
	resp, body := a5Raw(t, "/api/v1/sessions/"+s.ID+"/switch", []byte(`{"offset":0}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("switch with read's field: status=%d body=%s, want 400 (switch declares no fields)", resp.StatusCode, body)
	}

	// …and the control that the same route says 200 to a body it *does* declare,
	// so the 400s above are about these bodies and not about a5Raw itself.
	resp, body = a5Raw(t, "/api/v1/sessions/"+s.ID+"/read", []byte(`{"offset":0}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read with a declared field: status=%d body=%s, want 200", resp.StatusCode, body)
	}

	after := a5Settled(t, s.ID)
	if after.Payload != before.Payload {
		t.Fatalf("the shell output moved across the rejected round trips (%d -> %d bytes) — a rejected body reached the agent (AC-C3)",
			before.NextOffset, after.NextOffset)
	}
	got := getTyped(t, s.ID)
	if got.WorkloadType != s.WorkloadType || got.Model != s.Model {
		t.Fatalf("metadata moved across the rejected round trips: workloadType %q->%q model %q->%q (immutable after create)",
			s.WorkloadType, got.WorkloadType, s.Model, got.Model)
	}
	if got.State != "active" {
		t.Fatalf("state after the rejected round trips = %q, want active", got.State)
	}
}

// (d) The wire limit is a size gate, not a parse result: two bodies that differ
// by one byte and by nothing else answer 400 and 413.
func TestWireValidation_WireBodyLimitIsExactlyEightMiB(t *testing.T) {
	s := createSession(t, uniqueName(t))
	before := a5Settled(t, s.ID)

	atLimit := a5JSONOfSize(t, a5MaxWireBytes)
	resp, body := a5Raw(t, "/api/v1/sessions/"+s.ID+"/write", atLimit)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("write with a %d-byte body: status=%d body=%s, want 400 — %d bytes is at the limit, not over it",
			len(atLimit), resp.StatusCode, body, a5MaxWireBytes)
	}
	if got := a5ErrorField(t, body); got != a5InvalidInput {
		t.Fatalf("write at the limit: error=%q want %q (it should fail on its undeclared field, not on its size)", got, a5InvalidInput)
	}

	overLimit := a5JSONOfSize(t, a5MaxWireBytes+1)
	resp, body = a5Raw(t, "/api/v1/sessions/"+s.ID+"/write", overLimit)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("write with a %d-byte body: status=%d body=%s, want 413", len(overLimit), resp.StatusCode, body)
	}
	if got := a5ErrorField(t, body); got != a5BodyTooLarge {
		t.Fatalf("write one byte over the limit: error=%q want %q", got, a5BodyTooLarge)
	}

	if after := a5Settled(t, s.ID); after.Payload != before.Payload {
		t.Fatalf("the shell output moved across the oversize round trips (%d -> %d bytes)", before.NextOffset, after.NextOffset)
	}
}

// (e) The decoded prompt limit is a different gate from the wire limit — it is
// workload-specific and it runs *before* the state machine, so a snapshot
// session refuses without waking up. That "without waking up" is the point:
// state-api.md#시나리오 3 owns the fact that an accepted write on a snapshot
// session restores it (e2e_c3_write_branches_test.go), which is what makes the
// absence of a pod here evidence rather than a tautology.
func TestWireValidation_OversizePromptIsRefusedBeforeRestore(t *testing.T) {
	status, s := createTyped(t, map[string]any{"name": uniqueName(t), "workloadType": "claude-code"})
	if status != http.StatusCreated {
		t.Fatalf("create claude-code session: status=%d", status)
	}
	if s.Pod == "" {
		t.Fatal("created claude-code session has no workload pod")
	}

	// Control: this route accepts prompts from this session, so a later refusal
	// is about the payload's size and not about claude-code writes at large.
	if w := writeAt(t, s.ID, "a5-small-prompt"); w.Path != "active" {
		t.Fatalf("small prompt on an active claude-code session: path=%q want active", w.Path)
	}

	frozen, ok := snapshotSession(t, s.ID)
	if !ok {
		t.Skip("SUT predates the product snapshot endpoint — the pre-restore refusal is not exercisable here")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after snapshot = %q, want snapshot", frozen.State)
	}
	if frozen.Pod != "" {
		t.Fatalf("snapshot session still names pod %q; there is no reclaimed state to observe a restore against", frozen.Pod)
	}

	oversize, err := json.Marshal(map[string]string{"payload": strings.Repeat("a", a5MaxPromptBytes+1)})
	if err != nil {
		t.Fatalf("marshal oversize prompt: %v", err)
	}
	if len(oversize) > a5MaxWireBytes {
		t.Fatalf("the oversize prompt is %d bytes on the wire, over the %d-byte wire limit — it would be refused by the wrong gate",
			len(oversize), a5MaxWireBytes)
	}
	resp, body := a5Raw(t, "/api/v1/sessions/"+s.ID+"/write", oversize)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize prompt: status=%d body=%s, want 413", resp.StatusCode, body)
	}
	if got := a5ErrorField(t, body); got != a5PromptTooLarge {
		t.Fatalf("oversize prompt: error=%q want %q — the two 413s have different causes and only this one runs before the state machine",
			got, a5PromptTooLarge)
	}

	after := getTyped(t, s.ID)
	if after.State != "snapshot" {
		t.Fatalf("state after the refused prompt = %q, want the unchanged snapshot — the refusal restored the session (AC-C3)", after.State)
	}
	if after.Pod != "" {
		t.Fatalf("session names pod %q after the refused prompt; the refusal provisioned a pod before checking the prompt", after.Pod)
	}

	cs, _, hasCluster := kubeClient(t)
	if !hasCluster {
		return // the API half is asserted; the pod half needs the deployed cluster
	}
	ns := sessionNamespace()
	// A pod the snapshot already reclaimed can still answer List for the length
	// of its termination grace, so "no pod" is stated as "none that is not
	// already going away" — a restore would have produced a live one.
	for _, p := range a21PodsBySelector(t, cs, ns, labelSessionID+"="+s.ID) {
		if p.DeletionTimestamp == nil {
			t.Fatalf("session %s still has live pod %s after the refused prompt — the restore ran before the size check",
				s.ID, p.Name)
		}
	}
}
