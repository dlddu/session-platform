package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"k8s.io/client-go/kubernetes/fake"
)

// steerServer is the API with a stub agent the test can steer the answer of.
func steerServer(t *testing.T) (*httptest.Server, *agent.StubClient) {
	t.Helper()
	orch := k8s.NewStubOrchestrator("sessions")
	ckpt := archive.NewStubCheckpointer(true)
	stub := agent.NewStubClient()
	mgr := service.New(orch, configmap.NewStore(fake.NewSimpleClientset(), "sessions"), ckpt, stub)
	mux := http.NewServeMux()
	api.New(mgr).Routes(mux)
	return httptest.NewServer(mux), stub
}

func steer(t *testing.T, url, id string, body map[string]any) (int, session.Steered) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+"/api/v1/sessions/"+id+"/steer", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("steer: %v", err)
	}
	defer resp.Body.Close()
	var out session.Steered
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// AC-J9: a codex session's running turn takes the message, and the answer
// says applied — 200, with the turn it landed in.
func TestSteerSession_AppliedReachesThePod(t *testing.T) {
	srv, stub := steerServer(t)
	defer srv.Close()
	_, s := createRaw(t, srv.URL, map[string]any{"name": "steer-ok", "workloadType": "codex-chat"})

	status, out := steer(t, srv.URL, s.ID, map[string]any{"text": "  focus on the newer paper  "})
	if status != http.StatusOK || !out.Applied() {
		t.Fatalf("steer = %d %+v, want 200 applied", status, out)
	}
	if got := stub.Steered(s.Pod); len(got) != 1 || got[0] != "focus on the newer paper" {
		t.Fatalf("pod got %q, want the trimmed message once", got)
	}
}

// A message codex refused is a rejection the caller can act on: 409 with the
// reason, and never reported as delivered.
func TestSteerSession_RejectedIsNotDelivery(t *testing.T) {
	srv, stub := steerServer(t)
	defer srv.Close()
	stub.SteerOutcome = &session.Steered{Status: "rejected", Reason: "expectedTurnId does not match the active turn"}
	_, s := createRaw(t, srv.URL, map[string]any{"name": "steer-no", "workloadType": "codex-chat"})

	status, out := steer(t, srv.URL, s.ID, map[string]any{"text": "too late"})
	if status != http.StatusConflict || out.Applied() {
		t.Fatalf("steer = %d %+v, want 409 rejected", status, out)
	}
	if out.Reason == "" {
		t.Fatal("a rejection must carry its reason")
	}
}

// Only the codex workloads take free-text mid-turn messages; claude-code's
// mid-turn surface is AC-E2's questions and tool approvals.
func TestSteerSession_WrongWorkloadAndEmptyText(t *testing.T) {
	srv, _ := steerServer(t)
	defer srv.Close()
	_, claude := createRaw(t, srv.URL, map[string]any{"name": "steer-cc", "workloadType": "claude-code"})
	// ErrInvalidState: the session exists but its type has no such surface.
	if status, _ := steer(t, srv.URL, claude.ID, map[string]any{"text": "hi"}); status != http.StatusUnprocessableEntity {
		t.Fatalf("steer a claude-code session = %d, want 422", status)
	}
	_, codex := createRaw(t, srv.URL, map[string]any{"name": "steer-empty", "workloadType": "codex-chat"})
	if status, _ := steer(t, srv.URL, codex.ID, map[string]any{"text": "   "}); status != http.StatusBadRequest {
		t.Fatalf("steer with no text = %d, want 400", status)
	}
	if status, _ := steer(t, srv.URL, "no-such-session", map[string]any{"text": "hi"}); status != http.StatusNotFound {
		t.Fatalf("steer an unknown session = %d, want 404", status)
	}
}
