//go:build e2e

// 검증 AC: AC-E1
//
// Workload type selection on the *deployed* SUT (docs/prd/claude-code-workload.md
// AC-E1). The AC's verification method has four parts and this file asserts all
// four against the kind cluster:
//
//  1. `workloadType=claude-code` provisions a session whose pod can actually run
//     the Claude Code CLI (ground truth: `claude --version` inside the pod's
//     workload container),
//  2. omitting the field still yields a `shell` session,
//  3. explicit ""/null/non-string and unknown types are rejected with 400
//     *before* any pod is created,
//  4. existing-session routes reject workload/model mutations and the original
//     values survive.
//
// Part 1 only became reachable once the overlay stood up the two bootstrap
// endpoints the session pod contacts before its agent starts — see
// docs/test/e2e.md 「e2e 충실도 허용목록」 (`PLUGIN-CRED`) and deploy/.
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

// typedSession extends the shared DTO with the two immutable workload fields.
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

// workloadContainer is the tool-running container of a session pod. The Claude
// pod also carries an isolated credential-proxy sidecar, so exec must name it.
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

// execInContainer is execInPod with an explicit container, which a multi-
// container pod needs (the shared helper targets single-container shell pods).
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

// A claude-code session stands up on the deployed SUT and its pod really can
// run the Claude Code CLI. Create only returns after the pod reports Ready, and
// the pod cannot reach Ready unless entrypoint.sh completed its plugin
// bootstrap — so a passing create is itself evidence that the bootstrap ran.
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

// The plugin bootstrap resolved against the endpoints THIS overlay deploys, not
// against github.com. Pod Ready already proves the bootstrap ran to completion —
// entrypoint.sh is `set -eu`, so a failed token request or a failed
// `claude plugin marketplace add` kills the container before the agent starts.
// What this test adds is *which remote answered*: the in-cluster fixture carries
// a marker string that exists in no other marketplace, so finding it in the pod's
// plugin cache is decisive.
//
// The paths are entrypoint.sh's documented defaults, which the control plane does
// not override: the bootstrap runs the CLI under its own HOME and hands the agent
// the plugin cache it populated (a fresh `claude` invocation with a different HOME
// sees neither, which is why this asserts on the filesystem rather than on
// `claude plugin marketplace list`).
func TestWorkloadType_ClaudeCodePluginBootstrapUsedTheInClusterMarketplace(t *testing.T) {
	status, s := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code",
	})
	if status != http.StatusCreated {
		t.Fatalf("create claude-code session: status=%d", status)
	}
	cs, cfg, ok := kubeClient(t)
	if !ok {
		return
	}
	ns := sessionNamespace()
	getPodEventually(t, cs, ns, s.Pod)

	const (
		pluginCacheDir = "/tmp/session-platform-claude-plugin-seed"
		bootstrapHome  = "/tmp/session-platform-claude-plugin-home"
		// Present only in deploy/plugin-marketplace-git.yaml's fixture.
		fixtureMarker = "kind e2e fixture"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stdout, stderr, err := execInContainer(ctx, cs, cfg, ns, s.Pod, workloadContainer,
		[]string{"/bin/sh", "-c",
			"grep -rl '" + fixtureMarker + "' " + pluginCacheDir + " " + bootstrapHome +
				" 2>/dev/null | head -5; " +
				"echo '--- cache tree ---'; find " + pluginCacheDir + " " + bootstrapHome +
				" -maxdepth 4 2>/dev/null | head -40"})
	if err != nil {
		t.Fatalf("probe plugin cache in %s/%s: %v (stderr=%q)", ns, s.Pod, err, stderr)
	}
	hits, _, _ := strings.Cut(stdout, "--- cache tree ---")
	if strings.TrimSpace(hits) == "" {
		t.Fatalf("the in-cluster marketplace fixture is not in the session pod's plugin cache "+
			"— the bootstrap resolved some other remote.\n%s", stdout)
	}
}

// Omitting the field keeps the historical default: a shell session.
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

// Invalid types are rejected on the wire, before anything is provisioned. The
// pod count is the ground truth for "before provisioning": a rejected create
// must not leave a pod behind.
func TestWorkloadType_InvalidValuesRejectedBeforeProvisioning(t *testing.T) {
	cs, _, hasCluster := kubeClient(t)
	ns := sessionNamespace()
	before := podCount(t, cs, ns, hasCluster)

	for _, bad := range []any{"", nil, 42, "SHELL", "claude_code", " claude-code", "foo"} {
		status, _ := createTyped(t, map[string]any{"name": uniqueName(t), "workloadType": bad})
		if status != http.StatusBadRequest {
			t.Fatalf("workloadType=%#v status=%d, want 400", bad, status)
		}
	}

	if after := podCount(t, cs, ns, hasCluster); hasCluster && after != before {
		t.Fatalf("session pod count moved %d -> %d across rejected creates — a pod was provisioned", before, after)
	}
}

// Workload type and model are immutable: the existing-session routes reject
// attempted mutations and the original values survive.
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

// podCount counts session pods (those labelled with a session id) in ns.
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
