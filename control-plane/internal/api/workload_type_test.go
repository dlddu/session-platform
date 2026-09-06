package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// createRaw POSTs an arbitrary create body and returns the status plus the
// decoded session (zero value when the request was rejected).
func createRaw(t *testing.T, url string, body map[string]any) (int, session.Session) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+"/api/v1/sessions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	var s session.Session
	if resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			t.Fatalf("decode created session: %v", err)
		}
	}
	return resp.StatusCode, s
}

// AC-E1/AC-E6: the create wire contract validates immutable workload settings,
// echoes their normalized values, and copies them to provisioning.
func TestCreateSessionWorkloadType(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	t.Run("omitted defaults to shell", func(t *testing.T) {
		status, s := createRaw(t, srv.URL, map[string]any{"name": "wt-default"})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.WorkloadType != session.WorkloadTypeShell {
			t.Errorf("workloadType = %q, want %q", s.WorkloadType, session.WorkloadTypeShell)
		}
		if s.Model != "" || orch.ModelFor(s.ID) != "" {
			t.Errorf("shell model = %q, orchestrator model = %q, want both empty", s.Model, orch.ModelFor(s.ID))
		}
	})

	t.Run("explicit shell", func(t *testing.T) {
		status, s := createRaw(t, srv.URL, map[string]any{"name": "wt-shell", "workloadType": "shell"})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.WorkloadType != session.WorkloadTypeShell {
			t.Errorf("workloadType = %q, want %q", s.WorkloadType, session.WorkloadTypeShell)
		}
		if got := orch.ModelFor(s.ID); got != "" {
			t.Errorf("orchestrator model = %q, want empty", got)
		}
	})

	t.Run("claude-code defaults to platform model", func(t *testing.T) {
		status, s := createRaw(t, srv.URL, map[string]any{"name": "wt-cc", "workloadType": "claude-code"})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.WorkloadType != session.WorkloadTypeClaudeCode {
			t.Errorf("workloadType = %q, want %q", s.WorkloadType, session.WorkloadTypeClaudeCode)
		}
		if s.Model != session.PlatformDefaultModel {
			t.Errorf("model = %q, want %q", s.Model, session.PlatformDefaultModel)
		}
		if got := orch.ModelFor(s.ID); got != session.PlatformDefaultModel {
			t.Errorf("orchestrator model = %q, want %q", got, session.PlatformDefaultModel)
		}
	})

	t.Run("explicit claude-code model", func(t *testing.T) {
		const model = "~anthropic/claude-opus-latest"
		status, s := createRaw(t, srv.URL, map[string]any{
			"name": "wt-cc-model", "workloadType": "claude-code", "model": model,
		})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.Model != model {
			t.Errorf("model = %q, want %q", s.Model, model)
		}
		if got := orch.ModelFor(s.ID); got != model {
			t.Errorf("orchestrator model = %q, want %q", got, model)
		}
	})

	t.Run("approval-gated defaults to platform model", func(t *testing.T) {
		status, s := createRaw(t, srv.URL, map[string]any{"name": "wt-ag", "workloadType": "approval-gated"})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.WorkloadType != session.WorkloadTypeApprovalGated {
			t.Errorf("workloadType = %q, want %q", s.WorkloadType, session.WorkloadTypeApprovalGated)
		}
		// AC-F1: the model contract is AC-E6's, unchanged.
		if s.Model != session.PlatformDefaultModel {
			t.Errorf("model = %q, want %q", s.Model, session.PlatformDefaultModel)
		}
		if got := orch.WorkloadFor(s.ID); got != session.WorkloadTypeApprovalGated {
			t.Errorf("orchestrator workload = %q, want %q", got, session.WorkloadTypeApprovalGated)
		}
		if got := orch.ModelFor(s.ID); got != session.PlatformDefaultModel {
			t.Errorf("orchestrator model = %q, want %q", got, session.PlatformDefaultModel)
		}
	})

	t.Run("explicit approval-gated model", func(t *testing.T) {
		const model = "anthropic/claude-opus-4-6"
		status, s := createRaw(t, srv.URL, map[string]any{
			"name": "wt-ag-model", "workloadType": "approval-gated", "model": model,
		})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.Model != model || orch.ModelFor(s.ID) != model {
			t.Errorf("model = %q, orchestrator model = %q, want %q", s.Model, orch.ModelFor(s.ID), model)
		}
	})

	t.Run("unknown type is rejected", func(t *testing.T) {
		before := orch.RunningCount()
		for _, bad := range []any{"foo", "SHELL", "claude_code", "approval_gated", "APPROVAL-GATED", " approval-gated", "", nil, 42} {
			status, _ := createRaw(t, srv.URL, map[string]any{"name": "wt-bad", "workloadType": bad})
			if status != http.StatusBadRequest {
				t.Errorf("workloadType=%v: status = %d, want 400", bad, status)
			}
		}
		if got := orch.RunningCount(); got != before {
			t.Errorf("invalid workload types provisioned pods: running=%d, want %d", got, before)
		}
	})

	t.Run("invalid models are rejected", func(t *testing.T) {
		cases := []map[string]any{
			{"name": "model-shell", "workloadType": "shell", "model": "claude-sonnet-4-5"},
			{"name": "model-empty", "workloadType": "claude-code", "model": ""},
			{"name": "model-trim", "workloadType": "claude-code", "model": " claude-sonnet-4-5"},
			{"name": "model-null", "workloadType": "claude-code", "model": nil},
			{"name": "model-number", "workloadType": "claude-code", "model": 42},
			{"name": "model-space", "workloadType": "claude-code", "model": "bad model"},
			{"name": "model-option", "workloadType": "claude-code", "model": "--danger"},
			// AC-F1: same model contract, so the same inputs are refused.
			{"name": "model-ag-empty", "workloadType": "approval-gated", "model": ""},
			{"name": "model-ag-null", "workloadType": "approval-gated", "model": nil},
			{"name": "model-ag-space", "workloadType": "approval-gated", "model": "bad model"},
		}
		for _, body := range cases {
			status, _ := createRaw(t, srv.URL, body)
			if status != http.StatusBadRequest {
				t.Errorf("body=%v: status = %d, want 400", body, status)
			}
		}
	})
}

