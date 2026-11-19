package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func str(s string) *string { return &s }

// workFixture is the state the review asked to see end to end: a task created
// after the cutover with no turns, and a migrated task with two turns — the
// first finished, the second asking a question with choices.
func workFixture(stub *agent.StubClient) {
	stub.WorkRows = []session.WorkTaskRecord{
		{
			Task: session.WorkTask{
				ID: "task-fresh", SessionID: str("worker-1"), Title: "새 작업",
				Description: str("무엇을 why"), CompletionCriteria: str("머지되면 끝"),
				Status: "planned", Version: 1,
				CreatedAt: str("2026-11-19T00:00:00Z"), UpdatedAt: str("2026-11-19T00:00:00Z"),
			},
			TurnsTotal: 0,
		},
		{
			Task: session.WorkTask{
				ID: "task-moved", SessionID: str("worker-now"), Title: "옮겨 온 작업",
				Status: "blocked", Version: 3,
			},
			TurnsTotal: 2, Migrated: true,
			LatestTurn: &session.WorkTurn{
				ID: 2, TaskID: "task-moved", Request: str("리뷰 반영도 해 줘"),
				Question:    str("어디까지 반영할까요?"),
				Choices:     []session.WorkChoice{{Label: "전부", RequestText: "코멘트 전부 반영해 줘"}},
				RequestedAt: "2026-11-19T01:00:00Z",
			},
		},
	}
	stub.WorkTurnRows = map[string][]session.WorkTurn{
		"task-moved": {
			{
				ID: 1, TaskID: "task-moved", Request: str("이거 먼저 해 줘"),
				Instruction: str("저장소를 읽고 패치를 만들어라"),
				Response:    str("패치를 만들었습니다"),
				Explanation: str("요청한 변경을 PR 로 올렸어요"),
				Artifacts:   []string{"pull/42"},
				RequestedAt: "2026-11-19T00:30:00Z",
				StartedAt:   str("2026-11-19T00:31:00Z"), EndedAt: str("2026-11-19T00:40:00Z"),
				Origin: "user_request",
			},
			{
				ID: 2, TaskID: "task-moved", Request: str("리뷰 반영도 해 줘"),
				Question:    str("어디까지 반영할까요?"),
				Choices:     []session.WorkChoice{{Label: "전부", RequestText: "코멘트 전부 반영해 줘"}},
				RequestedAt: "2026-11-19T01:00:00Z",
				Origin:      "app_request",
			},
		},
	}
}

func getWorkTasks(t *testing.T, srv *httptest.Server, id, query string) (int, session.WorkTaskPage) {
	t.Helper()
	target := srv.URL + "/api/v1/sessions/" + id + "/work/tasks"
	if query != "" {
		target += "?" + query
	}
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("get work tasks: %v", err)
	}
	defer resp.Body.Close()
	var page session.WorkTaskPage
	_ = json.NewDecoder(resp.Body).Decode(&page)
	return resp.StatusCode, page
}

func getWorkTurns(t *testing.T, srv *httptest.Server, id, taskID, query string) (int, session.WorkTurnPage) {
	t.Helper()
	target := srv.URL + "/api/v1/sessions/" + id + "/work/tasks/" + taskID + "/turns"
	if query != "" {
		target += "?" + query
	}
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("get work turns: %v", err)
	}
	defer resp.Body.Close()
	var page session.WorkTurnPage
	_ = json.NewDecoder(resp.Body).Decode(&page)
	return resp.StatusCode, page
}

