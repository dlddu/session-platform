//go:build e2e

// 검증 시나리오: multitenancy.md#시나리오 1
package e2e_test

import (
	"net/http"
	"testing"
)

func TestMultitenancy1_TenantCreatesAndListsOnlyItsOwn(t *testing.T) {
	alpha := tenantToken(t, alphaAccount, tenantAudience)
	beta := tenantToken(t, betaAccount, tenantAudience)

	mine := createAs(t, alpha, map[string]any{"name": uniqueName(t)})
	if mine.Tenant != tenantAlpha || mine.State != "active" {
		t.Fatalf("alpha's session = %+v, want tenant %q and active", mine, tenantAlpha)
	}
	theirs := createAs(t, beta, map[string]any{"name": uniqueName(t)})

	alphaView := listAs(t, alpha, "")
	if _, ok := alphaView[mine.ID]; !ok {
		t.Errorf("alpha's list misses its own session %s", mine.ID)
	}
	if _, ok := alphaView[theirs.ID]; ok {
		t.Errorf("alpha's list shows beta's session %s", theirs.ID)
	}
	for _, s := range alphaView {
		if s.Tenant != tenantAlpha {
			t.Errorf("alpha's list holds %s owned by %q", s.ID, s.Tenant)
		}
	}
	if resp, raw := doAs(t, alpha, http.MethodGet, "/api/v1/sessions/"+mine.ID, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("alpha get own: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := doAs(t, alpha, http.MethodPost, "/api/v1/sessions/"+mine.ID+"/write", map[string]any{"payload": "echo tenant-ok\n"}); resp.StatusCode != http.StatusOK {
		t.Errorf("alpha write own: %d %s", resp.StatusCode, raw)
	}
}
