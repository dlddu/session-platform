package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func newServer() *httptest.Server {
	mgr := service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		criu.NewStubCheckpointer(false),
	)
	mux := http.NewServeMux()
	api.New(mgr).Routes(mux)
	return httptest.NewServer(mux)
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
