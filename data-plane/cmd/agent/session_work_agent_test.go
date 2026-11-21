package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The relay reaches the sidecar through a capability interface, which is a
// silent coupling: the check is a type assertion, so a workload that stops
// satisfying it does not fail to compile — every work read just answers 501.
// That is indistinguishable from the honest 501 a workload with no session
// database gives, so nothing downstream can tell the two apart. These tests
// pin both answers to the one input that decides between them.

// codexWorkReporter is the compile-time half: the concrete workload the relay
// is asked about has to be the interface the relay looks for.
var _ workReporter = codexCLI{}

// relayTestWorkload builds the agent the control plane talks to, with a codex
// workload whose session DB sidecar is at dbURL ("" = a slot that keeps none).
func relayTestWorkload(t *testing.T, dbURL string) *httptest.Server {
	t.Helper()
	cli := newCodexTestCLI(t, codexSidecar{}, nil)
	cli.sessionDBURL = dbURL
	_, srv := newCodexTestServer(t, newFakeClaudeRunner(), platformDefaultModel, false,
		filepath.Join(t.TempDir(), "state"), cli)
	return srv
}

// AC-N9/AC-N10 read path: a slot that keeps work records must serve them. The
// regression this pins is the capability gap — codexCLI carried the sidecar URL
// in a field but exposed no sessionDBMCPURL method, so the relay's assertion
// failed and both work reads and the request write answered 501 on every slot.
func TestWorkRelayServesASlotThatKeepsRecords(t *testing.T) {
	var got []string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case sessionDBWorkTasksPath:
			_, _ = w.Write([]byte(`{"tasks":[],"next_cursor":"c1"}`))
		case sessionDBWorkTurnsPath:
			_, _ = w.Write([]byte(`{"task_id":"t-1","turns":[]}`))
		case sessionDBWorkRequestsPath:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"turn":{"id":7}}`))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(sidecar.Close)
	srv := relayTestWorkload(t, sidecar.URL+sessionDBMCPPath)

	resp, err := http.Get(srv.URL + agentWorkTasksPath + "?limit=10&after_id=a-1")
	if err != nil {
		t.Fatalf("work tasks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("work tasks = %d, want the sidecar's page", resp.StatusCode)
	}
	var page struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode tasks page: %v", err)
	}
	if page.NextCursor != "c1" {
		t.Fatalf("next_cursor = %q, want the sidecar's own answer", page.NextCursor)
	}

	turns, err := http.Get(srv.URL + agentWorkTurnsPath + "?task_id=t-1")
	if err != nil {
		t.Fatalf("work turns: %v", err)
	}
	defer turns.Body.Close()
	if turns.StatusCode != http.StatusOK {
		t.Fatalf("work turns = %d, want the sidecar's page", turns.StatusCode)
	}

	made, err := http.Post(srv.URL+agentWorkRequestsPath, "application/json",
		strings.NewReader(`{"task_id":"t-1","request":"이거 해 줘"}`))
	if err != nil {
		t.Fatalf("work request: %v", err)
	}
	defer made.Body.Close()
	if made.StatusCode != http.StatusCreated {
		t.Fatalf("work request = %d, want the sidecar's 201", made.StatusCode)
	}

	// The relay forwards the caller's page arguments rather than inventing its
	// own, and reaches the work paths beside the MCP path it was given.
	want := []string{
		sessionDBWorkTasksPath + "?after_id=a-1&limit=10",
		sessionDBWorkTurnsPath + "?task_id=t-1",
		sessionDBWorkRequestsPath + "?",
	}
	if len(got) != len(want) {
		t.Fatalf("sidecar saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sidecar call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The other side of the same decision: a slot whose type keeps no session
// database says so with 501 rather than an empty page, which would claim the
// session has no work.
func TestWorkRelayRefusesASlotThatKeepsNoRecords(t *testing.T) {
	srv := relayTestWorkload(t, "")

	for _, path := range []string{agentWorkTasksPath, agentWorkTurnsPath} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("GET %s = %d, want 501", path, resp.StatusCode)
		}
	}
	resp, err := http.Post(srv.URL+agentWorkRequestsPath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST %s: %v", agentWorkRequestsPath, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("POST %s = %d, want 501", agentWorkRequestsPath, resp.StatusCode)
	}
}
