package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// workServer is the API with a stub agent holding a slot's work records.
func workServer(t *testing.T) (*httptest.Server, *agent.StubClient) {
	t.Helper()
	orch := k8s.NewStubOrchestrator("sessions")
	ckpt := archive.NewStubCheckpointer(true)
	stub := agent.NewStubClient()
	mgr := service.New(orch, configmap.NewStore(fake.NewSimpleClientset(), "sessions"), ckpt, stub,
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexChat, ckpt))
	mux := http.NewServeMux()
	api.New(mgr).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, stub
}

// workRow is one task as the slot holds it, for the tests about paging rather
// than about the fields (workFixture covers those).
func workRow(id, status string) session.WorkTaskRecord {
	return session.WorkTaskRecord{
		Task: session.WorkTask{
			ID: id, SessionID: str("now"), Title: "task " + id,
			Description: str("무엇을 why"), CompletionCriteria: str("머지되면 끝"),
			Status: status, Version: 1,
		},
	}
}

// postWorkRequest stores one person's request the way an app's backend does.
func postWorkRequest(
	t *testing.T, srv *httptest.Server, sessionID, taskID string, body map[string]any,
) (int, session.WorkRequestStored) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	target := srv.URL + "/api/v1/sessions/" + sessionID + "/work/tasks/" + taskID + "/requests"
	resp, err := http.Post(target, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("store request: %v", err)
	}
	defer resp.Body.Close()
	var stored session.WorkRequestStored
	_ = json.NewDecoder(resp.Body).Decode(&stored)
	return resp.StatusCode, stored
}

func newCodexChat(t *testing.T, srv *httptest.Server, name string) session.Session {
	t.Helper()
	_, s := createRaw(t, srv.URL, map[string]any{"name": name, "workloadType": "codex-chat"})
	return s
}

// The write side, end to end: a person's words reach the slot and become the
// next turn of the task, and the turn is then there to read back through the
// route the app actually uses. Before the work routes existed this path went
// through the ledger, and nothing checked that the two ends agreed — which is
// exactly how the control plane came to be calling a route the data plane no
// longer served.
func TestWorkRequests_BecomeTheNextTurnOfTheTask(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "write")

	status, stored := postWorkRequest(t, srv, s.ID, "task-moved", map[string]any{
		"id": "req-1", "text": "이것도 해 줘", "author": "user-1",
	})
	if status != http.StatusCreated {
		t.Fatalf("store = %d %+v, want 201", status, stored)
	}
	if stored.Replayed {
		t.Fatalf("a first store reported itself as a replay: %+v", stored)
	}
	if stored.Turn.Request == nil || *stored.Turn.Request != "이것도 해 줘" {
		t.Fatalf("turn = %+v, want the person's words verbatim", stored.Turn)
	}
	if stored.Turn.TaskID != "task-moved" {
		t.Fatalf("turn task = %q, want the task from the path", stored.Turn.TaskID)
	}
	// A turn a person wrote through this route says so, which is how a reader
	// tells it from one the session wrote itself.
	if stored.Turn.Origin != "app_request" {
		t.Fatalf("origin = %q, want app_request", stored.Turn.Origin)
	}
	// It is the task's newest turn when the app reads the task back.
	code, page := getWorkTurns(t, srv, s.ID, "task-moved", "")
	if code != http.StatusOK {
		t.Fatalf("turns = %d", code)
	}
	last := page.Turns[len(page.Turns)-1]
	if last.ID != stored.Turn.ID {
		t.Fatalf("turns = %+v, want the stored request last", page.Turns)
	}
	// The task page's turn tail moved with it, so a panel showing "somebody is
	// waiting" does not need a second call.
	_, tasks := getWorkTasks(t, srv, s.ID, "")
	for _, record := range tasks.Tasks {
		if record.Task.ID != "task-moved" {
			continue
		}
		if record.TurnsTotal != 3 || record.LatestTurn == nil || record.LatestTurn.ID != stored.Turn.ID {
			t.Fatalf("task tail = %+v, want the new turn counted and latest", record)
		}
	}
	// The body cannot aim at a task other than the one in the URL.
	if len(stub.WorkRequests) != 1 || stub.WorkRequests[0].TaskID != "task-moved" {
		t.Fatalf("stored = %+v, want the task from the path", stub.WorkRequests)
	}
}

