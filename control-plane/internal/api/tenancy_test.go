package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/tenancy"
)

// headerResolver stands in for TokenReview: X-Test-Tenant names the tenant a
// request speaks for, its absence is the operator.
type headerResolver struct{ cfg *tenancy.Config }

func (h headerResolver) Resolve(r *http.Request) (tenancy.Caller, error) {
	name := r.Header.Get("X-Test-Tenant")
	if name == "" {
		return tenancy.OperatorCaller, nil
	}
	t, ok := h.cfg.Lookup(name)
	if !ok {
		return tenancy.Caller{}, session.ErrForbidden
	}
	return tenancy.Caller{Tenant: t}, nil
}

func newTenantServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg, err := tenancy.Parse(`{"tenants":[
		{"name":"alpha","serviceAccounts":["alpha/sa"],"workloadTypes":["shell","codex"]},
		{"name":"beta","serviceAccounts":["beta/sa"],"workloadTypes":["claude-code"]},
		{"name":"omni","serviceAccounts":["omni/sa"],"workloadTypes":["*"]}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(
		api.WithTenancy(headerResolver{cfg}, cfg),
		api.WithOfferedWorkloadTypes(session.WorkloadTypeShell, session.WorkloadTypeClaudeCode, session.WorkloadTypeCodexChat),
	)
	t.Cleanup(srv.Close)
	return srv
}

func tenantCall(t *testing.T, srv *httptest.Server, method, path, tenant string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tenant != "" {
		req.Header.Set("X-Test-Tenant", tenant)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func mustCreate(t *testing.T, srv *httptest.Server, tenant string, body map[string]any) session.Session {
	t.Helper()
	resp, raw := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", tenant, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %v as %q: status %d %s", body, tenant, resp.StatusCode, raw)
	}
	var s session.Session
	_ = json.Unmarshal(raw, &s)
	return s
}

func listNames(t *testing.T, srv *httptest.Server, tenant, query string) []string {
	t.Helper()
	resp, raw := tenantCall(t, srv, http.MethodGet, "/api/v1/sessions"+query, tenant, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list as %q%s: %d %s", tenant, query, resp.StatusCode, raw)
	}
	var out struct{ Sessions []session.Session }
	_ = json.Unmarshal(raw, &out)
	names := []string{}
	for _, s := range out.Sessions {
		names = append(names, s.Name+"@"+s.Tenant)
	}
	return names
}

// AC-K2: a tenant creates into its own tenant and lists only its own
// sessions; the operator sees all and can filter.
func TestTenancyScopesCreateAndList(t *testing.T) {
	srv := newTenantServer(t)
	a := mustCreate(t, srv, "alpha", map[string]any{"name": "a1"})
	if a.Tenant != "alpha" {
		t.Fatalf("tenant session owner = %q, want alpha", a.Tenant)
	}
	mustCreate(t, srv, "beta", map[string]any{"name": "b1", "workloadType": "claude-code"})
	mustCreate(t, srv, "", map[string]any{"name": "op"})
	mustCreate(t, srv, "", map[string]any{"name": "a2", "tenant": "alpha"})

	if got := listNames(t, srv, "alpha", ""); len(got) != 2 {
		t.Errorf("alpha sees %v, want its two sessions only", got)
	}
	if got := listNames(t, srv, "beta", ""); len(got) != 1 || got[0] != "b1@beta" {
		t.Errorf("beta sees %v", got)
	}
	if got := listNames(t, srv, "", ""); len(got) != 4 {
		t.Errorf("operator sees %v, want all four", got)
	}
	if got := listNames(t, srv, "", "?tenant=alpha"); len(got) != 2 {
		t.Errorf("operator ?tenant=alpha sees %v", got)
	}
	if got := listNames(t, srv, "", "?tenant="); len(got) != 1 || got[0] != "op@" {
		t.Errorf("operator ?tenant= (untenanted) sees %v", got)
	}
	if resp, _ := tenantCall(t, srv, http.MethodGet, "/api/v1/sessions?tenant=beta", "alpha", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("tenant using the operator filter: %d, want 400", resp.StatusCode)
	}
}

// AC-K2: every session-id route answers another tenant's session as missing.
func TestTenancyHidesOtherTenantsSessions(t *testing.T) {
	srv := newTenantServer(t)
	b := mustCreate(t, srv, "beta", map[string]any{"name": "b1", "workloadType": "claude-code"})
	op := mustCreate(t, srv, "", map[string]any{"name": "op"})
	for _, id := range []string{b.ID, op.ID} {
		for _, r := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/sessions/" + id},
			{http.MethodPost, "/api/v1/sessions/" + id + "/read"},
			{http.MethodPost, "/api/v1/sessions/" + id + "/write"},
			{http.MethodPost, "/api/v1/sessions/" + id + "/switch"},
			{http.MethodPost, "/api/v1/sessions/" + id + "/snapshot"},
			{http.MethodGet, "/api/v1/sessions/" + id + "/stream"},
			{http.MethodDelete, "/api/v1/sessions/" + id},
		} {
			var body any
			if r.method == http.MethodPost {
				body = map[string]any{}
			}
			if resp, raw := tenantCall(t, srv, r.method, r.path, "alpha", body); resp.StatusCode != http.StatusNotFound {
				t.Errorf("alpha %s %s: %d %s, want 404", r.method, r.path, resp.StatusCode, raw)
			}
		}
	}
	if resp, _ := tenantCall(t, srv, http.MethodGet, "/api/v1/sessions/"+b.ID, "beta", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("beta reading its own session: %d", resp.StatusCode)
	}
	if resp, _ := tenantCall(t, srv, http.MethodGet, "/api/v1/sessions/"+b.ID, "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("operator reading beta's session: %d", resp.StatusCode)
	}
}

// AC-K3: the owning tenant must be granted the workload type, and a tenant
// cannot create into another tenant.
func TestTenancyCreateRules(t *testing.T) {
	srv := newTenantServer(t)
	cases := []struct {
		name   string
		tenant string
		body   map[string]any
		want   int
	}{
		{"tenant default shell granted", "alpha", map[string]any{"name": "x"}, http.StatusCreated},
		{"tenant type not granted", "alpha", map[string]any{"name": "x", "workloadType": "claude-code"}, http.StatusForbidden},
		{"tenant default shell not granted", "beta", map[string]any{"name": "x"}, http.StatusForbidden},
		{"tenant names itself", "alpha", map[string]any{"name": "x", "tenant": "alpha"}, http.StatusCreated},
		{"tenant names another", "alpha", map[string]any{"name": "x", "tenant": "beta"}, http.StatusForbidden},
		{"operator unknown tenant", "", map[string]any{"name": "x", "tenant": "gamma"}, http.StatusBadRequest},
		{"operator empty tenant", "", map[string]any{"name": "x", "tenant": ""}, http.StatusBadRequest},
		{"operator tenant type not granted", "", map[string]any{"name": "x", "tenant": "beta", "workloadType": "shell"}, http.StatusForbidden},
		{"operator untenanted any type", "", map[string]any{"name": "x", "workloadType": "claude-code"}, http.StatusCreated},
		{"wildcard tenant default shell", "omni", map[string]any{"name": "x"}, http.StatusCreated},
		{"wildcard tenant any type", "omni", map[string]any{"name": "x", "workloadType": "claude-code"}, http.StatusCreated},
		{"wildcard tenant unknown type", "omni", map[string]any{"name": "x", "workloadType": "vim"}, http.StatusBadRequest},
		{"operator into wildcard tenant", "", map[string]any{"name": "x", "tenant": "omni", "workloadType": "codex"}, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", tc.tenant, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d %s, want %d", resp.StatusCode, raw, tc.want)
			}
		})
	}
}

// AC-K3/AC-K4: /config shows a tenant only its granted types and the operator
// the tenant set to pick from.
func TestTenancyRuntimeConfig(t *testing.T) {
	srv := newTenantServer(t)
	type cfgResp struct {
		WorkloadTypes []struct{ Type string } `json:"workloadTypes"`
		Tenant        *struct {
			Name          string   `json:"name"`
			WorkloadTypes []string `json:"workloadTypes"`
		} `json:"tenant"`
		Tenants []struct {
			Name          string   `json:"name"`
			WorkloadTypes []string `json:"workloadTypes"`
		} `json:"tenants"`
	}
	decode := func(tenant string) cfgResp {
		resp, raw := tenantCall(t, srv, http.MethodGet, "/api/v1/config", tenant, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("config as %q: %d", tenant, resp.StatusCode)
		}
		var c cfgResp
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	alpha := decode("alpha")
	if alpha.Tenant == nil || alpha.Tenant.Name != "alpha" || len(alpha.Tenants) != 0 {
		t.Fatalf("alpha config tenant=%+v tenants=%+v", alpha.Tenant, alpha.Tenants)
	}
	if len(alpha.WorkloadTypes) != 2 || alpha.WorkloadTypes[0].Type != "shell" || alpha.WorkloadTypes[1].Type != "codex-chat" {
		t.Errorf("alpha workloadTypes = %+v, want shell, codex-chat", alpha.WorkloadTypes)
	}
	op := decode("")
	if op.Tenant != nil || len(op.Tenants) != 3 || len(op.WorkloadTypes) != 3 {
		t.Fatalf("operator config tenant=%+v tenants=%+v types=%+v", op.Tenant, op.Tenants, op.WorkloadTypes)
	}
	if op.Tenants[1].Name != "beta" || len(op.Tenants[1].WorkloadTypes) != 1 || op.Tenants[1].WorkloadTypes[0] != "claude-code" {
		t.Errorf("operator tenants = %+v", op.Tenants)
	}
	// "*" is never echoed: both sides see the offered types it expands to.
	if got := op.Tenants[2].WorkloadTypes; op.Tenants[2].Name != "omni" || len(got) != 3 || got[0] != "shell" || got[1] != "claude-code" || got[2] != "codex-chat" {
		t.Errorf("operator view of omni = %+v, want shell, claude-code, codex-chat", op.Tenants[2])
	}
	omni := decode("omni")
	if omni.Tenant == nil || len(omni.Tenant.WorkloadTypes) != 3 || len(omni.WorkloadTypes) != 3 {
		t.Errorf("omni config tenant=%+v types=%+v, want every offered type", omni.Tenant, omni.WorkloadTypes)
	}
}

// AC-K1: a refused caller never falls back to the operator.
func TestTenancyRefusedCaller(t *testing.T) {
	srv := newTenantServer(t)
	for _, path := range []string{"/api/v1/sessions", "/api/v1/config"} {
		if resp, _ := tenantCall(t, srv, http.MethodGet, path, "gamma", nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("unknown tenant GET %s: %d, want 403", path, resp.StatusCode)
		}
	}
}

// AC-K4: repo access management is the operator's; any bearer caller — a
// tenant or not — is refused before the route runs.
func TestRepoAccessAdminIsOperatorOnly(t *testing.T) {
	mux := http.NewServeMux()
	api.NewRepoAccess(nil, nil, nil, nil).Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/repo-access", nil)
	req.Header.Set("Authorization", "Bearer tenant-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer GET /repo-access = %d, want 403", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/api/v1/repo-access")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("operator GET /repo-access on a disabled deployment = %d, want 503 as before", resp.StatusCode)
	}
}
