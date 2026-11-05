//go:build e2e

// 검증 시나리오: multitenancy.md#시나리오 2
package e2e_test

import (
	"net/http"
	"testing"
)

func TestMultitenancy2_OtherSessionsAnswerAsMissing(t *testing.T) {
	alpha := tenantToken(t, alphaAccount, tenantAudience)
	beta := tenantToken(t, betaAccount, tenantAudience)
	betas := createAs(t, beta, map[string]any{"name": uniqueName(t)})
	operators := createAs(t, "", map[string]any{"name": uniqueName(t)})
	if operators.Tenant != "" {
		t.Fatalf("operator session without a tenant got tenant %q", operators.Tenant)
	}

	for _, id := range []string{betas.ID, operators.ID} {
		for _, r := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/v1/sessions/" + id, nil},
			{http.MethodPost, "/api/v1/sessions/" + id + "/read", map[string]any{"offset": 0}},
			{http.MethodPost, "/api/v1/sessions/" + id + "/write", map[string]any{"payload": "id\n"}},
			{http.MethodPost, "/api/v1/sessions/" + id + "/switch", map[string]any{}},
			{http.MethodPost, "/api/v1/sessions/" + id + "/snapshot", map[string]any{}},
			{http.MethodDelete, "/api/v1/sessions/" + id, nil},
		} {
			if resp, raw := doAs(t, alpha, r.method, r.path, r.body); resp.StatusCode != http.StatusNotFound {
				t.Errorf("alpha %s %s: %d %s, want 404", r.method, r.path, resp.StatusCode, raw)
			}
		}
		if resp, raw := doAs(t, "", http.MethodGet, "/api/v1/sessions/"+id, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("session %s did not survive alpha's attempts: %d %s", id, resp.StatusCode, raw)
		}
	}
	if _, ok := listAs(t, "", "")[betas.ID]; !ok {
		t.Error("the operator's list misses beta's session")
	}
	if got := listAs(t, "", "?tenant="+tenantBeta); len(got) == 0 || got[betas.ID].Tenant != tenantBeta {
		t.Errorf("operator ?tenant=%s = %v", tenantBeta, got)
	}
}
