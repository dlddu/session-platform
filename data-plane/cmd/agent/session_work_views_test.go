package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// workViewGet calls one of the work views the way the control plane does —
// from loopback, GET only.
func workViewGet(t *testing.T, h http.Handler, path, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := path
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The page shapes a reader outside the session decodes. They are declared here
// rather than reused from the server so that a field quietly disappearing from
// the response is a failing test rather than a shared struct change.
type workTurnsPage struct {
	TaskID      string `json:"task_id"`
	TaskVersion int64  `json:"task_version"`
	Turns       []struct {
		ID          int64   `json:"id"`
		Request     *string `json:"request"`
		Instruction *string `json:"instruction"`
		Response    *string `json:"response"`
		Explanation *string `json:"explanation"`
		Question    *string `json:"question"`
		Choices     []struct {
			Label       string `json:"label"`
			RequestText string `json:"request_text"`
		} `json:"choices"`
		Artifacts   []string `json:"artifacts"`
		RequestedAt string   `json:"requested_at"`
		StartedAt   *string  `json:"started_at"`
		EndedAt     *string  `json:"ended_at"`
		Origin      string   `json:"origin"`
	} `json:"turns"`
}

type workTasksPage struct {
	Tasks []struct {
		Task struct {
			ID                 string  `json:"id"`
			Title              string  `json:"title"`
			Description        *string `json:"description"`
			CompletionCriteria *string `json:"completion_criteria"`
			Status             string  `json:"status"`
			Version            int64   `json:"version"`
		} `json:"task"`
		TurnsTotal int64 `json:"turns_total"`
		LatestTurn *struct {
			ID       int64   `json:"id"`
			Question *string `json:"question"`
		} `json:"latest_turn"`
		Migrated bool `json:"migrated"`
	} `json:"tasks"`
	NextCursor string `json:"next_cursor"`
}

func workViewHandler(t *testing.T, s *sessionDB) http.Handler {
	t.Helper()
	proxy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	return withSessionDBMCP(proxy, s)
}

// AC-N9 read side, end to end through the views the control plane reads: the
// agreed model has to actually arrive out here. A page that could not carry a
// description, a completion criterion or anything a turn holds would show that
// work exists but never the work itself.
func TestWorkViewsCarryTheAgreedModelOutOfTheSession(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	newWorkTask(t, s, "task-fresh", "f1", map[string]any{
		"title": "새 작업", "description": "무엇을 why", "completion_criteria": "머지되면 끝",
	})

	// Two turns, written the way a parent writes them: the person's words
	// first, then what went to the session, then what came back and what the
	// parent made of it, then a question with choices.
	first := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-fresh", "request_id": "t1", "expected_version": 1,
		"turn": map[string]any{"request": "이거 해 줘"},
	})["turn"].(map[string]any)
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-fresh", "turn_id": int64(first["id"].(float64)), "request_id": "t1p",
		"turn": map[string]any{
			"instruction": "저장소를 읽고 패치를 만들어라", "response": "패치를 만들었습니다",
			"explanation": "요청한 변경을 PR 로 올렸어요", "artifacts": []any{"pull/42"},
			"started_at": "2026-11-19T00:00:00Z", "ended_at": "2026-11-19T00:10:00Z",
		},
	})
	second := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-fresh", "request_id": "t2", "expected_version": 1,
		"turn": map[string]any{
			"request": "리뷰 반영도 해 줘", "question": "리뷰 코멘트 중 어디까지 반영할까요?",
			"choices": []any{
				map[string]any{"label": "전부", "request_text": "코멘트 전부 반영해 줘"},
				map[string]any{"label": "필수만", "request_text": "필수 코멘트만 반영해 줘"},
			},
		},
	})["turn"].(map[string]any)

	rec := workViewGet(t, h, sessionDBWorkTasksPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("work tasks view = %d %s", rec.Code, rec.Body.String())
	}
	var tasks workTasksPage
	if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if len(tasks.Tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks.Tasks)
	}
	record := tasks.Tasks[0]
	if record.Task.Description == nil || *record.Task.Description != "무엇을 why" {
		t.Fatalf("description did not reach the view: %+v", record.Task)
	}
	if record.Task.CompletionCriteria == nil || *record.Task.CompletionCriteria != "머지되면 끝" {
		t.Fatalf("completion_criteria did not reach the view: %+v", record.Task)
	}
	if record.TurnsTotal != 2 || record.LatestTurn == nil || record.LatestTurn.Question == nil {
		t.Fatalf("turn tail did not reach the view: %+v", record)
	}
	if record.Migrated {
		t.Fatal("a task created in this image is reported as migrated")
	}

	// The turn page carries every agreed turn field, in the order they happened.
	rec = workViewGet(t, h, sessionDBWorkTurnsPath, "task_id=task-fresh")
	if rec.Code != http.StatusOK {
		t.Fatalf("work turns view = %d %s", rec.Code, rec.Body.String())
	}
	var turns workTurnsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &turns); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if len(turns.Turns) != 2 || turns.Turns[0].ID >= turns.Turns[1].ID {
		t.Fatalf("turns = %+v", turns.Turns)
	}
	one, two := turns.Turns[0], turns.Turns[1]
	for field, got := range map[string]*string{
		"request": one.Request, "instruction": one.Instruction,
		"response": one.Response, "explanation": one.Explanation,
	} {
		if got == nil || *got == "" {
			t.Fatalf("turn 1 lost %s: %+v", field, one)
		}
	}
	if one.StartedAt == nil || one.EndedAt == nil || len(one.Artifacts) != 1 || one.Artifacts[0] != "pull/42" {
		t.Fatalf("turn 1 lost its times or artifacts: %+v", one)
	}
	if two.Question == nil || len(two.Choices) != 2 || two.Choices[0].RequestText != "코멘트 전부 반영해 줘" {
		t.Fatalf("turn 2 lost its question or choices: %+v", two)
	}
	if two.ID != int64(second["id"].(float64)) {
		t.Fatalf("turn 2 id = %d", two.ID)
	}
	// A turn this image wrote carries no origin, so a reader can tell work
	// written here from work that came out of a ledger.
	if one.Origin != "" || two.Origin != "" {
		t.Fatalf("a session-written turn claims an origin: %+v %+v", one, two)
	}

	// A task this slot does not have is 404, not an empty page: an empty page
	// reads as "no turns yet" and would hide a wrong id.
	if rec = workViewGet(t, h, sessionDBWorkTurnsPath, "task_id=no-such"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task = %d %s", rec.Code, rec.Body.String())
	}
	if rec = workViewGet(t, h, sessionDBWorkTurnsPath, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing task_id = %d", rec.Code)
	}
	if rec = workViewGet(t, h, sessionDBWorkTasksPath, "limit=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit out of range = %d", rec.Code)
	}

	// Not GET, and not from loopback: the slot's work records are the pod's.
	req := httptest.NewRequest(http.MethodPost, sessionDBWorkTasksPath, nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST on the work task view = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, sessionDBWorkTurnsPath+"?task_id=task-fresh", nil)
	req.RemoteAddr = "10.1.2.3:4567"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("off-pod read of the work turn view = %d", rec.Code)
	}
}

