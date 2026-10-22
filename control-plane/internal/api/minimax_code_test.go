package api_test

import (
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-G1: the create wire contract accepts minimax-code and applies the model
// rules of AC-E6 to it, the same as for the other agent types.
func TestCreateSessionMiniMaxCode(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	status, s := createRaw(t, srv.URL, map[string]any{"name": "mm-default", "workloadType": "minimax-code"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	if s.WorkloadType != session.WorkloadTypeMiniMaxCode || s.Model != session.PlatformDefaultModel {
		t.Errorf("session = %s/%s, want minimax-code/%s", s.WorkloadType, s.Model, session.PlatformDefaultModel)
	}
	if got := orch.WorkloadFor(s.ID); got != session.WorkloadTypeMiniMaxCode {
		t.Errorf("provisioned for %q, want minimax-code", got)
	}

	status, s = createRaw(t, srv.URL, map[string]any{"name": "mm-model", "workloadType": "minimax-code", "model": "MiniMax-M3"})
	if status != http.StatusCreated || s.Model != "MiniMax-M3" {
		t.Fatalf("explicit model: status %d, model %q; want 201 and MiniMax-M3", status, s.Model)
	}

	if status, _ := createRaw(t, srv.URL, map[string]any{"name": "mm-bad", "workloadType": "minimax-code", "model": " spaced "}); status != http.StatusBadRequest {
		t.Errorf("model with surrounding whitespace: status %d, want 400", status)
	}
}
