package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-J7: POST /sessions/{id}/plugins/refresh is a codex session's own,
// answers with the session and what changed, and is scoped like every other
// session-id route — another tenant's session is not found.
func TestPluginRefreshEndpoint(t *testing.T) {
	srv := newTenantServer(t)

	resp, body := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", "alpha", map[string]string{"name": "long", "workloadType": "codex-chat"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create codex = %d %s", resp.StatusCode, body)
	}
	var codex session.Session
	if err := json.Unmarshal(body, &codex); err != nil {
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

	resp, body = tenantCall(t, srv, http.MethodPost, "/api/v1/sessions/"+codex.ID+"/plugins/refresh", "alpha", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh = %d %s", resp.StatusCode, body)
	}
	var got struct {
		Session  session.Session `json:"session"`
		Path     string          `json:"path"`
		State    string          `json:"state"`
		Added    []string        `json:"added"`
		Updated  []string        `json:"updated"`
		Removed  []string        `json:"removed"`
		Revision *string         `json:"revision"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Session.ID != codex.ID || got.Path != "active" || got.State != "unconfigured" ||
		got.Added == nil || got.Updated == nil || got.Removed == nil {
		t.Fatalf("refresh body = %s", body)
	}

	for _, tc := range []struct {
		name, id, tenant string
		body             any
		want             int
	}{
		{"another tenant", codex.ID, "beta", nil, http.StatusNotFound},
		{"unknown session", "nope", "", nil, http.StatusNotFound},
		{"not codex", shell.ID, "alpha", nil, http.StatusUnprocessableEntity},
		{"unknown field", codex.ID, "alpha", map[string]string{"force": "yes"}, http.StatusBadRequest},
	} {
		resp, body := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions/"+tc.id+"/plugins/refresh", tc.tenant, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: refresh = %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
		}
	}
	// The operator sees every tenant's sessions, and may refresh them.
	if resp, body := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions/"+codex.ID+"/plugins/refresh", "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("operator refresh = %d %s", resp.StatusCode, body)
	}
}
