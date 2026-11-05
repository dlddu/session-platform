//go:build e2e

// 검증 시나리오: codex-workload.md#시나리오 2
package e2e_test

import (
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCodex2_OnePodTokenOnlyInSidecar(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	s := createAs(t, "", map[string]any{"name": uniqueName(t), "workloadType": "codex"})
	resp, raw := doAs(t, "", http.MethodGet, "/api/v1/sessions/"+s.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d %s", resp.StatusCode, raw)
	}
	pods, err := cs.CoreV1().Pods(sessionNamespace()).List(context.Background(), metav1.ListOptions{
		LabelSelector: "session-id=" + s.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pods for the session = %d, want 1 (no helper pod)", len(pods.Items))
	}
	pod := pods.Items[0]
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("codex pod mounts an API credential")
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("containers = %d, want the agent and k3s-mcp-proxy", len(pod.Spec.Containers))
	}
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			secret := e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil
			switch c.Name {
			case "k3s-mcp-proxy":
				if secret && (e.Name != "K3S_MCP_TOKEN" || e.ValueFrom.SecretKeyRef.Name != "codex-credentials") {
					t.Errorf("sidecar reads Secret %s into %s", e.ValueFrom.SecretKeyRef.Name, e.Name)
				}
			default:
				if secret {
					t.Errorf("container %s reads Secret %s into %s", c.Name, e.ValueFrom.SecretKeyRef.Name, e.Name)
				}
				if e.Name == "K3S_MCP_PROXY_URL" && e.Value != "http://127.0.0.1:8093" {
					t.Errorf("K3S_MCP_PROXY_URL = %q, want the loopback sidecar", e.Value)
				}
			}
		}
	}
}
