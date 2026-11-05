//go:build e2e

// 검증 시나리오: codex-workload.md#시나리오 4
package e2e_test

import (
	"bytes"
	"context"
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

// execInAgent is execInPod pinned to the workload container: a codex pod has a
// sidecar, so the API server will not pick a default.
func execInAgent(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod string, command []string) (string, string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: "session", Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

// The same check the image build runs, here against the HOME a live session
// pod actually booted with — so a managed config the pod rewrote on start is
// what gets rendered, not the build's copy.
func TestCodex4_ModelSeesNoShellFileOrImageTool(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	s := createAs(t, "", map[string]any{"name": uniqueName(t), "workloadType": "codex"})
	pods, err := cs.CoreV1().Pods(sessionNamespace()).List(context.Background(), metav1.ListOptions{LabelSelector: "session-id=" + s.ID})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("codex pod: %v (%d pods)", err, len(pods.Items))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	stdout, stderr, err := execInAgent(ctx, cs, cfg, sessionNamespace(), pods.Items[0].Name,
		[]string{"node", "/opt/codex-runner/surface-check.mjs", "--home", "/session/state/home"})
	if err != nil {
		t.Fatalf("surface check failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "surface-check: ok") {
		t.Fatalf("surface check did not report ok:\n%s\n%s", stdout, stderr)
	}
	config, _, err := execInAgent(ctx, cs, cfg, sessionNamespace(), pods.Items[0].Name,
		[]string{"cat", "/session/state/home/.codex/config.toml"})
	if err != nil || !strings.Contains(config, "[mcp_servers.skills]") {
		t.Fatalf("managed config.toml lacks the skills MCP: %v\n%s", err, config)
	}
}
