//go:build integration

// Package integration is the opt-in happy-path harness (`make test-integration`).
//
// For the scaffolding it drives the control plane through its HTTP surface with
// the in-memory stub adapters, asserting the create/list/switch/terminate
// happy path and the session<->pod 1:1 mapping (test scenarios 1–3 in
// docs/test/architecture.md).
//
// The REAL harness this replaces will, before these assertions:
//   - bring up a kind cluster (deploy/kind-config.yaml),
//   - point the control plane's k8s + ConfigMap state-store adapters at it,
//   - and assert against actual pods.
//
// That wiring is intentionally deferred; see docs/criu-verification.md for the
// CRIU scenario, which stays skipped until a verified runtime exists.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func harness(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()
	// TODO(kind): replace the stubs below with adapters pointed at the kind
	// cluster brought up for the integration job. The ConfigMap state store
	// already runs against a fake clientset here, so its contract is exercised
	// in-process; only the pod orchestrator and CRIU remain stubs.
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := criu.NewStubCheckpointer(os.Getenv("CRIU_ENABLED") == "1")
	svc := service.New(orch, store, ckpt, agent.NewStubClient())

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

// Scenario 4 (AC-D4, concretising AC-B3): a session's shell state — here an env
// var and the working directory — survives a snapshot→restore round-trip, and
// the read cursor issued before the freeze stays valid afterwards (the
// scrollback rides the checkpoint, buffer-in-checkpoint).
//
// Unlike scenarios 1–3 this cannot run on the in-memory stubs: the marker
// round-trip (`echo $CRIUMARK` → the frozen value) needs a *real* shell frozen
// and thawed by a real CRIU runtime, so it is wired against the real
// cluster-backed Service (the same adapters main builds). It therefore runs only
// where CRIU_ENABLED=1 AND a CRIU-capable runtime is reachable; runtime-less CI
// skips (the gate is off there). Snapshot is a direct Service call — J5-S4 adds
// no HTTP snapshot endpoint. Where a CRIU runtime exists but the restore path
// isn't fully wired yet, this FAILS rather than skips: that is the intended
// "finish wiring the restore mechanism" signal for the provisioning work (see
// docs/criu-verification.md).
func TestScenario4_CRIUIntegrity(t *testing.T) {
	if os.Getenv("CRIU_ENABLED") != "1" {
		t.Skip("CRIU verification environment not configured; see docs/criu-verification.md")
	}
	svc := realService(t) // skips if no CRIU-capable runtime is reachable
	ctx := context.Background()

	// Set shell state that must survive the freeze (AC-D4 markers): an env var
	// and the working directory. export/cd emit nothing to stdout — the PTY
	// echoes the typed input — so anchor the pre-freeze cursor on that echo.
	sess := createReal(t, svc, "criu-integrity")
	writeShell(t, svc, sess.ID, "export CRIUMARK=frozen42\n")
	writeShell(t, svc, sess.ID, "cd /tmp\n")
	_, cursorBefore := eventuallyReadShell(t, svc, sess.ID, 0, func(p string) bool {
		return strings.Contains(p, "CRIUMARK=frozen42")
	})

	// Freeze in-process, reclaiming the pod (AC-B1/AC-A3).
	frozen, err := svc.Snapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if frozen.State != session.StateSnapshot || frozen.Pod != "" {
		t.Fatalf("after snapshot state=%q pod=%q, want snapshot with reclaimed pod", frozen.State, frozen.Pod)
	}

	// Accessing the frozen session restores the checkpoint into a new pod (AC-B2)
	// and runs on top of the frozen context: $CRIUMARK and the cwd must be
	// exactly as before the freeze.
	writeShell(t, svc, sess.ID, "echo restored:$CRIUMARK\n")
	writeShell(t, svc, sess.ID, "pwd\n")

	// AC-D4 + cursor continuity: read the delta from the *pre-freeze* cursor.
	// That the cursor is still a valid offset into the restored buffer (not past
	// its end) is the buffer-in-checkpoint guarantee. The delta proves the frozen
	// env var ($CRIUMARK → frozen42) and cwd (pwd → /tmp) came back intact.
	delta, _ := eventuallyReadShell(t, svc, sess.ID, cursorBefore, func(p string) bool {
		return strings.Contains(p, "restored:frozen42") && strings.Contains(p, "/tmp")
	})
	if strings.Contains(delta, "export CRIUMARK") {
		t.Fatalf("delta from the pre-freeze cursor replayed pre-freeze input %q; want the post-restore delta only (cursor continuity)", delta)
	}

	// offset 0 still replays the whole history across the freeze — pre-freeze
	// input echo and post-restore output both present, in order (non-consuming).
	full, _ := eventuallyReadShell(t, svc, sess.ID, 0, func(p string) bool {
		return strings.Contains(p, "CRIUMARK=frozen42") && strings.Contains(p, "restored:frozen42")
	})
	if pre, post := strings.Index(full, "CRIUMARK=frozen42"), strings.Index(full, "restored:frozen42"); pre == -1 || post == -1 || pre > post {
		t.Fatalf("full history order broken across the freeze: pre=%d post=%d in %q", pre, post, full)
	}
}

