//go:build e2e

// 검증 시나리오: codex-workload.md#시나리오 4
package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	stdout, stderr, err := execInPod(ctx, cs, cfg, sessionNamespace(), pods.Items[0].Name,
		[]string{"node", "/opt/codex-runner/surface-check.mjs", "--home", "/session/state/home"})
	if err != nil {
		t.Fatalf("surface check failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "surface-check: ok") {
		t.Fatalf("surface check did not report ok:\n%s\n%s", stdout, stderr)
	}
	config, _, err := execInPod(ctx, cs, cfg, sessionNamespace(), pods.Items[0].Name,
		[]string{"cat", "/session/state/home/.codex/config.toml"})
	if err != nil || !strings.Contains(config, "[mcp_servers.skills]") {
		t.Fatalf("managed config.toml lacks the skills MCP: %v\n%s", err, config)
	}
}