// AC-E1/AC-E6: workload type and model are immutable for the session lifetime.
func TestWorkloadTypeIsImmutableAfterCreate(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	const model = "claude-sonnet-4-5"
	status, created := createRaw(t, srv.URL, map[string]any{
		"name": "wt-immutable", "workloadType": "claude-code", "model": model,
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}

	for _, path := range []string{"/read", "/write", "/switch"} {
		body := bytes.NewReader([]byte(`{"workloadType":"shell","model":"platform-default","payload":"x"}`))
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+created.ID+path, "application/json", body)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("post %s status = %d, want 400", path, resp.StatusCode)
		}
	}

	resp, err := http.Get(srv.URL + "/api/v1/sessions/" + created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got session.Session
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WorkloadType != session.WorkloadTypeClaudeCode {
		t.Errorf("workloadType after mutating calls = %q, want %q", got.WorkloadType, session.WorkloadTypeClaudeCode)
	}
	if got.Model != model {
		t.Errorf("model after mutating calls = %q, want %q", got.Model, model)
	}
	if provisioned := orch.ModelFor(created.ID); provisioned != model {
		t.Errorf("orchestrator model = %q, want %q", provisioned, model)
	}
}

// AC-E6: the model is fixed across a snapshot/restore round trip over HTTP.
func TestModelSurvivesSnapshotRestoreOverHTTP(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	const model = "claude-sonnet-4-5"
	status, created := createRaw(t, srv.URL, map[string]any{
		"name": "model-round-trip", "workloadType": "claude-code", "model": model,
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", status)
	}

	resp, err := http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("snapshot status = %d, want 200", resp.StatusCode)
	}
	var frozen session.Session
	if err := json.NewDecoder(resp.Body).Decode(&frozen); err != nil {
		resp.Body.Close()
		t.Fatalf("decode snapshot: %v", err)
	}
	resp.Body.Close()
	if frozen.State != session.StateSnapshot || frozen.Model != model {
		t.Errorf("frozen state/model = %q/%q, want snapshot/%q", frozen.State, frozen.Model, model)
	}

	resp, err = http.Post(srv.URL+"/api/v1/sessions/"+created.ID+"/switch", "application/json", nil)
	if err != nil {
		t.Fatalf("restore switch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("restore switch status = %d, want 200", resp.StatusCode)
	}
	var restored session.Session
	if err := json.NewDecoder(resp.Body).Decode(&restored); err != nil {
		resp.Body.Close()
		t.Fatalf("decode restored session: %v", err)
	}
	resp.Body.Close()
	if restored.State != session.StateActive || restored.Model != model {
		t.Errorf("restored state/model = %q/%q, want active/%q", restored.State, restored.Model, model)
	}
	if got := orch.ModelFor(created.ID); got != model {
		t.Errorf("restored orchestrator model = %q, want %q", got, model)
	}
}