// AC-N9: the agreed model has to survive the whole path out to the app. The
// ledger routes cannot carry a description, a completion criterion or anything
// a turn holds, so this is the test that the app can show the work rather than
// only that work exists.
func TestWorkTasks_CarryTheAgreedModel(t *testing.T) {
	srv, stub := ledgerServer(t)
	workFixture(stub)
	_, s := createRaw(t, srv.URL, map[string]any{"name": "work", "workloadType": "codex-chat"})

	status, page := getWorkTasks(t, srv, s.ID, "")
	if status != http.StatusOK || len(page.Tasks) != 2 {
		t.Fatalf("work tasks = %d %+v", status, page)
	}
	fresh := page.Tasks[0]
	if fresh.Task.Description == nil || *fresh.Task.Description != "무엇을 why" {
		t.Fatalf("description lost: %+v", fresh.Task)
	}
	if fresh.Task.CompletionCriteria == nil || *fresh.Task.CompletionCriteria != "머지되면 끝" {
		t.Fatalf("completion_criteria lost: %+v", fresh.Task)
	}
	// A task with no turns is the ordinary case and is served, not an error.
	if fresh.TurnsTotal != 0 || fresh.LatestTurn != nil || fresh.Migrated {
		t.Fatalf("a fresh task is not plain: %+v", fresh)
	}
	moved := page.Tasks[1]
	if !moved.Migrated || moved.TurnsTotal != 2 || moved.LatestTurn == nil {
		t.Fatalf("migrated task lost its tail: %+v", moved)
	}
	// A migrated task that never recorded a description says so with null
	// rather than an empty string pretending to be the answer.
	if moved.Task.Description != nil || moved.Task.CompletionCriteria != nil {
		t.Fatalf("a migrated task invented fields: %+v", moved.Task)
	}
	if moved.LatestTurn.Question == nil || len(moved.LatestTurn.Choices) != 1 {
		t.Fatalf("latest turn lost its question: %+v", moved.LatestTurn)
	}
}

// Every field of a turn reaches the app, in the order the turns happened.
func TestWorkTurns_CarryEveryAgreedField(t *testing.T) {
	srv, stub := ledgerServer(t)
	workFixture(stub)
	_, s := createRaw(t, srv.URL, map[string]any{"name": "turns", "workloadType": "codex-chat"})

	status, page := getWorkTurns(t, srv, s.ID, "task-moved", "")
	if status != http.StatusOK || len(page.Turns) != 2 {
		t.Fatalf("turns = %d %+v", status, page)
	}
	if page.TaskID != "task-moved" || page.TaskVersion != 3 {
		t.Fatalf("page does not say what it was read against: %+v", page)
	}
	one := page.Turns[0]
	for field, got := range map[string]*string{
		"request": one.Request, "instruction": one.Instruction,
		"response": one.Response, "explanation": one.Explanation,
		"started_at": one.StartedAt, "ended_at": one.EndedAt,
	} {
		if got == nil || *got == "" {
			t.Fatalf("turn 1 lost %s: %+v", field, one)
		}
	}
	if len(one.Artifacts) != 1 || one.Artifacts[0] != "pull/42" {
		t.Fatalf("turn 1 lost its artifacts: %+v", one)
	}
	// Where a turn came from reaches the app, so a migrated exchange is not
	// shown as if the session had written it here.
	if one.Origin != "user_request" || page.Turns[1].Origin != "app_request" {
		t.Fatalf("origins lost: %q %q", one.Origin, page.Turns[1].Origin)
	}
	two := page.Turns[1]
	if two.Question == nil || len(two.Choices) != 1 || two.Choices[0].RequestText != "코멘트 전부 반영해 줘" {
		t.Fatalf("turn 2 lost its question or choices: %+v", two)
	}

	// open=true is the parent's queue: the finished turn drops out.
	status, open := getWorkTurns(t, srv, s.ID, "task-moved", "open=true")
	if status != http.StatusOK || len(open.Turns) != 1 || open.Turns[0].ID != 2 {
		t.Fatalf("open turns = %d %+v", status, open)
	}

	// The keyset cursor pages without repeating or skipping a turn.
	status, first := getWorkTurns(t, srv, s.ID, "task-moved", "limit=1")
	if status != http.StatusOK || len(first.Turns) != 1 || first.NextCursor != 1 {
		t.Fatalf("first page = %d %+v", status, first)
	}
	status, second := getWorkTurns(t, srv, s.ID, "task-moved", "limit=1&after_turn_id=1")
	if status != http.StatusOK || len(second.Turns) != 1 || second.Turns[0].ID != 2 {
		t.Fatalf("second page = %d %+v", status, second)
	}
}

