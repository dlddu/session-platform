//go:build e2e

// 검증 시나리오: multitenancy.md#시나리오 3
package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestMultitenancy3_GrantBoundsCreateAndConfig(t *testing.T) {
	beta := tenantToken(t, betaAccount, tenantAudience)

	if resp, raw := doAs(t, beta, http.MethodPost, "/api/v1/sessions", map[string]any{"name": uniqueName(t), "workloadType": "codex"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("beta creating codex: %d %s, want 403", resp.StatusCode, raw)
	}
	if resp, raw := doAs(t, beta, http.MethodPost, "/api/v1/sessions", map[string]any{"name": uniqueName(t), "tenant": tenantAlpha}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("beta creating into alpha: %d %s, want 403", resp.StatusCode, raw)
	}
	if resp, raw := doAs(t, "", http.MethodPost, "/api/v1/sessions", map[string]any{"name": uniqueName(t), "tenant": tenantBeta, "workloadType": "codex"}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("operator creating codex for beta: %d %s, want 403", resp.StatusCode, raw)
	}

	resp, raw := doAs(t, beta, http.MethodGet, "/api/v1/config", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("beta config: %d %s", resp.StatusCode, raw)
	}
	var cfg struct {
		WorkloadTypes []struct{ Type string } `json:"workloadTypes"`
		Tenant        *struct{ Name string }  `json:"tenant"`
		Tenants       []struct{ Name string } `json:"tenants"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Tenant == nil || cfg.Tenant.Name != tenantBeta || len(cfg.Tenants) != 0 {
		t.Errorf("beta config tenant=%+v tenants=%+v", cfg.Tenant, cfg.Tenants)
	}
	if len(cfg.WorkloadTypes) != 1 || cfg.WorkloadTypes[0].Type != "shell" {
		t.Errorf("beta workloadTypes = %+v, want shell only", cfg.WorkloadTypes)
	}

	resp, raw = doAs(t, "", http.MethodGet, "/api/v1/config", nil)
	if err := json.Unmarshal(raw, &cfg); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("operator config: %d %s", resp.StatusCode, raw)
	}
	if cfg.Tenant != nil || len(cfg.Tenants) != 2 {
		t.Errorf("operator config tenant=%+v tenants=%+v, want the two SUT tenants", cfg.Tenant, cfg.Tenants)
	}
}
