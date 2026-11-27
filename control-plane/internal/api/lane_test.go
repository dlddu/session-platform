package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// The lanes of a manager session at the entry point a client actually uses
// (AC-N11). These tests go through the HTTP API rather than the service,
// because the question each of them answers — which conversation did this reach,
// and what is refused — is only true if it is true of the real route.

func postJSON(t *testing.T, url string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func getLanes(t *testing.T, srv *httptest.Server, id string) (int, session.LanePage) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/v1/sessions/" + id + "/lanes")
	if err != nil {
		t.Fatalf("get lanes: %v", err)
	}
	defer resp.Body.Close()
	var page session.LanePage
	_ = json.NewDecoder(resp.Body).Decode(&page)
	return resp.StatusCode, page
}

// newManagerSession is a running paired manager, which is the only shape that
// has a second conversation to address.
func newManagerSession(t *testing.T, srv *httptest.Server, stub *agent.StubClient, name string) session.Session {
	t.Helper()
	stub.LanePaired(true)
	status, s := createRaw(t, srv.URL, map[string]any{"name": name, "workloadType": "codex-manager"})
	if status != http.StatusCreated {
		t.Fatalf("create manager session = %d", status)
	}
	return s
}

// TestLanesRouteSaysThePairIsInOnePod is the fact a reader outside the session
// needs and cannot otherwise check: both conversations, and the one pod they
// are in.
func TestLanesRouteSaysThePairIsInOnePod(t *testing.T) {
	srv, stub := workServer(t)
	s := newManagerSession(t, srv, stub, "lanes-paired")

	status, page := getLanes(t, srv, s.ID)
	if status != http.StatusOK {
		t.Fatalf("GET lanes = %d, want 200", status)
	}
	if !page.Paired || len(page.Lanes) != 2 {
		t.Fatalf("lanes = %+v, want two conversations and paired=true", page)
	}
	if page.Pod == "" {
		t.Fatal("the page must name the pod the lanes are in")
	}
	for _, lane := range page.Lanes {
		if lane.Pod != page.Pod {
			t.Fatalf("lane %+v is reported in a different pod from %q — the pair must be in one pod", lane, page.Pod)
		}
	}
	if page.Lanes[0].Role != session.LaneParent || page.Lanes[1].Role != session.LaneMonitor {
		t.Fatalf("lanes = %+v, want the parent then the monitor", page.Lanes)
	}
	if page.Lanes[1].Path == page.Lanes[0].Path {
		t.Fatalf("two lanes in one pod must be two addresses: %+v", page.Lanes)
	}

	// A workload that runs one conversation says so, which is not the same as
	// having none: the chat is there.
	stub.LanePaired(false)
	_, single := getLanes(t, srv, s.ID)
	if single.Paired || len(single.Lanes) != 1 {
		t.Fatalf("unpaired lanes = %+v, want one conversation and paired=false", single)
	}

	// A pod with no such route — one started before the pair — is unsupported
	// rather than an empty list.
	stub.LaneUnreachable(true)
	status, _ = getLanes(t, srv, s.ID)
	if status != http.StatusNotImplemented {
		t.Fatalf("GET lanes on a pod that predates the pair = %d, want 501", status)
	}
	stub.LaneUnreachable(false)
}

// TestLanesRouteIsPassive: asking which lanes a session has must not be the
// thing that starts its pod, or a reader checking the pair's shape would wake
// a frozen session to do it.
func TestLanesRouteIsPassive(t *testing.T) {
	srv, stub := workServer(t)
	s := newManagerSession(t, srv, stub, "lanes-passive")

	if status, _ := postJSON(t, srv.URL+"/api/v1/sessions/"+s.ID+"/snapshot", map[string]any{}); status != http.StatusOK {
		t.Fatalf("snapshot = %d", status)
	}
	status, page := getLanes(t, srv, s.ID)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("GET lanes on a snapshot = %d, want 422 rather than a restore", status)
	}
	if page.Paired || len(page.Lanes) != 0 {
		t.Fatalf("a snapshot reported lanes %+v; it has no live pod to ask", page)
	}
	// And it stayed a snapshot: the read did not restore it.
	resp, err := http.Get(srv.URL + "/api/v1/sessions/" + s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var after session.Session
	_ = json.NewDecoder(resp.Body).Decode(&after)
	if after.State != session.StateSnapshot {
		t.Fatalf("state after asking for lanes = %q, want it still snapshotted", after.State)
	}
}

