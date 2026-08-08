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

// AC-E1: the create request carries the workload type. Omitting it must keep
// the pre-type behaviour (a shell session), an allowed value must be honoured
// and echoed back, and anything else must be a 400 — the type axis must not
// become a way to smuggle an unknown workload past the control plane.
func TestCreateSessionWorkloadType(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	t.Run("omitted defaults to shell", func(t *testing.T) {
		status, s := createRaw(t, srv.URL, map[string]any{"name": "wt-default"})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.WorkloadType != session.WorkloadTypeShell {
			t.Errorf("workloadType = %q, want %q", s.WorkloadType, session.WorkloadTypeShell)
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
	})

	t.Run("explicit claude-code", func(t *testing.T) {
		status, s := createRaw(t, srv.URL, map[string]any{"name": "wt-cc", "workloadType": "claude-code"})
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		if s.WorkloadType != session.WorkloadTypeClaudeCode {
			t.Errorf("workloadType = %q, want %q", s.WorkloadType, session.WorkloadTypeClaudeCode)
		}
	})

	t.Run("unknown type is rejected", func(t *testing.T) {
		for _, bad := range []string{"foo", "SHELL", "claude_code", " shell"} {
			status, _ := createRaw(t, srv.URL, map[string]any{"name": "wt-bad", "workloadType": bad})
			if status != http.StatusBadRequest {
				t.Errorf("workloadType=%q: status = %d, want 400", bad, status)
			}
		}
	})
}

// AC-E1: the type is immutable for the session's lifetime. There is no API that
// changes it — this pins that: the fields a client can send on create are the
// only lever, and a subsequent GET reports what creation fixed. If a mutation
// endpoint is ever added, this test is where the immutability rule has to be
// re-argued.
func TestWorkloadTypeIsImmutableAfterCreate(t *testing.T) {
	srv := newServer()
	defer srv.Close()

	status, created := createRaw(t, srv.URL, map[string]any{"name": "wt-immutable", "workloadType": "claude-code"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}

	// Every mutating route the API exposes for an existing session; none of them
	// takes a type, so none of them can change it.
	for _, path := range []string{"/read", "/write", "/switch"} {
		body := bytes.NewReader([]byte(`{"workloadType":"shell","payload":"x"}`))
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+created.ID+path, "application/json", body)
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		resp.Body.Close()
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
}
