//go:build e2e

// 검증 시나리오: codex-task-workload.md#시나리오 4
package e2e_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

func TestCodexTask4_EffortIsFixedAtCreateAndReachesThePod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	if got := configEfforts(t, codexTaskPool); !slices.Equal(got, []string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Fatalf("/config codex-task reasoningEfforts = %v", got)
	}

	status, s, raw := createCodexTask(t, map[string]any{"reasoningEffort": "high"})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, raw)
	}
	resp, raw := doAs(t, "", http.MethodGet, "/api/v1/sessions/"+s.ID, nil)
	var got struct {
		ReasoningEffort string `json:"reasoningEffort"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.ReasoningEffort != "high" {
		t.Fatalf("get: %d %s, want reasoningEffort high", resp.StatusCode, raw)
	}
	pod := sessionWorkloadPod(t, cs, s.ID)
	if e, ok := containerEnv(pod.Spec.Containers[0], "CODEX_REASONING_EFFORT"); !ok || e.Value != "high" {
		t.Errorf("CODEX_REASONING_EFFORT = %+v, want high", e)
	}

	for _, bad := range []any{"max", ""} {
		name := uniqueName(t)
		status, _, raw := createCodexTask(t, map[string]any{"name": name, "reasoningEffort": bad})
		if status != http.StatusBadRequest {
			t.Errorf("reasoningEffort %q: %d %s, want 400", bad, status, raw)
		}
		assertNoSessionNamed(t, name)
	}
}

// configEfforts is the reasoningEfforts list /config offers for a type.
func configEfforts(t *testing.T, workloadType string) []string {
	t.Helper()
	resp, raw := doAs(t, "", http.MethodGet, "/api/v1/config", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config: %d %s", resp.StatusCode, raw)
	}
	var cfg struct {
		WorkloadTypes []struct {
			Type             string   `json:"type"`
			ReasoningEfforts []string `json:"reasoningEfforts"`
		} `json:"workloadTypes"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	for _, w := range cfg.WorkloadTypes {
		if w.Type == workloadType {
			if w.ReasoningEfforts == nil {
				t.Fatalf("/config %s reasoningEfforts is null", workloadType)
			}
			return w.ReasoningEfforts
		}
	}
	t.Fatalf("/config does not offer %s", workloadType)
	return nil
}

// assertNoSessionNamed checks a refused create left no session (and so no pod).
func assertNoSessionNamed(t *testing.T, name string) {
	t.Helper()
	for _, s := range listAs(t, "", "") {
		if s.Name == name {
			t.Errorf("refused create left session %s (%s)", s.ID, name)
		}
	}
}
