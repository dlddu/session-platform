package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Who may move a slot's writer, through the product's own two hops.
//
// Being in the pod is not the authorisation. The workload container shares this
// sidecar's loopback, so the session's own model reaches the very address the
// control plane reaches; what it does not have is the credential the
// orchestrator puts on the sidecar alone. These tests drive the real handlers
// and the real agent relay — the relay carries the caller's header and holds no
// credential of its own, which is what keeps the workload out.

// sealPost calls the sidecar's writer route with whatever authorisation the
// caller has, the way the agent relays one.
func sealPost(t *testing.T, base, path, auth string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if json.Unmarshal(payload, &decoded) != nil {
		decoded = map[string]any{"text": strings.TrimSpace(string(payload))}
	}
	return resp.StatusCode, decoded
}

func TestOnlyACallerWithTheSidecarsCredentialMayMoveTheWriter(t *testing.T) {
	const credential = testWriterCredential
	t.Setenv(monitorPairWriterTokenEnv, credential)
	slot := handoverFixture(t, t.TempDir())

	// The session's own model: it is in the pod, it can reach loopback, and it
	// has nothing else. Every shape of "nothing else" is refused.
	for _, auth := range []string{"", "Bearer ", "Bearer guess", "Bearer " + credential + "x"} {
		status, body := sealPost(t, slot.base, sessionDBWorkWriterPath, auth,
			map[string]any{"writer": workWriterMonitor, "holder": "mon-1"})
		if status != http.StatusForbidden {
			t.Fatalf("a caller with %q moved the writer: %d %+v", auth, status, body)
		}
		if body["code"] != "permission_denied" {
			t.Fatalf("refusal for %q = %+v", auth, body)
		}
	}
	// It cannot write as the monitor either.
	if status, _ := sealPost(t, slot.base, sessionDBWorkCallPath, "",
		map[string]any{"name": "turn_append", "args": map[string]any{"task_id": "task-old"}}); status != http.StatusForbidden {
		t.Fatalf("a caller with no credential wrote as the monitor: %d", status)
	}
	// Nothing moved: the session still owns its records and still writes them.
	if writer := slot.writerNow(); writer["writer"] != workWriterSession {
		t.Fatalf("the refused calls moved the seal: %+v", writer)
	}
	slot.mustOwn("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "still-mine",
		"turn": map[string]any{"request": "거절 뒤에도 내가 쓴다"},
	})

	// The control plane, with the credential, does move it.
	status, sealed := sealPost(t, slot.base, sessionDBWorkWriterPath, "Bearer "+credential,
		map[string]any{"writer": workWriterMonitor, "holder": "mon-1"})
	if status != http.StatusOK || sealed["changed"] != true {
		t.Fatalf("the control plane could not seal: %d %+v", status, sealed)
	}
}

// writerNow needs the credential too, so the helper used by the other tests
// goes through the same gate. A slot with no credential configured refuses the
// whole path — which is what makes the pair's opt-in decline rather than run
// without this boundary.
func TestASlotWithNoCredentialRefusesTheHandoverOutright(t *testing.T) {
	t.Setenv(monitorPairWriterTokenEnv, "")
	slot := handoverFixture(t, t.TempDir())
	for _, path := range []string{sessionDBWorkWriterPath, sessionDBWorkCallPath} {
		status, body := sealPost(t, slot.base, path, "Bearer anything", map[string]any{
			"writer": workWriterMonitor, "holder": "mon-1", "name": "turn_append"})
		if status != http.StatusNotImplemented || body["code"] != "unsupported" {
			t.Fatalf("%s on an unconfigured slot = %d %+v, want an outright refusal", path, status, body)
		}
	}
	// And the session keeps working exactly as it did before the feature
	// existed: this is the state every deployment is in until it is prepared.
	slot.mustOwn("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "unconfigured",
		"turn": map[string]any{"request": "기능 없는 배포에서도 그대로"},
	})
	if got := slot.turnTexts("task-old"); len(got) != 3 {
		t.Fatalf("an unconfigured slot lost its own path: %v", got)
	}
}

// The whole normal path: control plane → the workload agent's route on the pod
// network → the sidecar's loopback route. The agent is in the workload
// container and holds no credential; it carries the caller's header. So the
// same relay that serves the control plane refuses the model beside it.
func TestTheAgentRelayCarriesTheCredentialAndHoldsNone(t *testing.T) {
	const credential = testWriterCredential
	t.Setenv(monitorPairWriterTokenEnv, credential)
	dir := t.TempDir()
	slot := handoverFixture(t, dir)

	// The agent, wired the way the product wires it: the real server, with a
	// codex workload whose session DB sidecar is this slot.
	agentSrv := relayTestWorkload(t, slot.base+sessionDBMCPPath)

	// Through the relay with no credential — a caller on the pod network that
	// is not the control plane — the sidecar refuses and the refusal is what
	// comes back, not a gateway error.
	if status, body := sealPost(t, agentSrv.URL, agentWorkWriterPath, "",
		map[string]any{"writer": workWriterMonitor, "holder": "mon-1"}); status != http.StatusForbidden {
		t.Fatalf("the relay let an uncredentialed caller seal: %d %+v", status, body)
	}

	// With the credential the handover goes through all three hops.
	status, sealed := sealPost(t, agentSrv.URL, agentWorkWriterPath, "Bearer "+credential,
		map[string]any{"writer": workWriterMonitor, "holder": "mon-1"})
	if status != http.StatusOK || sealed["changed"] != true {
		t.Fatalf("relayed handover = %d %+v", status, sealed)
	}

	// And so does the monitor's write, landing in the same records the session
	// read before the handover.
	status, called := sealPost(t, agentSrv.URL, agentWorkCallPath, "Bearer "+credential,
		map[string]any{"name": "turn_append", "args": map[string]any{
			"task_id": "task-old", "request_id": "relayed-1",
			"expected_version":      1,
			"expected_turn_version": monitorInt(t, slot.mustOwn("turn_list", map[string]any{"task_id": "task-old"}), "turn_version"),
			"turn":                  map[string]any{"request": "릴레이를 지나 기록된 지시"},
		}})
	if status != http.StatusOK || called["isError"] == true {
		t.Fatalf("relayed monitor write = %d %+v", status, called)
	}
	if got := slot.turnTexts("task-old"); len(got) != 3 || got[2] != "릴레이를 지나 기록된 지시" {
		t.Fatalf("the relayed write is not in the one history: %v", got)
	}
	// The session itself is sealed out, as it must be once the pair holds the
	// records — and it learned that without any restart.
	if _, code := slot.own("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "after-relay",
		"turn": map[string]any{"request": "봉인 뒤 부모"},
	}); code != "permission_denied" {
		t.Fatalf("the sealed session wrote after a relayed handover: %q", code)
	}
}
