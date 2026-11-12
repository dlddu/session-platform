package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/tenancy"
)

type apiManifests struct {
	mu   sync.Mutex
	byID map[string]session.Manifest
}

func (m *apiManifests) PutManifest(_ context.Context, man session.Manifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[man.ID] = man
	return nil
}

func (m *apiManifests) GetManifest(_ context.Context, id string) (*session.Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	man, ok := m.byID[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	return &man, nil
}

func newRecoverServers(t *testing.T, tenantConfigs ...string) []*httptest.Server {
	t.Helper()
	ckpt := archive.NewStubCheckpointer(true)
	mgr := service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		ckpt,
		agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
		service.WithSessionManifests(&apiManifests{byID: map[string]session.Manifest{}}),
	)
	var servers []*httptest.Server
	for _, raw := range tenantConfigs {
		cfg, err := tenancy.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		api.New(mgr, api.WithTenancy(headerResolver{cfg}, cfg)).Routes(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		servers = append(servers, srv)
	}
	return servers
}

const recoverTenants = `{"tenants":[
	{"name":"alpha","serviceAccounts":["alpha/sa"],"workloadTypes":["shell","claude-code"]},
	{"name":"beta","serviceAccounts":["beta/sa"],"workloadTypes":["*"]}
]}`

func freezeAndDelete(t *testing.T, srv *httptest.Server, tenant string, body map[string]any) session.Session {
	t.Helper()
	s := mustCreate(t, srv, tenant, body)
	if resp, raw := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions/"+s.ID+"/snapshot", tenant, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot: %d %s", resp.StatusCode, raw)
	}
	if resp, raw := tenantCall(t, srv, http.MethodDelete, "/api/v1/sessions/"+s.ID, tenant, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, raw)
	}
	return s
}

// AC-B4 with AC-K2: a deleted session comes back for its own tenant and the
// operator, and stays invisible to everyone else.
func TestRecoverSessionRoute(t *testing.T) {
	srv := newRecoverServers(t, recoverTenants)[0]
	gone := freezeAndDelete(t, srv, "alpha", map[string]any{"name": "a1"})
	path := "/api/v1/sessions/" + gone.ID + "/recover"

	if resp, raw := tenantCall(t, srv, http.MethodPost, path, "beta", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other tenant recover: %d %s, want 404", resp.StatusCode, raw)
	}
	if resp, raw := tenantCall(t, srv, http.MethodGet, "/api/v1/sessions/"+gone.ID, "alpha", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rejected recover left a record: %d %s", resp.StatusCode, raw)
	}
	resp, raw := tenantCall(t, srv, http.MethodPost, path, "alpha", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner recover: %d %s", resp.StatusCode, raw)
	}
	var got session.Session
	_ = json.Unmarshal(raw, &got)
	if got.ID != gone.ID || got.State != session.StateActive || got.Tenant != "alpha" || got.Name != "a1" {
		t.Fatalf("recovered = %+v", got)
	}
	if resp, raw := tenantCall(t, srv, http.MethodPost, path, "alpha", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("recover of a live id: %d %s, want 409", resp.StatusCode, raw)
	}
	if resp, raw := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions/0123456789abcdef0123456789abcdef/recover", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: %d %s, want 404", resp.StatusCode, raw)
	}
	if resp, raw := tenantCall(t, srv, http.MethodPost, path, "alpha", map[string]any{"x": 1}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-empty body: %d %s, want 400", resp.StatusCode, raw)
	}

	opGone := freezeAndDelete(t, srv, "beta", map[string]any{"name": "b1"})
	if resp, raw := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions/"+opGone.ID+"/recover", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator recover of a tenant session: %d %s", resp.StatusCode, raw)
	}
}

// AC-B4 with AC-K3: recovery provisions a pod, so the owning tenant must still
// be granted the session's workload type.
func TestRecoverSessionRouteRechecksTheTypeGrant(t *testing.T) {
	servers := newRecoverServers(t, recoverTenants, `{"tenants":[
		{"name":"alpha","serviceAccounts":["alpha/sa"],"workloadTypes":["shell"]}
	]}`)
	before, after := servers[0], servers[1]
	gone := freezeAndDelete(t, before, "alpha", map[string]any{"name": "agent", "workloadType": "claude-code"})
	path := "/api/v1/sessions/" + gone.ID + "/recover"
	if resp, raw := tenantCall(t, after, http.MethodPost, path, "alpha", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("recover after the grant was narrowed: %d %s, want 403", resp.StatusCode, raw)
	}
	if resp, raw := tenantCall(t, after, http.MethodPost, path, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator recover: %d %s", resp.StatusCode, raw)
	}
}
