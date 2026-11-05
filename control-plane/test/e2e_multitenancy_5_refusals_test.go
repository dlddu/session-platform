//go:build e2e

// 검증 시나리오: multitenancy.md#시나리오 5
package e2e_test

import (
	"net/http"
	"testing"
)

func TestMultitenancy5_RefusedCallersNeverBecomeTheOperator(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"not a token", "garbage", http.StatusUnauthorized},
		{"apiserver audience", tenantToken(t, alphaAccount, "https://kubernetes.default.svc.cluster.local"), http.StatusUnauthorized},
		{"no tenant claims the account", tenantToken(t, outsiderAccount, tenantAudience), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/sessions", "/api/v1/config"} {
				if resp, raw := doAs(t, tc.token, http.MethodGet, path, nil); resp.StatusCode != tc.want {
					t.Errorf("GET %s: %d %s, want %d", path, resp.StatusCode, raw, tc.want)
				}
			}
		})
	}
	alpha := tenantToken(t, alphaAccount, tenantAudience)
	if resp, raw := doAs(t, alpha, http.MethodGet, "/api/v1/repo-access", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("tenant on the repo-access management API: %d %s, want 403", resp.StatusCode, raw)
	}
}
