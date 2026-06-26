//go:build e2e

// Package e2e_test drives the *deployed* control-plane SUT over HTTP and asserts
// the create/list/get/switch·read·write happy path end-to-end (build tag `e2e`,
// run via `make e2e-up && go test -tags=e2e ./test/...`).
//
// Unlike integration_test.go (which mounts the handlers in-process), this suite
// is a black box: it only knows the wire contract (the /api/v1 surface and its
// JSON DTOs) and talks to whatever E2E_BASE_URL points at — the kind-deployed
// stub control-plane (default http://localhost:8080, see deploy/ + scripts/e2e).
//
// Scope is the α stub level: the adapters are in-memory stubs, so every created
// session stays `active`. The B-path (idle -> snapshot -> restore) and real
// pod/Redis/CRIU assertions are deferred — see e2e_deferred_test.go.
package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Creating a session now provisions a real pod and waits for it to report Ready
// (the in-cluster client-go orchestrator), so the create calls can take longer
// than a stub round-trip — the timeout has headroom for image pull + schedule.
var client = &http.Client{Timeout: 90 * time.Second}

func baseURL() string {
	if v := os.Getenv("E2E_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8080"
}

// session mirrors the JSON the API emits (control-plane/api/openapi.yaml). It is
// declared locally so the e2e suite asserts the wire contract independently of
// the internal domain types.
type session struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Pod        string `json:"pod"`
	CreatedAt  string `json:"createdAt"`
	LastAccess string `json:"lastAccess"`
}

type readResp struct {
	Session session `json:"session"`
	Path    string  `json:"path"`
	Payload string  `json:"payload"`
}

type writeResp struct {
	Session session `json:"session"`
	Path    string  `json:"path"`
}

// do performs a request against the SUT and returns the response plus the
// fully-read body, failing the test on transport errors.
func do(t *testing.T, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL()+path, r)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v (is the SUT up? run `make e2e-up` or set E2E_BASE_URL)", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s %s: %v", method, path, err)
	}
	return resp, out
}

// uniqueName derives a collision-free session name from the test name; the SUT
// state is shared in-process across runs, so names must be unique.
func uniqueName(t *testing.T) string {
	t.Helper()
	base := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func createSession(t *testing.T, name string) session {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions", map[string]string{"name": name})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %q: status=%d body=%s", name, resp.StatusCode, body)
	}
	var s session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decode created session: %v body=%s", err, body)
	}
	return s
}

func TestHealthz(t *testing.T) {
	resp, body := do(t, http.MethodGet, "/api/v1/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status=%d body=%s", resp.StatusCode, body)
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode healthz: %v body=%s", err, body)
	}
	if m["status"] != "ok" {
		t.Fatalf("healthz status=%q want ok", m["status"])
	}
}

// AC-A1/A2: creating a session returns an active session bound to a dedicated pod.
func TestCreateSession_ActiveWithPod(t *testing.T) {
	s := createSession(t, uniqueName(t))
	if s.ID == "" {
		t.Fatal("expected a non-empty session id")
	}
	if s.State != "active" {
		t.Fatalf("state=%q want active", s.State)
	}
	if s.Pod == "" {
		t.Fatal("expected a dedicated pod name (AC-A1/A2)")
	}
}

// AC-A2: N sessions map 1:1 to N unique pods.
func TestCreateSessions_UniquePods(t *testing.T) {
	pods := map[string]string{} // pod -> session id
	for i := 0; i < 3; i++ {
		s := createSession(t, fmt.Sprintf("%s-%d", uniqueName(t), i))
		if s.Pod == "" {
			t.Fatalf("session %s has no pod", s.ID)
		}
		if prev, dup := pods[s.Pod]; dup {
			t.Fatalf("pod %q shared by sessions %s and %s (AC-A2 violated)", s.Pod, prev, s.ID)
		}
		pods[s.Pod] = s.ID
	}
	if len(pods) != 3 {
		t.Fatalf("expected 3 unique pods, got %d", len(pods))
	}
}

// V5: a created session appears in the list (single source of truth).
func TestListSessions_ContainsCreated(t *testing.T) {
	s := createSession(t, uniqueName(t))
	resp, body := do(t, http.MethodGet, "/api/v1/sessions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	var lr struct {
		Sessions []session `json:"sessions"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode list: %v body=%s", err, body)
	}
	for _, x := range lr.Sessions {
		if x.ID == s.ID {
			if x.Name != s.Name {
				t.Fatalf("listed name=%q want %q", x.Name, s.Name)
			}
			return
		}
	}
	t.Fatalf("created session %s (%s) not found in list of %d", s.ID, s.Name, len(lr.Sessions))
}

// V5: GET /sessions/{id} returns the same session as create.
func TestGetSession_Matches(t *testing.T) {
	s := createSession(t, uniqueName(t))
	resp, body := do(t, http.MethodGet, "/api/v1/sessions/"+s.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d body=%s", resp.StatusCode, body)
	}
	var got session
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode get: %v body=%s", err, body)
	}
	if got.ID != s.ID || got.Name != s.Name || got.Pod != s.Pod || got.State != "active" {
		t.Fatalf("get mismatch: got %+v want id/name/pod of %+v", got, s)
	}
}

// AC-C4: switching an already-active session is a no-op (stays active).
func TestSwitchSession_ActiveNoop(t *testing.T) {
	s := createSession(t, uniqueName(t))
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/switch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch status=%d body=%s", resp.StatusCode, body)
	}
	var got session
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode switch: %v body=%s", err, body)
	}
	if got.ID != s.ID {
		t.Fatalf("switch id=%q want %q", got.ID, s.ID)
	}
	if got.State != "active" {
		t.Fatalf("switch state=%q want active (no-op)", got.State)
	}
}

// AC-C2: reading an active session is served by the "active" path with a payload.
func TestReadSession_ActivePath(t *testing.T) {
	s := createSession(t, uniqueName(t))
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/read", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read status=%d body=%s", resp.StatusCode, body)
	}
	var r readResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode read: %v body=%s", err, body)
	}
	if r.Path != "active" {
		t.Fatalf("read path=%q want active", r.Path)
	}
	if r.Payload == "" {
		t.Fatal("expected a non-empty read payload")
	}
	if r.Session.ID != s.ID {
		t.Fatalf("read session id=%q want %q", r.Session.ID, s.ID)
	}
}

// AC-C3: writing an active session is served by the "active" path.
func TestWriteSession_ActivePath(t *testing.T) {
	s := createSession(t, uniqueName(t))
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/write", map[string]string{"payload": "stub-write"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status=%d body=%s", resp.StatusCode, body)
	}
	var w writeResp
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("decode write: %v body=%s", err, body)
	}
	if w.Path != "active" {
		t.Fatalf("write path=%q want active", w.Path)
	}
	if w.Session.ID != s.ID {
		t.Fatalf("write session id=%q want %q", w.Session.ID, s.ID)
	}
}

// Error mapping: an unknown session id is a 404.
func TestGetSession_NotFound(t *testing.T) {
	resp, _ := do(t, http.MethodGet, "/api/v1/sessions/does-not-exist", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get unknown status=%d want 404", resp.StatusCode)
	}
}
