//go:build e2e

// 검증 시나리오: codex-task-workload.md#시나리오 2
package e2e_test

import (
	"net/http"
	"testing"
)

func TestCodexTask2_SidecarTokenServiceAccountAndSlot(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	status, s, raw := createCodexTask(t, nil)
	if status != http.StatusCreated {
		t.Fatalf("create codex-task: %d %s", status, raw)
	}
	pod := sessionWorkloadPod(t, cs, s.ID)
	if pod.Spec.AutomountServiceAccountToken == nil || !*pod.Spec.AutomountServiceAccountToken {
		t.Error("codex-task pod does not mount the data plane service account token")
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("containers = %d, want the agent and k3s-mcp-proxy", len(pod.Spec.Containers))
	}
	main, sidecar := pod.Spec.Containers[0], pod.Spec.Containers[1]
	if sidecar.Name != "k3s-mcp-proxy" {
		t.Fatalf("second container = %s, want k3s-mcp-proxy", sidecar.Name)
	}
	for name, want := range map[string]string{
		"DATA_PLANE_WORKLOAD": "codex-task",
		"K3S_MCP_PROXY_URL":   "http://127.0.0.1:8093",
		"AGENT_AUTH_SLOT_DIR": authSlotMount,
	} {
		if e, ok := containerEnv(main, name); !ok || e.Value != want {
			t.Errorf("main %s = %+v, want %q", name, e, want)
		}
	}
	for _, e := range main.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("main container reads Secret %s into %s", e.ValueFrom.SecretKeyRef.Name, e.Name)
		}
	}
	for _, e := range sidecar.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil &&
			(e.Name != "K3S_MCP_TOKEN" || e.ValueFrom.SecretKeyRef.Name != "codex-task-credentials") {
			t.Errorf("sidecar reads Secret %s into %s", e.ValueFrom.SecretKeyRef.Name, e.Name)
		}
	}
	if e, ok := containerEnv(sidecar, "K3S_MCP_TOOL_POLICY"); !ok || e.Value != "allow-all" {
		t.Errorf("sidecar K3S_MCP_TOOL_POLICY = %+v, want allow-all", e)
	}
}
