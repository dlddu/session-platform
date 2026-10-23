//go:build e2e

// 검증 시나리오: 없음 (스모크·인프라)
//
// Registered as a non-AC matching unit in docs/test/e2e.md §"비-AC 파일 등재".
//
// Everything on the path except the answering service is real, which is what
// makes the assertion worth having: a stand-in that answered without TLS, or a
// proxy that trusted anything, would pass a weaker test than this one.
package e2e_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// providerReplyMarker comes from deploy/e2e-anthropic-fake.yaml and appears
// nowhere else in the tree, so finding it inside a session pod says the bytes
// came from that deployment rather than from a cache or a default somewhere
// along the way.
const providerReplyMarker = "session-platform-e2e-provider-ok"

// -sS keeps the body clean while still surfacing transport errors, and
// --fail-with-body turns a proxy 5xx into a non-zero exit whose body still
// reaches the test.
func curlProxy(t *testing.T, pod string, body string, extra ...string) string {
	t.Helper()
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; this assertion needs the deployed SUT")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	command := append([]string{
		"curl", "-sS", "--fail-with-body", "--max-time", "20",
		"-H", "Content-Type: application/json",
		"-X", "POST", "--data", body,
	}, extra...)
	command = append(command, "http://127.0.0.1:8091/v1/messages")

	stdout, stderr, err := execInContainer(ctx, cs, cfg, sessionNamespace(), pod, workloadContainer, command)
	if err != nil {
		t.Fatalf("curl through the credential proxy in %s: %v (stdout=%q stderr=%q)", pod, err, stdout, stderr)
	}
	return stdout
}

func newClaudeSessionPod(t *testing.T) string {
	t.Helper()
	status, s := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code",
	})
	if status != http.StatusCreated {
		t.Fatalf("create claude-code session: status=%d", status)
	}
	if s.Pod == "" {
		t.Fatal("claude-code session has no dedicated pod")
	}
	t.Cleanup(func() {
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil); resp.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", s.ID, resp.StatusCode, raw)
		}
	})
	return s.Pod
}

func TestProviderReachableThroughTheCredentialProxy(t *testing.T) {
	pod := newClaudeSessionPod(t)

	out := curlProxy(t, pod, `{"model":"claude-e2e-model","max_tokens":16,`+
		`"messages":[{"role":"user","content":"ping"}]}`)
	if !strings.Contains(out, providerReplyMarker) {
		t.Fatalf("provider reply = %q, want it to contain %q — the session pod did not reach the provider",
			out, providerReplyMarker)
	}
	if strings.Contains(out, "e2e-placeholder-not-a-real-token") {
		t.Fatalf("provider reply echoed the platform token back into the session container: %q", out)
	}
}

// Worth its own case because a provider that only ever answered with a single
// JSON object would leave the proxy's incremental-forwarding path unexercised
// in the deployed SUT.
func TestProviderStreamingResponseSurvivesTheCredentialProxy(t *testing.T) {
	pod := newClaudeSessionPod(t)

	out := curlProxy(t, pod, `{"model":"claude-e2e-model","max_tokens":16,"stream":true,`+
		`"messages":[{"role":"user","content":"ping"}]}`)
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("streamed reply is missing %q: %q", want, out)
		}
	}
	if !strings.Contains(out, providerReplyMarker) {
		t.Fatalf("streamed reply = %q, want it to carry %q", out, providerReplyMarker)
	}
}

// The stand-in answers 401 to anything but the platform token, so a reply
// carrying the marker is what proves the injection happened and the forged
// header was dropped.
func TestCredentialProxyReplacesCallerSuppliedAuthorization(t *testing.T) {
	pod := newClaudeSessionPod(t)

	out := curlProxy(t, pod, `{"model":"claude-e2e-model","max_tokens":16,`+
		`"messages":[{"role":"user","content":"ping"}]}`,
		"-H", "Authorization: Bearer forged-by-the-session")
	if !strings.Contains(out, providerReplyMarker) {
		t.Fatalf("reply = %q, want the provider marker — a forged Authorization must be replaced, not forwarded", out)
	}
}