// A task with no turns at all is the ordinary case — a task has none until
// somebody writes one. The page has to serve it rather than fail on the
// aggregate over zero rows.
func TestWorkViewsServeTasksWithNoTurns(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	newWorkTask(t, s, "task-quiet", "q1", map[string]any{"title": "아직 아무 말도 없는 작업"})

	rec := workViewGet(t, h, sessionDBWorkTasksPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("work tasks view = %d %s", rec.Code, rec.Body.String())
	}
	var page workTasksPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].TurnsTotal != 0 || page.Tasks[0].LatestTurn != nil {
		t.Fatalf("a task with no turns = %+v", page.Tasks)
	}
	rec = workViewGet(t, h, sessionDBWorkTurnsPath, "task_id=task-quiet")
	if rec.Code != http.StatusOK {
		t.Fatalf("turns of a quiet task = %d %s", rec.Code, rec.Body.String())
	}
	var turns workTurnsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns.Turns) != 0 {
		t.Fatalf("turns = %+v, want an empty list rather than an error", turns.Turns)
	}
}

// A turn that came out of a ledger says so through the same view — which is the
// whole reason the origin is an owned record rather than part of the preserved
// copy the move dropped. The move itself is gone now (session_work_state.go),
// so the state under test is the one it left: the origin record stands on its
// own, and the view still reads it after the move's bookkeeping is dropped.
func TestWorkViewsNameTheOriginOfACarriedTurn(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	seedRetiredSlot(t, s)
	turn := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "t1", "expected_version": 1,
		"turn": map[string]any{"request": "이거 해 줘"},
	})["turn"].(map[string]any)
	workExec(t, s, "INSERT INTO sp_work_turn_origin (turn_id,origin,source_id,author) VALUES (?,?,?,?)",
		int64(turn["id"].(float64)), workTurnOriginUserRequest, "ledger-request-1", "person")

	rec := workViewGet(t, h, sessionDBWorkTurnsPath, "task_id=task-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("turns view = %d %s", rec.Code, rec.Body.String())
	}
	var turns workTurnsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns.Turns) == 0 || turns.Turns[0].Origin != workTurnOriginUserRequest {
		t.Fatalf("carried turns = %+v, want the origin kept", turns.Turns)
	}
	// The ledger never recorded an instruction or a session's own answer, so a
	// carried turn must not claim one.
	if turns.Turns[0].Instruction != nil || turns.Turns[0].Response != nil {
		t.Fatalf("a carried turn invented an instruction or a response: %+v", turns.Turns[0])
	}
}
