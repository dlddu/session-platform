//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 11
package e2e_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

func TestClaudeCode11_EffortIsFixedAtCreateAndReachesThePod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	if got := configEfforts(t, "claude-code"); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("/config claude-code reasoningEfforts = %v", got)
	}
	for _, other := range []string{"shell", "approval-gated", "codex-chat"} {
		if got := configEfforts(t, other); len(got) != 0 {
			t.Errorf("/config %s reasoningEfforts = %v, want []", other, got)
		}
	}

	s := createAs(t, "", map[string]any{"name": uniqueName(t), "workloadType": "claude-code", "reasoningEffort": "max"})
	resp, raw := doAs(t, "", http.MethodGet, "/api/v1/sessions/"+s.ID, nil)
	var got struct {
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.ReasoningEffort != "max" {
		t.Fatalf("get: %d %s, want reasoningEffort max", resp.StatusCode, raw)
	}
	pod := sessionWorkloadPod(t, cs, s.ID)
	if e, ok := containerEnv(pod.Spec.Containers[0], "CLAUDE_CODE_EFFORT"); !ok || e.Value != "max" {
		t.Errorf("CLAUDE_CODE_EFFORT = %+v, want max", e)
	}

	for _, other := range []string{"shell", "approval-gated", "codex-chat"} {
		name := uniqueName(t)
		resp, raw := doAs(t, "", http.MethodPost, "/api/v1/sessions", map[string]any{
			"name": name, "workloadType": other, "reasoningEffort": "high",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s with reasoningEffort: %d %s, want 400", other, resp.StatusCode, raw)
			if resp.StatusCode == http.StatusCreated {
				var made tenantSession
				if json.Unmarshal(raw, &made) == nil {
					doAs(t, "", http.MethodDelete, "/api/v1/sessions/"+made.ID, nil)
				}
			}
		}
		assertNoSessionNamed(t, name)
	}
}
