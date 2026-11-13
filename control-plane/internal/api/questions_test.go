package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-E2: the question routes are a claude-code session's own and are scoped
// like every other session-id route.
func TestQuestionEndpoints(t *testing.T) {
	srv := newTenantServer(t)
	resp, body := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", "beta", map[string]string{"name": "cc", "workloadType": "claude-code"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create claude-code = %d %s", resp.StatusCode, body)
	}
	var claude session.Session
	if err := json.Unmarshal(body, &claude); err != nil {
		t.Fatal(err)
	}
	resp, body = tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", "alpha", map[string]string{"name": "sh"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create shell = %d %s", resp.StatusCode, body)
	}
	var shell session.Session
	if err := json.Unmarshal(body, &shell); err != nil {
		t.Fatal(err)
	}

	resp, body = tenantCall(t, srv, http.MethodGet, "/api/v1/sessions/"+claude.ID+"/questions", "beta", nil)
	if resp.StatusCode != http.StatusOK || string(body) != "{\"questions\":[]}\n" {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	answer := map[string]any{"answers": map[string]string{"Which?": "A"}}
	for _, tc := range []struct {
		name, method, path, tenant string
		body                       any
		want                       int
	}{
		{"list, another tenant", http.MethodGet, "/api/v1/sessions/" + claude.ID + "/questions", "alpha", nil, http.StatusNotFound},
		{"list, not claude-code", http.MethodGet, "/api/v1/sessions/" + shell.ID + "/questions", "alpha", nil, http.StatusUnprocessableEntity},
		{"answer, nothing waits", http.MethodPost, "/api/v1/sessions/" + claude.ID + "/questions/q_1/answer", "beta", answer, http.StatusNotFound},
		{"answer, another tenant", http.MethodPost, "/api/v1/sessions/" + claude.ID + "/questions/q_1/answer", "alpha", answer, http.StatusNotFound},
		{"answer, no body", http.MethodPost, "/api/v1/sessions/" + claude.ID + "/questions/q_1/answer", "beta", nil, http.StatusBadRequest},
		{"answer, unknown field", http.MethodPost, "/api/v1/sessions/" + claude.ID + "/questions/q_1/answer", "beta", map[string]any{"answer": "A"}, http.StatusBadRequest},
		{"answer, not claude-code", http.MethodPost, "/api/v1/sessions/" + shell.ID + "/questions/q_1/answer", "alpha", answer, http.StatusUnprocessableEntity},
	} {
		resp, body := tenantCall(t, srv, tc.method, tc.path, tc.tenant, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
		}
	}
}