// The idempotency contract an app's retry depends on: the same id with the same
// words is the same turn answered 200, and the same id with different words is
// a conflict rather than a second turn.
func TestWorkRequests_RetryIsTheSameTurnAndNotASecondOne(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "retry")

	_, first := postWorkRequest(t, srv, s.ID, "task-moved", map[string]any{
		"id": "req-1", "text": "같은 말", "author": "user-1",
	})
	status, again := postWorkRequest(t, srv, s.ID, "task-moved", map[string]any{
		"id": "req-1", "text": "같은 말", "author": "user-1",
	})
	if status != http.StatusOK {
		t.Fatalf("retry = %d %+v, want 200", status, again)
	}
	if !again.Replayed || again.Turn.ID != first.Turn.ID {
		t.Fatalf("retry = %+v, want the turn the first store made (%+v)", again, first.Turn)
	}
	if status, _ := postWorkRequest(t, srv, s.ID, "task-moved", map[string]any{
		"id": "req-1", "text": "다른 말", "author": "user-1",
	}); status != http.StatusConflict {
		t.Fatalf("same id with different words = %d, want 409", status)
	}
	// One turn was added, not three.
	_, page := getWorkTurns(t, srv, s.ID, "task-moved", "")
	if len(page.Turns) != 3 {
		t.Fatalf("turns = %d, want the fixture's two plus one", len(page.Turns))
	}
}

// A pick names the turn it was offered on. The slot refuses one from a turn the
// task has moved past, and that refusal is its own class: the screen is out of
// date, so the app re-reads the task and keeps the words the person typed.
func TestWorkRequests_RefuseAPickFromATurnTheTaskMovedPast(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "pick")

	// Turn 2 is the latest, and its choices are the ones being offered.
	if status, _ := postWorkRequest(t, srv, s.ID, "task-moved", map[string]any{
		"id": "pick-1", "text": "코멘트 전부 반영해 줘", "author": "user-1", "choice_turn_id": 2,
	}); status != http.StatusCreated {
		t.Fatalf("a pick from the latest turn = %d, want 201", status)
	}
	// That store made a newer turn, so the old screen is now old.
	status, _ := postWorkRequest(t, srv, s.ID, "task-moved", map[string]any{
		"id": "pick-2", "text": "코멘트 전부 반영해 줘", "author": "user-1", "choice_turn_id": 2,
	})
	if status != http.StatusConflict {
		t.Fatalf("a pick from a turn the task moved past = %d, want 409", status)
	}
	// Free text is never refused for staleness: the person's own words do not
	// depend on which screen they were typed on.
	if status, _ := postWorkRequest(t, srv, s.ID, "task-moved", map[string]any{
		"id": "free-1", "text": "제 말로 쓸게요", "author": "user-1",
	}); status != http.StatusCreated {
		t.Fatalf("free text = %d, want 201", status)
	}
}

