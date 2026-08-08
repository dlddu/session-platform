package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func newServer(opts ...api.Option) *httptest.Server {
	srv, _ := newServerWithOrchestrator(opts...)
	return srv
}

func newServerWithOrchestrator(opts ...api.Option) (*httptest.Server, *k8s.StubOrchestrator) {
	orch := k8s.NewStubOrchestrator("sessions")
	ckpt := criu.NewStubCheckpointer(true)
	mgr := service.New(
		orch,
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		ckpt,
		agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	mux := http.NewServeMux()
	api.New(mgr, opts...).Routes(mux)
	return httptest.NewServer(mux), orch
}

// createForTest creates a session through the API and returns it.
func createForTest(t *testing.T, srv *httptest.Server, name string) session.Session {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var s session.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode created session: %v", err)
	}
	return s
}

// The snapshot endpoint is test-only: it exists to let the e2e suite reach the
// snapshot state (the product trigger policy is still undecided), so it must be
// absent unless WithTestEndpoints is set.
func TestSnapshotEndpointIsGated(t *testing.T) {
	// Default (production) surface: no snapshot route.
	srv := newServer()
	defer srv.Close()
	s := createForTest(t, srv, "gated-off")
	resp, err := http.Post(srv.URL+"/api/v1/sessions/"+s.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("snapshot (gate off): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("snapshot with the gate off = %d, want 404 (route must not exist)", resp.StatusCode)
	}

	// With test endpoints on: the session freezes and its pod is reclaimed.
	srvTest := newServer(api.WithTestEndpoints(true))
	defer srvTest.Close()
	s2 := createForTest(t, srvTest, "gated-on")
	resp2, err := http.Post(srvTest.URL+"/api/v1/sessions/"+s2.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("snapshot (gate on): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("snapshot with the gate on = %d, want 200", resp2.StatusCode)
	}
	var frozen session.Session
	if err := json.NewDecoder(resp2.Body).Decode(&frozen); err != nil {
		t.Fatalf("decode snapshot response: %v", err)
	}
	if frozen.State != session.StateSnapshot {
		t.Errorf("state = %q, want snapshot", frozen.State)
	}
	if frozen.Pod != "" {
		t.Errorf("pod = %q, want it reclaimed (AC-A3)", frozen.Pod)
	}
}

// A snapshot of an unknown session is a 404 like the other handlers.
func TestSnapshotUnknownSession(t *testing.T) {
	srv := newServer(api.WithTestEndpoints(true))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/v1/sessions/nope/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("snapshot of an unknown session = %d, want 404", resp.StatusCode)
	}
}

// TestHappyPath exercises create -> list -> switch end-to-end through the HTTP
// surface with stub adapters (AC-A1/A2 create, V5 list, AC-C4 switch).
func TestHappyPath(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	// create
	body, _ := json.Marshal(map[string]string{"name": "api-gateway-dev"})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created session.Session
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	resp.Body.Close()
	if created.State != session.StateActive {
		t.Errorf("new session state = %q, want active", created.State)
	}
	if created.Pod == "" {
		t.Error("new session should have a pod assigned (AC-A2)")
	}

	// list
	resp, err = http.Get(srv.URL + "/api/v1/sessions")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(listed.Sessions) != 1 {
		t.Fatalf("list len = %d, want 1", len(listed.Sessions))
	}

	// switch (active -> active no-op)
	resp, err = http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/switch", "application/json", nil)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestReadWriteWireContract drives write→read over HTTP: the write payload
// comes back from the (stub) agent and read honours the offset/nextOffset
// cursor contract (AC-D2/D3 wire shape).
func TestReadWriteWireContract(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"name": "shell-wire"})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created session.Session
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	resp.Body.Close()

	wbody, _ := json.Marshal(map[string]string{"payload": "echo hi\n"})
	resp, err = http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/write", "application/json", bytes.NewReader(wbody))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status = %d, want 200", resp.StatusCode)
	}

	readAt := func(offset int64) (string, int64) {
		t.Helper()
		rbody, _ := json.Marshal(map[string]int64{"offset": offset})
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/read", "application/json", bytes.NewReader(rbody))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("read status = %d, want 200", resp.StatusCode)
		}
		var out struct {
			Payload    string `json:"payload"`
			NextOffset int64  `json:"nextOffset"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode read: %v", err)
		}
		return out.Payload, out.NextOffset
	}

	payload, next := readAt(0)
	if payload != "echo hi\n" {
		t.Fatalf("read payload = %q, want the written payload", payload)
	}
	if next != int64(len("echo hi\n")) {
		t.Fatalf("nextOffset = %d, want %d", next, len("echo hi\n"))
	}
	if delta, _ := readAt(next); delta != "" {
		t.Fatalf("cursor read = %q, want empty delta", delta)
	}

	// A negative offset is invalid input.
	rbody, _ := json.Marshal(map[string]int64{"offset": -1})
	resp, err = http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/read", "application/json", bytes.NewReader(rbody))
	if err != nil {
		t.Fatalf("read -1: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative offset status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateValidation(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{"name": ""})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty-name create status = %d, want 400", resp.StatusCode)
	}
}

func TestGetUnknownReturns404(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/sessions/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown get status = %d, want 404", resp.StatusCode)
	}
}