// TestWriteReachesTheLaneItNames is the separation at the entry point: a write
// naming the monitor goes to the monitor's conversation, a write naming nothing
// goes to the person's chat, and neither appears in the other.
func TestWriteReachesTheLaneItNames(t *testing.T) {
	srv, stub := workServer(t)
	s := newManagerSession(t, srv, stub, "lane-write")
	write := func(body map[string]any) int {
		status, _ := postJSON(t, srv.URL+"/api/v1/sessions/"+s.ID+"/write", body)
		return status
	}

	if status := write(map[string]any{"payload": "사용자에게 답할 말"}); status != http.StatusOK {
		t.Fatalf("write with no lane = %d, want 200", status)
	}
	if status := write(map[string]any{"payload": "작업을 옮겨라", "lane": "monitor"}); status != http.StatusOK {
		t.Fatalf("write to the monitor lane = %d, want 200", status)
	}
	if status := write(map[string]any{"payload": "사용자에게 또 답할 말", "lane": "parent"}); status != http.StatusOK {
		t.Fatalf("write naming the parent lane = %d, want 200", status)
	}

	pod := stub.LaneWrites(podOf(t, srv, s.ID), session.LaneMonitor)
	if len(pod) != 1 || pod[0] != "작업을 옮겨라" {
		t.Fatalf("monitor lane received %v, want only what was addressed to it", pod)
	}

	// The parent lane's conversation is the pod's own, so a read with no lane
	// sees both parent writes and neither of the monitor's.
	_, chat := readLane(t, srv, s.ID, "", 0)
	if !strings.Contains(chat, "사용자에게 답할 말") || !strings.Contains(chat, "사용자에게 또 답할 말") {
		t.Fatalf("the chat lost a write meant for it: %q", chat)
	}
	if strings.Contains(chat, "작업을 옮겨라") {
		t.Fatalf("the monitor's prompt leaked into the person's chat: %q", chat)
	}
	_, monitor := readLane(t, srv, s.ID, "monitor", 0)
	if monitor != "작업을 옮겨라" {
		t.Fatalf("the monitor lane's conversation = %q, want its own prompt alone", monitor)
	}
}

// TestLaneCursorsAreSeparate: each lane has its own output buffer, so an offset
// from one lane means nothing on the other and must not be read as "nothing
// new".
func TestLaneCursorsAreSeparate(t *testing.T) {
	srv, stub := workServer(t)
	s := newManagerSession(t, srv, stub, "lane-cursor")
	postJSON(t, srv.URL+"/api/v1/sessions/"+s.ID+"/write", map[string]any{"payload": "열두자이상의긴답변입니다"})
	postJSON(t, srv.URL+"/api/v1/sessions/"+s.ID+"/write",
		map[string]any{"payload": "짧다", "lane": "monitor"})

	chatNext, _ := readLane(t, srv, s.ID, "parent", 0)
	monitorNext, monitorPayload := readLane(t, srv, s.ID, "monitor", 0)
	if chatNext == monitorNext {
		t.Fatalf("both lanes issued cursor %d; the buffers are not separate", chatNext)
	}
	// Reading the monitor at its own cursor is empty; reading it at the chat's
	// larger cursor must not silently be empty for the wrong reason.
	if _, tail := readLane(t, srv, s.ID, "monitor", monitorNext); tail != "" {
		t.Fatalf("monitor at its own cursor = %q, want nothing new", tail)
	}
	if _, again := readLane(t, srv, s.ID, "monitor", 0); again != monitorPayload {
		t.Fatalf("the monitor lane's read is not stable: %q then %q", monitorPayload, again)
	}
}