// Every refusal the write can give, each with the status a caller branches on.
func TestWorkRequests_RefusalsAreClassified(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "refuse")

	for _, tc := range []struct {
		name, taskID string
		body         map[string]any
		want         int
	}{
		{"a task this slot does not have", "no-such-task",
			map[string]any{"id": "r1", "text": "x", "author": "u"}, http.StatusNotFound},
		{"no id", "task-moved",
			map[string]any{"text": "x", "author": "u"}, http.StatusBadRequest},
		{"an id that is not an id", "task-moved",
			map[string]any{"id": "has spaces", "text": "x", "author": "u"}, http.StatusBadRequest},
		{"empty words", "task-moved",
			map[string]any{"id": "r2", "text": "   ", "author": "u"}, http.StatusBadRequest},
		{"words past the bound", "task-moved",
			map[string]any{"id": "r3", "text": strings.Repeat("가", session.WorkRequestTextMax+1), "author": "u"},
			http.StatusBadRequest},
		{"no author", "task-moved",
			map[string]any{"id": "r4", "text": "x"}, http.StatusBadRequest},
		{"a choice turn that is not a turn id", "task-moved",
			map[string]any{"id": "r5", "text": "x", "author": "u", "choice_turn_id": 0}, http.StatusBadRequest},
		{"a field this route does not take", "task-moved",
			map[string]any{"id": "r6", "text": "x", "author": "u", "choice_id": "c-1"}, http.StatusBadRequest},
	} {
		if status, _ := postWorkRequest(t, srv, s.ID, tc.taskID, tc.body); status != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, status, tc.want)
		}
	}
	// Nothing malformed reached the slot: a refusal here costs no hop into the
	// pod, and no half-stored turn is left behind.
	if len(stub.WorkRequests) != 0 {
		t.Fatalf("stored = %+v, want none", stub.WorkRequests)
	}
}

// A workload with no session database keeps no work records. That is 501 — not
// an empty page, which would claim the session has no work.
func TestWorkRoutes_UnsupportedWorkloadIsNotEmptyWork(t *testing.T) {
	srv, _ := workServer(t)
	_, shell := createRaw(t, srv.URL, map[string]any{"name": "sh"})
	if status, _ := getWorkTasks(t, srv, shell.ID, ""); status != http.StatusNotImplemented {
		t.Errorf("shell task read = %d, want 501", status)
	}
	if status, _ := getWorkTurns(t, srv, shell.ID, "task-moved", ""); status != http.StatusNotImplemented {
		t.Errorf("shell turn read = %d, want 501", status)
	}
	if status, _ := postWorkRequest(t, srv, shell.ID, "task-moved",
		map[string]any{"id": "r1", "text": "x", "author": "u"}); status != http.StatusNotImplemented {
		t.Errorf("shell write = %d, want 501", status)
	}
}

// A snapshot session keeps its records on the slot's volume but has no pod to
// reach them through. That is a distinct answer (409) — and no call on this
// path may restore the snapshot, or opening a panel would wake a session and
// leaving a request would be a way to start one.
func TestWorkRoutes_SnapshotIsUnavailableAndIsNotWoken(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "snap")

	resp, err := http.Post(srv.URL+"/api/v1/sessions/"+s.ID+"/snapshot", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot = %d", resp.StatusCode)
	}

	reads, writes := stub.WorkReads, len(stub.WorkRequests)
	if status, page := getWorkTasks(t, srv, s.ID, ""); status != http.StatusConflict {
		t.Fatalf("snapshot task read = %d %+v, want 409", status, page)
	}
	if status, _ := getWorkTurns(t, srv, s.ID, "task-moved", ""); status != http.StatusConflict {
		t.Fatalf("snapshot turn read = %d, want 409", status)
	}
	if status, _ := postWorkRequest(t, srv, s.ID, "task-moved",
		map[string]any{"id": "r1", "text": "x", "author": "u"}); status != http.StatusConflict {
		t.Fatalf("snapshot write = %d, want 409", status)
	}
	if stub.WorkReads != reads || len(stub.WorkRequests) != writes {
		t.Fatalf("a snapshot call reached the pod: reads %d->%d, writes %d->%d",
			reads, stub.WorkReads, writes, len(stub.WorkRequests))
	}

	// The session is still a snapshot: nothing on this path restored it or
	// counted as an access.
	resp, err = http.Get(srv.URL + "/api/v1/sessions/" + s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var after session.Session
	_ = json.NewDecoder(resp.Body).Decode(&after)
	if after.State != session.StateSnapshot {
		t.Fatalf("a work call moved the session to %q; it must stay a snapshot", after.State)
	}
}