// realService builds the real, cluster-backed Service (the same adapters main
// wires) so Scenario 4 exercises a genuine snapshot→restore against a CRIU
// runtime. It skips — never fails — when the runtime is absent, so runtime-less
// CI stays green while a provisioned environment runs the assertions.
func realService(t *testing.T) *service.Service {
	t.Helper()
	client, ns, err := k8s.BuildClient()
	if err != nil {
		t.Skipf("CRIU_ENABLED=1 but no reachable cluster (%v); the AC-D4 round-trip needs a CRIU-capable runtime — see docs/criu-verification.md", err)
	}
	image := os.Getenv("DATA_PLANE_IMAGE")
	if image == "" {
		t.Skip("CRIU_ENABLED=1 but DATA_PLANE_IMAGE unset; the round-trip needs the published data plane agent image")
	}
	orch := k8s.NewClientOrchestrator(client, ns,
		k8s.WithImage(image), k8s.WithShell(os.Getenv("DATA_PLANE_SHELL")),
		k8s.WithCheckpointPrivileged(true))
	store := configmap.NewStore(client, ns)
	ag := agent.NewHTTPClient(client, ns)
	// Agent-driven checkpointer (the wired path). An in-memory store bridges the
	// archive from checkpoint to restore within this test process, so the AC-D4
	// round trip is verified without needing S3 — the S3 store is covered by unit
	// tests and the deployed control plane.
	ckpt := criu.NewAgentCheckpointer(ag, &memStore{blobs: map[string][]byte{}})
	return service.New(orch, store, ckpt, ag)
}

// memStore is an in-process CheckpointStore for the CRIU round-trip test: it
// keeps archives in memory between checkpoint and restore.
type memStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.blobs[key] = b
	m.mu.Unlock()
	return "mem://" + key, nil
}

func (m *memStore) Get(_ context.Context, ref string) (io.ReadCloser, error) {
	m.mu.Lock()
	b, ok := m.blobs[strings.TrimPrefix(ref, "mem://")]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("mem store: %s not found", ref)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// createReal creates a session through the real Service and schedules its
// teardown (pod + state reclaimed) at test end regardless of outcome.
func createReal(t *testing.T, svc *service.Service, name string) *session.Session {
	t.Helper()
	sess, err := svc.Create(context.Background(), session.CreateRequest{Name: name})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { _ = svc.Terminate(context.Background(), sess.ID) })
	return sess
}

// writeShell injects a line into the session shell's stdin (AC-D2), restoring or
// promoting the session first per the uniform resume-on-access rule.
func writeShell(t *testing.T, svc *service.Service, id, payload string) {
	t.Helper()
	if _, err := svc.Write(context.Background(), id, payload); err != nil {
		t.Fatalf("write %q: %v", payload, err)
	}
}

// eventuallyReadShell polls Read(offset) until the payload satisfies ok, failing
// after a deadline. Shell output timing is non-deterministic, so assertions are
// containment + eventually, never exact matches (mirrors the data plane agent's
// own tests).
func eventuallyReadShell(t *testing.T, svc *service.Service, id string, offset int64, ok func(string) bool) (string, int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var payload string
	var next int64
	for time.Now().Before(deadline) {
		res, err := svc.Read(context.Background(), id, offset)
		if err != nil {
			t.Fatalf("read at offset %d: %v", offset, err)
		}
		payload, next = res.Payload, res.NextOffset
		if ok(payload) {
			return payload, next
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("read at offset %d never satisfied the condition; last payload=%q", offset, payload)
	return "", 0
}
