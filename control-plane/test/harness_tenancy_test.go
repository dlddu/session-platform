//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The tenants deploy/overlays/e2e/e2e-tenancy.yaml configures in the SUT.
const (
	tenantAlpha      = "e2e-alpha"
	tenantBeta       = "e2e-beta"
	tenantGamma      = "e2e-gamma"
	alphaAccount     = "tenant-alpha"
	betaAccount      = "tenant-beta"
	gammaAccount     = "tenant-gamma"
	outsiderAccount  = "tenant-outsider"
	tenantAudience   = "session-platform"
	tokenLifeSeconds = int64(600)
)

type tenantSession struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	WorkloadType string `json:"workloadType"`
	Tenant       string `json:"tenant"`
	State        string `json:"state"`
}

// tenantToken mints the same projected token a tenant's pod would mount. It
// needs the kind kubeconfig; a SUT reached without one cannot run these.
func tenantToken(t *testing.T, account, audience string) string {
	t.Helper()
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT: tenant tokens cannot be minted")
	}
	expiry := tokenLifeSeconds
	tr, err := cs.CoreV1().ServiceAccounts(sessionNamespace()).CreateToken(context.Background(), account,
		&authv1.TokenRequest{Spec: authv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &expiry}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("mint token for %s: %v", account, err)
	}
	return tr.Status.Token
}

// doAs is do with a bearer token; an empty token sends no Authorization header,
// which is the operator.
func doAs(t *testing.T, token, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL()+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, out
}

func createAs(t *testing.T, token string, body map[string]any) tenantSession {
	t.Helper()
	resp, raw := doAs(t, token, http.MethodPost, "/api/v1/sessions", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %v: status=%d body=%s", body, resp.StatusCode, raw)
	}
	var s tenantSession
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Cleanup(func() { doAs(t, "", http.MethodDelete, "/api/v1/sessions/"+s.ID, nil) })
	return s
}

func listAs(t *testing.T, token, query string) map[string]tenantSession {
	t.Helper()
	resp, raw := doAs(t, token, http.MethodGet, "/api/v1/sessions"+query, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list%s: status=%d body=%s", query, resp.StatusCode, raw)
	}
	var out struct {
		Sessions []tenantSession `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	byID := map[string]tenantSession{}
	for _, s := range out.Sessions {
		byID[s.ID] = s
	}
	return byID
}