// TestLaneIsRefusedRatherThanDefaulted: a misspelt lane silently reading the
// chat would be a client that thinks it is watching the monitor.
func TestLaneIsRefusedRatherThanDefaulted(t *testing.T) {
	srv, stub := workServer(t)
	s := newManagerSession(t, srv, stub, "lane-bad")
	base := srv.URL + "/api/v1/sessions/" + s.ID

	for _, lane := range []string{"Monitor", "monito", "worker", "both", "parent,monitor"} {
		if status, _ := postJSON(t, base+"/write", map[string]any{"payload": "x", "lane": lane}); status != http.StatusBadRequest {
			t.Fatalf("write lane=%q = %d, want 400", lane, status)
		}
		if status, _ := postJSON(t, base+"/read", map[string]any{"lane": lane}); status != http.StatusBadRequest {
			t.Fatalf("read lane=%q = %d, want 400", lane, status)
		}
		resp, err := http.Get(base + "/stream?lane=" + lane)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("stream lane=%q = %d, want 400", lane, resp.StatusCode)
		}
	}

	// An empty lane is the chat, not a refusal: that is what keeps every client
	// written before the pair correct. Whitespace alone is read the same way —
	// it names no lane — rather than as a lane called " ".
	for _, lane := range []string{"", " ", "  parent  "} {
		if status, _ := postJSON(t, base+"/read", map[string]any{"lane": lane}); status != http.StatusOK {
			t.Fatalf("read lane=%q = %d, want the chat", lane, status)
		}
	}
}

// TestLaneWriteAndToolApprovalsAreMutuallyExclusive: one is a manager's choice
// and the other claude-code's, so a write that names both is refused instead of
// having one of them quietly dropped.
func TestLaneWriteAndToolApprovalsAreMutuallyExclusive(t *testing.T) {
	srv, stub := workServer(t)
	s := newManagerSession(t, srv, stub, "lane-approvals")
	status, body := postJSON(t, srv.URL+"/api/v1/sessions/"+s.ID+"/write", map[string]any{
		"payload": "x", "lane": "monitor", "toolApprovals": true,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("lane + toolApprovals = %d, want 400", status)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "lane") {
		t.Fatalf("the refusal %q must say what the conflict is", message)
	}
	// Nothing was written to either conversation.
	if writes := stub.LaneWrites(podOf(t, srv, s.ID), session.LaneMonitor); len(writes) != 0 {
		t.Fatalf("a refused write still reached the monitor lane: %v", writes)
	}
}

// TestMonitorLaneIsOnlyAManagers: a second conversation is a manager's shape.
// Naming it on a workload that runs one is unsupported rather than a write that
// goes somewhere unexpected.
func TestMonitorLaneIsOnlyAManagers(t *testing.T) {
	srv, _ := workServer(t)
	status, s := createRaw(t, srv.URL, map[string]any{"name": "lane-shell", "workloadType": "shell"})
	if status != http.StatusCreated {
		t.Fatalf("create shell session = %d", status)
	}
	if status, _ := postJSON(t, srv.URL+"/api/v1/sessions/"+s.ID+"/write",
		map[string]any{"payload": "x", "lane": "monitor"}); status != http.StatusNotImplemented {
		t.Fatalf("monitor lane on a shell session = %d, want 501", status)
	}
	// Its own chat still writes exactly as it did.
	if status, _ := postJSON(t, srv.URL+"/api/v1/sessions/"+s.ID+"/write",
		map[string]any{"payload": "echo hi"}); status != http.StatusOK {
		t.Fatalf("a write with no lane on a shell session = %d, want 200", status)
	}
}

func readLane(t *testing.T, srv *httptest.Server, id, lane string, offset int64) (int64, string) {
	t.Helper()
	body := map[string]any{"offset": offset}
	if lane != "" {
		body["lane"] = lane
	}
	status, decoded := postJSON(t, srv.URL+"/api/v1/sessions/"+id+"/read", body)
	if status != http.StatusOK {
		t.Fatalf("read lane %q = %d: %v", lane, status, decoded)
	}
	next, _ := decoded["nextOffset"].(float64)
	payload, _ := decoded["payload"].(string)
	return int64(next), payload
}

// podOf is the pod the session's lanes run in, read through the route that
// reports it rather than guessed from the session name.
func podOf(t *testing.T, srv *httptest.Server, id string) string {
	t.Helper()
	status, page := getLanes(t, srv, id)
	if status != http.StatusOK {
		t.Fatalf("GET lanes = %d", status)
	}
	return page.Pod
}