// AC-K2: every route on this path is scoped like any other session-id route,
// the write included. Another tenant's session answers exactly like a missing
// one, so one person's chat can never read — or write to — another's tasks.
func TestWorkRoutes_AreScopedToTheOwner(t *testing.T) {
	srv := newTenantServer(t)
	resp, body := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", "alpha",
		map[string]string{"name": "mine", "workloadType": "codex-chat"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, body)
	}
	var mine session.Session
	if err := json.Unmarshal(body, &mine); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/sessions/" + mine.ID
	for _, route := range []struct {
		name, method, path string
		body               any
		owner              int
	}{
		{"the task page", http.MethodGet, base + "/work/tasks", nil, http.StatusOK},
		{"a task's turns", http.MethodGet, base + "/work/tasks/t1/turns", nil, http.StatusNotFound},
		{"storing a request", http.MethodPost, base + "/work/tasks/t1/requests",
			map[string]string{"id": "r1", "text": "x", "author": "u"}, http.StatusNotFound},
	} {
		for _, tc := range []struct {
			who    string
			tenant string
			want   int
		}{
			// The owner's own answer depends on the route; what matters is that
			// it is never the tenant refusal.
			{"the owner", "alpha", route.owner},
			{"another tenant", "beta", http.StatusNotFound},
			{"an unknown caller", "nobody", http.StatusForbidden},
		} {
			resp, body := tenantCall(t, srv, route.method, route.path, tc.tenant, route.body)
			if resp.StatusCode != tc.want {
				t.Errorf("%s / %s = %d %s, want %d", route.name, tc.who, resp.StatusCode, body, tc.want)
			}
		}
	}
}

// Each route takes the one method it is mounted with. A read route that
// accepted a write would be a way to change work by reading it.
func TestWorkRoutes_TakeOnlyTheirOwnMethod(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "methods")
	base := srv.URL + "/api/v1/sessions/" + s.ID
	for _, tc := range []struct {
		path    string
		refused []string
	}{
		{base + "/work/tasks", []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}},
		{base + "/work/tasks/task-moved/turns",
			[]string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}},
		{base + "/work/tasks/task-moved/requests",
			[]string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}},
	} {
		for _, method := range tc.refused {
			req, err := http.NewRequest(method, tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
				t.Errorf("%s reached %s; it is not that route's method", method, tc.path)
			}
		}
	}
}

// Paging the task page: the cursor carries forward and an empty one ends the
// walk, so a reader can tell a finished walk from a truncated one. A page is
// not a snapshot, which is why a reader dedupes by id rather than assuming the
// pages partition the rows.
func TestWorkTasks_PageWithACursor(t *testing.T) {
	srv, stub := workServer(t)
	stub.WorkRows = []session.WorkTaskRecord{
		workRow("t1", "running"), workRow("t2", "blocked"), workRow("t3", "review"),
	}
	s := newCodexChat(t, srv, "page")

	status, first := getWorkTasks(t, srv, s.ID, "limit=2")
	if status != http.StatusOK || len(first.Tasks) != 2 {
		t.Fatalf("first page = %d %+v", status, first)
	}
	if first.NextCursor != "t2" {
		t.Fatalf("next_cursor = %q, want the last id of the page", first.NextCursor)
	}
	_, second := getWorkTasks(t, srv, s.ID, "limit=2&after_id="+first.NextCursor)
	if len(second.Tasks) != 1 || second.Tasks[0].Task.ID != "t3" {
		t.Fatalf("second page = %+v", second.Tasks)
	}
	if second.NextCursor != "" {
		t.Fatalf("next_cursor = %q on the last page, want empty", second.NextCursor)
	}
	// An out-of-range page size is refused rather than clamped: a caller must
	// never be handed a different page than it asked for without being told.
	for _, bad := range []string{"0", "-1", "101", "abc", "1.5"} {
		if status, _ := getWorkTasks(t, srv, s.ID, "limit="+bad); status != http.StatusBadRequest {
			t.Errorf("limit=%s = %d, want 400", bad, status)
		}
	}
	// A status outside the domain is refused too — an empty page would read as
	// "no such work" instead of "no such status".
	if status, _ := getWorkTasks(t, srv, s.ID, "status=nonsense"); status != http.StatusBadRequest {
		t.Errorf("an unknown status = %d, want 400", status)
	}
	// An empty slot is an empty page, not an error: having no tasks yet is the
	// ordinary state of a new chat.
	_, blank := workServer(t)
	_ = blank
	emptySrv, _ := workServer(t)
	empty := newCodexChat(t, emptySrv, "empty")
	status, page := getWorkTasks(t, emptySrv, empty.ID, "")
	if status != http.StatusOK || len(page.Tasks) != 0 || page.NextCursor != "" {
		t.Fatalf("empty slot = %d %+v", status, page)
	}
}

