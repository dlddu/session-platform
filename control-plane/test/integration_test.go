//go:build integration

// Package integration is the opt-in happy-path harness (`make test-integration`).
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
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
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
	// in-process; only the pod orchestrator and the archive checkpointer remain
	// stubs.
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	ckpt := archive.NewStubCheckpointer(true)
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

// Snapshot here is a direct Service call — the public product transition is
// driven by the idle reaper.
func TestScenario4_ShellArchiveIntegrity(t *testing.T) {
	if os.Getenv("SHELL_ARCHIVE_ENABLED") != "1" {
		t.Skip("shell archive round trip not configured; set SHELL_ARCHIVE_ENABLED=1 with a reachable cluster")
	}
	svc := realService(t) // skips if no cluster or agent image is reachable
	ctx := context.Background()

	// Markers are produced by the shell at run time, so the PTY's echo of the
	// typed input can never match them.
	sess := createReal(t, svc, "shell-archive-integrity")
	writeShell(t, svc, sess.ID, "printf 'mark:%s\\n' frozen42 > ~/marker.txt\n")
	writeShell(t, svc, sess.ID, "export ARCHMARK=frozen42\n")
	writeShell(t, svc, sess.ID, "cd /tmp\n")
	writeShell(t, svc, sess.ID, "echo ready:$((40+2))\n")
	_, cursorBefore := eventuallyReadShell(t, svc, sess.ID, 0, func(p string) bool {
		return strings.Contains(p, "ready:42")
	})

	frozen, err := svc.Snapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if frozen.State != session.StateSnapshot || frozen.Pod != "" {
		t.Fatalf("after snapshot state=%q pod=%q, want snapshot with reclaimed pod", frozen.State, frozen.Pod)
	}

	writeShell(t, svc, sess.ID, "cat ~/marker.txt\n")
	writeShell(t, svc, sess.ID, "echo \"env:[$ARCHMARK]\"\n")
	writeShell(t, svc, sess.ID, "echo \"cwd:$(pwd)\"\n")

	delta, _ := eventuallyReadShell(t, svc, sess.ID, cursorBefore, func(p string) bool {
		return strings.Contains(p, "mark:frozen42") &&
			(strings.Contains(p, "env:[]") || strings.Contains(p, "env:[frozen42]")) &&
			strings.Contains(p, "cwd:/")
	})
	if !strings.Contains(delta, "env:[]") {
		t.Fatalf("restored shell kept $ARCHMARK; want a fresh shell (AC-D4): %q", delta)
	}
	if !strings.Contains(delta, "cwd:/session/state/home") {
		t.Fatalf("restored shell did not start in its home directory (AC-D4): %q", delta)
	}
	if strings.Contains(delta, "ready:42") {
		t.Fatalf("delta from the pre-freeze cursor replayed pre-freeze output %q; want the post-restore delta only (cursor continuity)", delta)
	}

	full, _ := eventuallyReadShell(t, svc, sess.ID, 0, func(p string) bool {
		return strings.Contains(p, "ready:42") && strings.Contains(p, "mark:frozen42")
	})
	if pre, post := strings.Index(full, "ready:42"), strings.Index(full, "mark:frozen42"); pre == -1 || post == -1 || pre > post {
		t.Fatalf("full history order broken across the freeze: pre=%d post=%d in %q", pre, post, full)
	}
}

func realService(t *testing.T) *service.Service {
	t.Helper()
	client, ns, err := k8s.BuildClient()
	if err != nil {
		t.Skipf("SHELL_ARCHIVE_ENABLED=1 but no reachable cluster (%v); the AC-D4 round trip needs real session pods", err)
	}
	image := os.Getenv("DATA_PLANE_IMAGE")
	if image == "" {
		t.Skip("SHELL_ARCHIVE_ENABLED=1 but DATA_PLANE_IMAGE unset; the round trip needs the published data plane agent image")
	}
	orch := k8s.NewClientOrchestrator(client, ns,
		k8s.WithImage(image), k8s.WithShell(os.Getenv("DATA_PLANE_SHELL")))
	store := configmap.NewStore(client, ns)
	ag := agent.NewHTTPClient(client, ns)
	// The in-memory store bridges the archive from checkpoint to restore inside
	// this process, so the round trip needs no S3 — that store is covered by unit
	// tests and the deployed control plane.
	ckpt := archive.NewAgentCheckpointer(ag, &memStore{blobs: map[string][]byte{}})
	return service.New(orch, store, ckpt, ag)
}

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

func createReal(t *testing.T, svc *service.Service, name string) *session.Session {
	t.Helper()
	sess, err := svc.Create(context.Background(), session.CreateRequest{Name: name})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { _ = svc.Terminate(context.Background(), sess.ID) })
	return sess
}

func writeShell(t *testing.T, svc *service.Service, id, payload string) {
	t.Helper()
	if _, err := svc.Write(context.Background(), id, payload); err != nil {
		t.Fatalf("write %q: %v", payload, err)
	}
}

// Shell output timing is non-deterministic, so assertions here are containment
// + eventually, never exact matches (mirrors the data plane agent's own tests).
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
