//go:build integration

// Package integration is the opt-in happy-path harness (`make test-integration`).
//
// For the scaffolding it drives the control plane through its HTTP surface with
// the in-memory stub adapters, asserting the create/list/switch/terminate
// happy path and the session<->pod 1:1 mapping (test scenarios 1–3 in
// docs/test/architecture.md).
//
// The REAL harness this replaces will, before these assertions:
//   - bring up a kind cluster (deploy/kind-config.yaml) and a Redis
//     (deploy/ overlay over k8s/redis.yaml),
//   - point the control plane's k8s + redis adapters at them,
//   - and assert against actual pods.
//
// That wiring is intentionally deferred; see docs/criu-verification.md for the
// CRIU scenario, which stays skipped until a verified runtime exists.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/redis"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func harness(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()
	// TODO(kind+redis): replace the stubs below with adapters pointed at the
	// kind cluster and Redis brought up for the integration job.
	orch := k8s.NewStubOrchestrator("sessions")
	store := redis.NewStubStore(os.Getenv("REDIS_ADDR"))
	ckpt := criu.NewStubCheckpointer(os.Getenv("CRIU_ENABLED") == "1")
	svc := service.New(orch, store, ckpt)

	mux := http.NewServeMux()
	api.New(svc).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, svc
}

func create(t *testing.T, srv *httptest.Server, name string) session.Session {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s status=%d", name, resp.StatusCode)
	}
	var s session.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return s
}

// Scenario 1 (AC-A1): creating a session provisions a dedicated data plane pod.
func TestScenario1_CreateProvisionsPod(t *testing.T) {
	srv, _ := harness(t)
	s := create(t, srv, "api-gateway-dev")
	if s.State != session.StateActive {
		t.Fatalf("state=%q want active", s.State)
	}
	if s.Pod == "" {
		t.Fatal("expected a dedicated pod (AC-A1/A2)")
	}
}

// Scenario 2 (AC-A2): N sessions => N unique pods; terminating one doesn't
// affect the others.
func TestScenario2_OneToOneMappingAndIsolation(t *testing.T) {
	srv, svc := harness(t)
	a := create(t, srv, "s-a")
	b := create(t, srv, "s-b")
	c := create(t, srv, "s-c")

	pods := map[string]bool{a.Pod: true, b.Pod: true, c.Pod: true}
	if len(pods) != 3 {
		t.Fatalf("expected 3 unique pods, got %v", pods)
	}

	if err := svc.Terminate(context.Background(), b.ID); err != nil {
		t.Fatalf("terminate b: %v", err)
	}
	for _, id := range []string{a.ID, c.ID} {
		if _, err := svc.Get(context.Background(), id); err != nil {
			t.Fatalf("session %s affected by terminating b: %v", id, err)
		}
	}
}

// Scenario 3 (AC-A3): snapshotting reclaims the pod.
func TestScenario3_SnapshotReclaimsPod(t *testing.T) {
	srv, svc := harness(t)
	s := create(t, srv, "model-train")
	frozen, err := svc.Snapshot(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if frozen.Pod != "" {
		t.Fatal("expected pod reclaimed after snapshot (AC-A3)")
	}
}

// CRIU integrity (AC-B3) stays skipped until a verified CRIU runtime exists.
func TestScenario4_CRIUIntegrity(t *testing.T) {
	if os.Getenv("CRIU_ENABLED") != "1" {
		t.Skip("CRIU verification environment not configured; see docs/criu-verification.md")
	}
	t.Fatal("CRIU integrity assertions not implemented — gate is on but runtime is unverified")
}