// A task with no turns yet answers an empty page; a task the slot does not have
// is not found. An empty page for an unknown id would read as "nothing has
// happened yet" and hide a wrong id.
func TestWorkTurns_EmptyIsNotTheSameAsUnknown(t *testing.T) {
	srv, stub := ledgerServer(t)
	workFixture(stub)
	_, s := createRaw(t, srv.URL, map[string]any{"name": "empty", "workloadType": "codex-chat"})

	status, page := getWorkTurns(t, srv, s.ID, "task-fresh", "")
	if status != http.StatusOK || len(page.Turns) != 0 {
		t.Fatalf("a task with no turns = %d %+v", status, page)
	}
	if status, _ := getWorkTurns(t, srv, s.ID, "no-such-task", ""); status != http.StatusNotFound {
		t.Fatalf("unknown task = %d, want 404", status)
	}
}

// Scope and shape refusals, the same ones the ledger routes make: a page size
// out of range is refused rather than clamped, a status outside the domain is
// refused rather than answered with an empty page, a workload type that keeps
// no work records says so, and another tenant's session does not exist.
func TestWorkRoutes_RefusalsAndScope(t *testing.T) {
	srv, stub := ledgerServer(t)
	workFixture(stub)
	_, s := createRaw(t, srv.URL, map[string]any{"name": "refusals", "workloadType": "codex-chat"})

	for _, query := range []string{"limit=0", "limit=101", "limit=-5", "limit=lots"} {
		if status, _ := getWorkTasks(t, srv, s.ID, query); status != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400", query, status)
		}
	}
	if status, _ := getWorkTasks(t, srv, s.ID, "status=nonsense"); status != http.StatusBadRequest {
		t.Fatalf("a status outside the domain was accepted")
	}
	if status, _ := getWorkTurns(t, srv, s.ID, "task-moved", "after_turn_id=-1"); status != http.StatusBadRequest {
		t.Fatalf("a negative cursor was accepted")
	}
	if status, _ := getWorkTurns(t, srv, s.ID, "task-moved", "open=maybe"); status != http.StatusBadRequest {
		t.Fatalf("open=maybe was accepted")
	}
	// status filters that are in the domain are answered.
	if status, page := getWorkTasks(t, srv, s.ID, "status=blocked"); status != http.StatusOK ||
		len(page.Tasks) != 1 || page.Tasks[0].Task.ID != "task-moved" {
		t.Fatalf("status filter = %d %+v", status, page)
	}
	// session_id="none" is how a caller asks for the unassigned ones.
	if status, page := getWorkTasks(t, srv, s.ID, "session_id=worker-1"); status != http.StatusOK ||
		len(page.Tasks) != 1 || page.Tasks[0].Task.ID != "task-fresh" {
		t.Fatalf("session filter = %d %+v", status, page)
	}

	// A session of a type that keeps no work records.
	_, shell := createRaw(t, srv.URL, map[string]any{"name": "shell", "workloadType": "shell"})
	if status, _ := getWorkTasks(t, srv, shell.ID, ""); status != http.StatusNotImplemented {
		t.Fatalf("a shell session = %d, want 501", status)
	}
	if status, _ := getWorkTasks(t, srv, "sess-does-not-exist", ""); status != http.StatusNotFound {
		t.Fatalf("an absent session = %d, want 404", status)
	}

	// Reading the work records is passive: it never became a write.
	if stub.WorkReads == 0 {
		t.Fatal("the stub served no work page")
	}
	if len(stub.LedgerRequests) != 0 {
		t.Fatalf("reading work stored something: %+v", stub.LedgerRequests)
	}
}
