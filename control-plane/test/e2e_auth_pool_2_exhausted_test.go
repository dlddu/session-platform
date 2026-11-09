//go:build e2e

// 검증 시나리오: agent-auth-pool.md#시나리오 2
package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestAuthPool2_FullPoolRefusesWithoutAPod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	made, status, raw := fillCodexTaskPool(t)
	if len(made) == 0 {
		t.Fatalf("pool refused the first codex-task session: %d %s", status, raw)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("create on a full pool: %d %s, want 503", status, raw)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Error != "auth pool exhausted" {
		t.Fatalf("503 body = %s, want error \"auth pool exhausted\"", raw)
	}

	seen := map[int]string{}
	for _, s := range made {
		slot := heldSlotOf(t, cs, codexTaskPool, s.ID)
		if slot < 0 {
			t.Fatalf("session %s holds no slot", s.ID)
		}
		if other, dup := seen[slot]; dup {
			t.Fatalf("sessions %s and %s hold the same slot %d", other, s.ID, slot)
		}
		seen[slot] = s.ID
	}

	// The refused create left nothing behind: every session of the type that
	// exists is one this test made or another suite's, and each holds a slot.
	for _, l := range poolLeases(t, cs, codexTaskPool) {
		if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" {
			t.Errorf("lease %s is free but the pool said it was exhausted", l.Name)
		}
	}
	for _, s := range listAs(t, "", "") {
		if s.WorkloadType == codexTaskPool && heldSlotOf(t, cs, codexTaskPool, s.ID) < 0 {
			t.Errorf("codex-task session %s exists without a slot", s.ID)
		}
	}
}
