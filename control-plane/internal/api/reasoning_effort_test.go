package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/api"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-E8: the reasoning effort's wire contract — echoed and copied to the pod
// when the type lists it, refused before provisioning otherwise.
func TestCreateSessionReasoningEffort(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	status, s := createRaw(t, srv.URL, map[string]any{"name": "e1", "workloadType": "claude-code", "reasoningEffort": "max"})
	if status != http.StatusCreated || s.ReasoningEffort != "max" || orch.SpecFor(s.ID).ReasoningEffort != "max" {
		t.Fatalf("claude-code max: status %d, session %q, pod %q", status, s.ReasoningEffort, orch.SpecFor(s.ID).ReasoningEffort)
	}
	status, s = createRaw(t, srv.URL, map[string]any{"name": "e2", "workloadType": "claude-code"})
	if status != http.StatusCreated || s.ReasoningEffort != "" {
		t.Fatalf("omitted effort: status %d, effort %q", status, s.ReasoningEffort)
	}
	before := orch.RunningCount()
	for _, body := range []map[string]any{
		{"name": "x", "workloadType": "claude-code", "reasoningEffort": ""},
		{"name": "x", "workloadType": "claude-code", "reasoningEffort": nil},
		{"name": "x", "workloadType": "claude-code", "reasoningEffort": 3},
		{"name": "x", "workloadType": "claude-code", "reasoningEffort": "minimal"},
		{"name": "x", "workloadType": "codex-task", "reasoningEffort": "max"},
		{"name": "x", "workloadType": "minimax-code", "reasoningEffort": "high"},
		{"name": "x", "workloadType": "codex-chat", "reasoningEffort": "high"},
		{"name": "x", "reasoningEffort": "high"},
	} {
		if status, _ := createRaw(t, srv.URL, body); status != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", body, status)
		}
	}
	if after := orch.RunningCount(); after != before {
		t.Fatalf("refused creates started pods: %d -> %d", before, after)
	}
}

// AC-J1: every caller moved to codex-chat, so a create naming the old type is
// refused before any pod exists.
func TestCreateSessionLegacyCodexNameRefused(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()
	before := orch.RunningCount()
	status, _ := createRaw(t, srv.URL, map[string]any{"name": "old-client", "workloadType": "codex"})
	if status != http.StatusBadRequest || orch.RunningCount() != before {
		t.Fatalf("legacy codex: status %d, starts %d -> %d", status, before, orch.RunningCount())
	}
}

// AC-N2: a full pool is 503 with only the error class on the wire.
func TestCreateSessionAuthPoolExhausted(t *testing.T) {
	ckpt := archive.NewStubCheckpointer(true)
	mgr := service.New(k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"), ckpt, agent.NewStubClient(),
		service.WithAuthPool(session.WorkloadTypeCodexTask, k8s.NewMemoryAuthPool(1)))
	mux := http.NewServeMux()
	api.New(mgr).Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if status, _ := createRaw(t, srv.URL, map[string]any{"name": "first", "workloadType": "codex-task"}); status != http.StatusCreated {
		t.Fatalf("first create status %d", status)
	}
	resp, err := http.Post(srv.URL+"/api/v1/sessions", "application/json",
		strings.NewReader(`{"name":"second","workloadType":"codex-task"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != http.StatusServiceUnavailable || body.Error != "auth pool exhausted" {
		t.Fatalf("second create = %d %s, want 503 auth pool exhausted", resp.StatusCode, raw)
	}
}
