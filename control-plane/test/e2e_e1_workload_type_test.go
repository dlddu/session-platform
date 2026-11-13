//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 1
//
// docs/prd/claude-code-workload.md AC-E1, asserted against the kind cluster.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Declared locally so this suite keeps asserting the wire contract rather than
// the control plane's internal types.
type typedSession struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	Pod          string `json:"pod"`
	WorkloadType string `json:"workloadType"`
	Model        string `json:"model"`
}

// The Claude pod also carries an isolated credential-proxy sidecar, so exec has
// to name the container it wants.
const workloadContainer = "session"

func createTyped(t *testing.T, body map[string]any) (int, typedSession) {
	t.Helper()
	resp, raw := do(t, http.MethodPost, "/api/v1/sessions", body)
	var s typedSession
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode created session: %v body=%s", err, raw)
		}
	}
	return resp.StatusCode, s
}

func getTyped(t *testing.T, id string) typedSession {
	t.Helper()
	resp, raw := do(t, http.MethodGet, "/api/v1/sessions/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status=%d body=%s", id, resp.StatusCode, raw)
	}
	var s typedSession
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode session: %v body=%s", err, raw)
	}
	return s
}

// The shared execInPod helper targets single-container shell pods; a Claude pod
// needs the container named explicitly.
func execInContainer(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod, container string, command []string) (string, string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("build exec executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

// Create only returns after the pod reports Ready, which needs the agent and
// both sidecars up — so a passing create is itself evidence that they started.
func TestWorkloadType_ClaudeCodeSessionRunsTheClaudeCLI(t *testing.T) {
	status, s := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code",
	})
	if status != http.StatusCreated {
		t.Fatalf("create claude-code session: status=%d", status)
	}
	if s.State != "active" {
		t.Fatalf("state=%q want active", s.State)
	}
	if s.WorkloadType != "claude-code" {
		t.Fatalf("workloadType=%q want claude-code", s.WorkloadType)
	}
	if s.Pod == "" {
		t.Fatal("claude-code session has no dedicated pod (AC-A2)")
	}

	cs, cfg, ok := kubeClient(t)
	if !ok {
		return // wire contract asserted; the cluster half needs the deployed cluster
	}
	ns := sessionNamespace()
	pod := getPodEventually(t, cs, ns, s.Pod)
	if _, found := containerByName(pod, workloadContainer); !found {
		t.Fatalf("pod %s has no %q container: %v", s.Pod, workloadContainer, containerNames(pod))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stdout, stderr, err := execInContainer(ctx, cs, cfg, ns, s.Pod, workloadContainer,
		[]string{"claude", "--version"})
	if err != nil {
		t.Fatalf("claude --version in %s/%s: %v (stdout=%q stderr=%q)", ns, s.Pod, err, stdout, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("claude --version printed nothing (stderr=%q) — the CLI is not runnable in the session pod", stderr)
	}
}

// The kind SUT gives claude-code no K3s MCP token, as it gives codex none: the
// pod's k3s-mcp-proxy sidecar starts unconfigured, and a session that selected
// plugins still reaches Ready, without them (AC-E6, AC-L1). The sidecar's own
// health route is what the agent reads to decide that.
func TestWorkloadType_ClaudeCodeStartsWithAnUnconfiguredK3sSidecar(t *testing.T) {
	status, s := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code", "plugins": []string{"development"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create claude-code session: status=%d", status)
	}
	if s.State != "active" {
		t.Fatalf("state=%q want active", s.State)
	}
	cs, cfg, ok := kubeClient(t)
	if !ok {
		return
	}
	ns := sessionNamespace()
	pod := getPodEventually(t, cs, ns, s.Pod)
	if _, found := containerByName(pod, "k3s-mcp-proxy"); !found {
		t.Fatalf("pod %s has no k3s-mcp-proxy sidecar: %v", s.Pod, containerNames(pod))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stdout, stderr, err := execInContainer(ctx, cs, cfg, ns, s.Pod, workloadContainer,
		[]string{"curl", "-sS", "--max-time", "10", "http://127.0.0.1:8093/healthz"})
	if err != nil {
		t.Fatalf("sidecar health from %s/%s: %v (stdout=%q stderr=%q)", ns, s.Pod, err, stdout, stderr)
	}
	if !strings.Contains(stdout, `"workload":"k3s-mcp-proxy"`) || !strings.Contains(stdout, `"configured":false`) {
		t.Fatalf("sidecar health = %q, want an unconfigured k3s-mcp-proxy", stdout)
	}
}

func TestWorkloadType_OmittedDefaultsToShell(t *testing.T) {
	status, s := createTyped(t, map[string]any{"name": uniqueName(t)})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d", status)
	}
	if s.WorkloadType != "shell" {
		t.Fatalf("workloadType=%q want shell", s.WorkloadType)
	}
	if s.State != "active" || s.Pod == "" {
		t.Fatalf("state=%q pod=%q — default session did not come up", s.State, s.Pod)
	}
}

// The pod count is the ground truth for "before provisioning": a rejected
// create must not leave a pod behind.
func TestWorkloadType_InvalidValuesRejectedBeforeProvisioning(t *testing.T) {
	cs, _, hasCluster := kubeClient(t)
	ns := sessionNamespace()
	before := podCount(t, cs, ns, hasCluster)

	// approval-gated was a type until the approval gate became a capability of
	// other types (docs/prd/approval-gate.md); a deployed control plane must
	// now refuse it like any other unknown value.
	for _, bad := range []any{"", nil, 42, "SHELL", "claude_code", " claude-code", "foo", "approval-gated"} {
		status, _ := createTyped(t, map[string]any{"name": uniqueName(t), "workloadType": bad})
		if status != http.StatusBadRequest {
			t.Fatalf("workloadType=%#v status=%d, want 400", bad, status)
		}
	}

	if after := podCount(t, cs, ns, hasCluster); hasCluster && after != before {
		t.Fatalf("session pod count moved %d -> %d across rejected creates — a pod was provisioned", before, after)
	}
}

func TestWorkloadType_ImmutableAfterCreate(t *testing.T) {
	const model = "claude-e2e-model"
	status, created := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code", "model": model,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d", status)
	}

	for _, path := range []string{"/read", "/write", "/switch"} {
		resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+created.ID+path, map[string]any{
			"workloadType": "shell", "model": "platform-default", "payload": "x",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("post %s status=%d want 400 body=%s", path, resp.StatusCode, body)
		}
	}

	got := getTyped(t, created.ID)
	if got.WorkloadType != "claude-code" {
		t.Fatalf("workloadType after mutating calls = %q, want claude-code", got.WorkloadType)
	}
	if got.Model != model {
		t.Fatalf("model after mutating calls = %q, want %q", got.Model, model)
	}
}

func containerByName(pod *corev1.Pod, name string) (corev1.Container, bool) {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

func containerNames(pod *corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

func podCount(t *testing.T, cs kubernetes.Interface, ns string, hasCluster bool) int {
	t.Helper()
	if !hasCluster {
		return 0
	}
	list, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: "session-id",
	})
	if err != nil {
		t.Fatalf("list session pods in %s: %v", ns, err)
	}
	return len(list.Items)
}