// Paging one task's turns, which is also that task's history: there is no
// separate history route any more, because a past management event was never a
// turn (AC-N10).
func TestWorkTurns_PageOneTasksHistory(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "history")

	status, first := getWorkTurns(t, srv, s.ID, "task-moved", "limit=1")
	if status != http.StatusOK || len(first.Turns) != 1 {
		t.Fatalf("first page = %d %+v", status, first)
	}
	if first.Turns[0].ID != 1 {
		t.Fatalf("turns = %+v, want the oldest first", first.Turns)
	}
	if first.NextCursor != 1 {
		t.Fatalf("next_cursor = %d, want the last turn id of the page", first.NextCursor)
	}
	_, second := getWorkTurns(t, srv, s.ID, "task-moved", "limit=1&after_turn_id=1")
	if len(second.Turns) != 1 || second.Turns[0].ID != 2 {
		t.Fatalf("second page = %+v", second.Turns)
	}
	// The task travels with its turns, so a reader showing one task has its
	// description beside the exchanges rather than needing a second call that
	// could land on a different version.
	if second.TaskID != "task-moved" || second.Task.ID != "task-moved" {
		t.Fatalf("page = %+v, want the task beside its turns", second)
	}
	// open=true narrows it to what still needs the parent.
	_, open := getWorkTurns(t, srv, s.ID, "task-moved", "open=true")
	if len(open.Turns) == 0 {
		t.Fatalf("open turns = %+v, want the one nobody has finished", open.Turns)
	}
	for _, bad := range []string{"-1", "abc"} {
		if status, _ := getWorkTurns(t, srv, s.ID, "task-moved", "after_turn_id="+bad); status != http.StatusBadRequest {
			t.Errorf("after_turn_id=%s = %d, want 400", bad, status)
		}
	}
	if status, _ := getWorkTurns(t, srv, s.ID, "task-moved", "open=maybe"); status != http.StatusBadRequest {
		t.Errorf("open=maybe = %d, want 400", status)
	}
	// A task id that could not be one is refused before the hop.
	if status, _ := getWorkTurns(t, srv, s.ID, "has%20spaces", ""); status != http.StatusBadRequest {
		t.Errorf("a malformed task id = %d, want 400", status)
	}
}

// The ledger routes are gone, not quietly aliased. A caller still asking for
// them has to find out, and a compatibility shim would have left two shapes for
// one truth with nothing behind the second (AC-N10).
func TestLedgerRoutesAreGone(t *testing.T) {
	srv, stub := workServer(t)
	workFixture(stub)
	s := newCodexChat(t, srv, "gone")
	base := srv.URL + "/api/v1/sessions/" + s.ID
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base + "/ledger/tasks"},
		{http.MethodPost, base + "/ledger/tasks/task-moved/requests"},
		{http.MethodGet, base + "/ledger/tasks/task-moved/events"},
	} {
		req, err := http.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404: the route is gone", tc.method, tc.path, resp.StatusCode)
		}
	}
}
