package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// What a person's stored request does to the status of the task it lands on: a
// task that was waiting on them is received as `planned`, and nothing else
// moves.

// taskStatusAndVersion reads the stored row rather than a view, because what is
// under test is what committed — a projection could agree with the test while
// the row said something else.
func taskStatusAndVersion(t *testing.T, s *sessionDB, taskID string) (string, int64) {
	t.Helper()
	row := workRow(t, s, "SELECT status,version FROM sp_work_tasks WHERE id=?", taskID)
	if row == nil {
		t.Fatalf("task %s is not on this slot", taskID)
	}
	status, ok := row["status"].(string)
	if !ok {
		t.Fatalf("status = %#v", row["status"])
	}
	version, ok := row["version"].(int64)
	if !ok {
		t.Fatalf("version = %#v", row["version"])
	}
	return status, version
}

// decodeWorkTasksPage reads one task page the way the control plane does, and
// fails on a status the view did not answer with 200 rather than on an empty
// page further down.
func decodeWorkTasksPage(t *testing.T, rec *httptest.ResponseRecorder, into *workTasksPage) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("the task view answered %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatal(err)
	}
}

func taskTurnCount(t *testing.T, s *sessionDB, taskID string) int64 {
	t.Helper()
	row := workRow(t, s, "SELECT count(*) AS n FROM sp_work_turns WHERE task_id=?", taskID)
	return row["n"].(int64)
}

// A request on a task that was waiting on the person is its receipt: the turn
// and the new status land in one commit, and `planned` is where the task waits
// to be picked up rather than anything having been approved or started.
func TestWorkRequestOnAReviewTaskIsReceivedAsPlanned(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})
	version, turnVersion := turnCursor(t, s, "task-review")
	book := int64(mustWork(t, s, taskBookGetTool,
		map[string]any{"task_id": "task-review"})["book_version"].(float64))

	stored, replayed, err := storeWorkRequest(t, s, "req-1", "task-review", "이 선택지로 해 주세요", nil)
	if err != nil || replayed {
		t.Fatalf("store = %v replayed=%v", err, replayed)
	}
	if stored.Request == nil || *stored.Request != "이 선택지로 해 주세요" {
		t.Fatalf("the stored turn is not the person's words: %+v", stored)
	}
	if stored.EndedAt != nil || stored.StartedAt != nil {
		t.Fatalf("the receipt started or ended the turn: %+v", stored)
	}

	status, moved := taskStatusAndVersion(t, s, "task-review")
	if status != "planned" {
		t.Fatalf("status = %q, want planned — the request is received and waiting to be picked up", status)
	}
	// The status is a new state of the task, so the version moves with it: a
	// monitor holding what it read before the request landed has to read again
	// rather than write a status over the receipt.
	if moved != version+1 {
		t.Fatalf("version = %d, want %d", moved, version+1)
	}
	if got := taskTurnCount(t, s, "task-review"); got != 1 {
		t.Fatalf("turns = %d, want the one request", got)
	}
	// The turn lane moved because a turn arrived; the parent's own bookkeeping
	// did not, because this path writes none of it.
	if _, nowTurnVersion := turnCursor(t, s, "task-review"); nowTurnVersion != turnVersion+1 {
		t.Fatalf("turn_version = %d, want %d", nowTurnVersion, turnVersion+1)
	}
	after := int64(mustWork(t, s, taskBookGetTool,
		map[string]any{"task_id": "task-review"})["book_version"].(float64))
	if after != book {
		t.Fatalf("book_version = %d, want %d — the receipt wrote the parent's bookkeeping", after, book)
	}
	// Received is not running: the only waiting turn is still open, which is
	// what tells the monitor there is something to pick up.
	open := workRow(t, s,
		"SELECT count(*) AS n FROM sp_work_turns WHERE task_id=? AND request IS NOT NULL AND ended_at IS NULL",
		"task-review")
	if open["n"].(int64) != 1 {
		t.Fatalf("open requests = %v, want the one nobody has picked up", open["n"])
	}
}

// Only the status a person was asked to settle moves. Everything else — work
// already running, work already finished, and the word an older writer still
// uses — is left exactly as it is, so a request can never send a task
// backwards.
func TestWorkRequestLeavesEveryOtherStatusAlone(t *testing.T) {
	s, _ := newTestSessionDB(t)
	for i, status := range []string{"planned", "running", "completed", "unknown", "blocked"} {
		taskID := "task-" + status
		newWorkTask(t, s, taskID, "c"+status, map[string]any{"status": status})
		_, version := taskStatusAndVersion(t, s, taskID)
		if _, _, err := storeWorkRequest(t, s, "req-"+status, taskID, "요청 하나", nil); err != nil {
			t.Fatalf("[%d] store on %s: %v", i, status, err)
		}
		got, nowVersion := taskStatusAndVersion(t, s, taskID)
		if got != status {
			t.Fatalf("a request moved %s to %q", status, got)
		}
		// A turn is an event on the task rather than a new state of it, so the
		// version stands still when no status moved.
		if nowVersion != version {
			t.Fatalf("[%s] version = %d, want %d — a turn alone moved the version", status, nowVersion, version)
		}
		if got := taskTurnCount(t, s, taskID); got != 1 {
			t.Fatalf("[%s] turns = %d", status, got)
		}
	}
}

// A retry of an answer the app never saw is the same request, not a second one
// — and it must not pull a task the monitor has already taken back to the queue.
func TestWorkRequestReplayDoesNotMoveATaskTheMonitorTook(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})
	if _, _, err := storeWorkRequest(t, s, "req-1", "task-review", "이어서 해 주세요", nil); err != nil {
		t.Fatal(err)
	}
	_, version := taskStatusAndVersion(t, s, "task-review")

	// The monitor reads what came back, checks it, and assigns the work. That
	// is the only thing that puts a task in running.
	mustWork(t, s, "task_put", map[string]any{
		"id": "task-review", "request_id": "m1", "expected_version": version, "task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": "running",
		},
	})

	// The app's POST answer was lost, so it sends the same id and the same
	// words again.
	stored, replayed, err := storeWorkRequest(t, s, "req-1", "task-review", "이어서 해 주세요", nil)
	if err != nil || !replayed {
		t.Fatalf("replay = %v replayed=%v, want the turn already appended", err, replayed)
	}
	if stored.Request == nil || *stored.Request != "이어서 해 주세요" {
		t.Fatalf("the replay answered with other words: %+v", stored)
	}
	status, _ := taskStatusAndVersion(t, s, "task-review")
	if status != "running" {
		t.Fatalf("status = %q, want running — a late retry reverted work the monitor had taken", status)
	}
	if got := taskTurnCount(t, s, "task-review"); got != 1 {
		t.Fatalf("turns = %d, want one — the replay stored a second turn", got)
	}
}

// Once received, a task stays received: a second request is another turn on the
// queue and does not move the status again.
func TestWorkSecondRequestOnAReceivedTaskMovesNothing(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})
	if _, _, err := storeWorkRequest(t, s, "req-1", "task-review", "첫 요청", nil); err != nil {
		t.Fatal(err)
	}
	_, version := taskStatusAndVersion(t, s, "task-review")
	if _, _, err := storeWorkRequest(t, s, "req-2", "task-review", "덧붙이는 말", nil); err != nil {
		t.Fatal(err)
	}
	status, nowVersion := taskStatusAndVersion(t, s, "task-review")
	if status != "planned" || nowVersion != version {
		t.Fatalf("status/version = %q/%d, want planned/%d", status, nowVersion, version)
	}
	if got := taskTurnCount(t, s, "task-review"); got != 2 {
		t.Fatalf("turns = %d, want both requests", got)
	}
}

// A refused store leaves the task where it was. The status move is part of the
// store's own transaction, so there is no state in which a task was received
// but the request was not.
func TestWorkRefusedRequestLeavesAReviewTaskInReview(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})
	version, turnVersion := turnCursor(t, s, "task-review")
	// A turn with choices, and then a newer turn — so the first turn's choices
	// are the ones a screen left open would still be holding.
	offered := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-review", "request_id": "t1", "expected_version": version,
		"expected_turn_version": turnVersion,
		"turn": map[string]any{
			"question": "어느 쪽으로 할까요?",
			"choices": []map[string]any{
				{"label": "A 안", "request_text": "A 안으로 해 주세요"},
			},
		},
	})
	staleTurn := int64(offered["turn"].(map[string]any)["id"].(float64))
	version, turnVersion = turnCursor(t, s, "task-review")
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-review", "request_id": "t2", "expected_version": version,
		"expected_turn_version": turnVersion,
		"turn":                  map[string]any{"explanation": "그 사이에 하나 더"},
	})
	before := taskTurnCount(t, s, "task-review")

	// A pick from the screen the task has moved past.
	_, _, err := storeWorkRequest(t, s, "req-stale", "task-review", "A 안으로 해 주세요", &staleTurn)
	var known workError
	if !asWorkError(err, &known) || known.Code != "stale_choice" {
		t.Fatalf("refusal = %v, want stale_choice", err)
	}
	status, _ := taskStatusAndVersion(t, s, "task-review")
	if status != "review" {
		t.Fatalf("status = %q, want review — a refused request moved the task", status)
	}
	if got := taskTurnCount(t, s, "task-review"); got != before {
		t.Fatalf("turns = %d, want %d", got, before)
	}

	// The same id with different words is a conflict rather than a second
	// turn, and it moves nothing either.
	if _, _, err = storeWorkRequest(t, s, "req-1", "task-review", "본문 하나", nil); err != nil {
		t.Fatal(err)
	}
	_, version = taskStatusAndVersion(t, s, "task-review")
	_, _, err = storeWorkRequest(t, s, "req-1", "task-review", "다른 본문", nil)
	if !asWorkError(err, &known) || known.Code != "conflict" {
		t.Fatalf("same id different words = %v, want conflict", err)
	}
	status, nowVersion := taskStatusAndVersion(t, s, "task-review")
	if status != "planned" || nowVersion != version {
		t.Fatalf("status/version = %q/%d after a conflict", status, nowVersion)
	}
}

// A write that fails after the status moved takes the move with it. The move is
// the last write of the store, so this drives two stores through one
// transaction and fails the second: nothing the first one wrote may survive.
func TestWorkRequestReceiptRollsBackWithTheTransaction(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txErr := s.workTx(ctx, db, func(tx *sql.Tx) error {
		if _, _, inner := storeRequestAsTurn(
			ctx, tx, "req-1", "task-review", "첫 요청", "user-1", nil, workClock()); inner != nil {
			t.Fatalf("the first store refused: %v", inner)
		}
		// Same id, different words: a conflict, and the transaction carries the
		// first store's status move out with it.
		_, _, inner := storeRequestAsTurn(
			ctx, tx, "req-1", "task-review", "다른 본문", "user-1", nil, workClock())
		return inner
	})
	var known workError
	if !asWorkError(txErr, &known) || known.Code != "conflict" {
		t.Fatalf("transaction = %v, want conflict", txErr)
	}
	status, version := taskStatusAndVersion(t, s, "task-review")
	if status != "review" || version != 1 {
		t.Fatalf("status/version = %q/%d, want review/1 — a rolled back receipt was committed", status, version)
	}
	if got := taskTurnCount(t, s, "task-review"); got != 0 {
		t.Fatalf("turns = %d, want none", got)
	}
}

// The receipt is about the task the request named. Another task in review on
// the same slot is not touched by it.
func TestWorkRequestReceiptIsScopedToItsOwnTask(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": "review"})
	newWorkTask(t, s, "task-b", "c2", map[string]any{"status": "review"})
	if _, _, err := storeWorkRequest(t, s, "req-1", "task-a", "A 작업 요청", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskStatusAndVersion(t, s, "task-a"); status != "planned" {
		t.Fatalf("task-a = %q", status)
	}
	if status, version := taskStatusAndVersion(t, s, "task-b"); status != "review" || version != 1 {
		t.Fatalf("task-b = %q/%d — a request on another task moved it", status, version)
	}
	if got := taskTurnCount(t, s, "task-b"); got != 0 {
		t.Fatalf("task-b turns = %d", got)
	}
	// A task this slot does not have is not found, and nothing is created for
	// it on the way to saying so.
	_, _, err := storeWorkRequest(t, s, "req-2", "task-missing", "없는 작업", nil)
	var known workError
	if !asWorkError(err, &known) || known.Code != "not_found" {
		t.Fatalf("store on an absent task = %v, want not_found", err)
	}
}

// What was asked survives the receipt. The question, the choices beside it and
// the task's own description and completion criteria are the account of why the
// person was asked, and the monitor's own handling of the answer still needs
// them — so sending a sentence settles nothing by itself.
func TestWorkRequestReceiptKeepsWhatWasAsked(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})
	version, turnVersion := turnCursor(t, s, "task-review")
	asked := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-review", "request_id": "t1", "expected_version": version,
		"expected_turn_version": turnVersion,
		"turn": map[string]any{
			"question":    "둘 중 어느 쪽으로 할까요?",
			"explanation": "권한이 없어 더 나아가지 못했습니다",
			"choices": []map[string]any{
				{"label": "A 안", "request_text": "A 안으로 해 주세요"},
				{"label": "B 안", "request_text": "B 안으로 해 주세요"},
			},
		},
	})
	askedID := int64(asked["turn"].(map[string]any)["id"].(float64))

	// The person presses a choice on the latest turn and sends it.
	if _, _, err := storeWorkRequest(t, s, "req-1", "task-review", "A 안으로 해 주세요", &askedID); err != nil {
		t.Fatal(err)
	}
	if status, _ := taskStatusAndVersion(t, s, "task-review"); status != "planned" {
		t.Fatalf("status = %q, want planned", status)
	}
	page, err := s.workTurnsView(context.Background(), workArgs{TaskID: "task-review"})
	if err != nil {
		t.Fatal(err)
	}
	decoded := page.(map[string]any)
	task := decoded["task"].(workTask)
	if task.Description == nil || task.CompletionCriteria == nil {
		t.Fatalf("the task lost what it is for: %+v", task)
	}
	turns := decoded["turns"].([]workTurnView)
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want the question and the answer", len(turns))
	}
	if turns[0].Question == nil || *turns[0].Question != "둘 중 어느 쪽으로 할까요?" {
		t.Fatalf("the question was rewritten: %+v", turns[0])
	}
	if len(turns[0].Choices) != 2 || turns[0].Choices[0].RequestText != "A 안으로 해 주세요" {
		t.Fatalf("the offered choices did not survive: %+v", turns[0].Choices)
	}
	if turns[0].Explanation == nil || *turns[0].Explanation != "권한이 없어 더 나아가지 못했습니다" {
		t.Fatalf("the basis for asking was dropped: %+v", turns[0])
	}
	// The question's turn is not closed by somebody answering it: ending it is
	// the monitor's own record of having dealt with it.
	if turns[0].EndedAt != nil {
		t.Fatalf("the receipt ended the turn the question was asked on: %+v", turns[0])
	}
	if turns[1].Origin != workTurnOriginAppRequest {
		t.Fatalf("the answer is not recorded as the person's own: %+v", turns[1])
	}
}

// Two stores of the same request racing each other leave one turn and one move.
func TestWorkRequestReceiptHappensOnceUnderARace(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			// Either outcome is correct for one of the two callers: a store, or
			// the turn already appended. What may not happen is two turns or
			// two moves.
			_, _, _ = storeWorkRequest(t, s, "req-1", "task-review", "같은 요청", nil)
		}()
	}
	wait.Wait()
	status, version := taskStatusAndVersion(t, s, "task-review")
	if status != "planned" || version != 2 {
		t.Fatalf("status/version = %q/%d, want planned/2 — the move ran twice", status, version)
	}
	if got := taskTurnCount(t, s, "task-review"); got != 1 {
		t.Fatalf("turns = %d, want one", got)
	}
}

// The whole way in, over the real route: the control plane's POST body reaches
// this slot's database and the task is received, and the retry of an answer the
// caller never saw answers 200 without moving anything a second time.
func TestWorkRequestReceiptOverTheSubmitRoute(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, sessionDBWorkRequestsPath, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	body := `{"id":"req-1","task_id":"task-review","text":"A 안으로 해 주세요","author":"user-7"}`

	rec := post(body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("store answered %d: %s", rec.Code, rec.Body.String())
	}
	var stored struct {
		Turn struct {
			ID      int64   `json:"id"`
			Request *string `json:"request"`
			Origin  string  `json:"origin"`
			EndedAt *string `json:"ended_at"`
		} `json:"turn"`
		Replayed bool `json:"replayed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Replayed || stored.Turn.Request == nil || *stored.Turn.Request != "A 안으로 해 주세요" ||
		stored.Turn.Origin != workTurnOriginAppRequest || stored.Turn.EndedAt != nil {
		t.Fatalf("the answer is not the open turn the request became: %+v", stored)
	}
	status, version := taskStatusAndVersion(t, s, "task-review")
	if status != "planned" || version != 2 {
		t.Fatalf("status/version = %q/%d, want planned/2", status, version)
	}

	// The caller never saw the 201 and sends the same request again.
	rec = post(body)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay answered %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Replayed {
		t.Fatalf("the replay did not say so: %+v", stored)
	}
	nowStatus, nowVersion := taskStatusAndVersion(t, s, "task-review")
	if nowStatus != status || nowVersion != version {
		t.Fatalf("the replay moved the task again: %q/%d", nowStatus, nowVersion)
	}
	if got := taskTurnCount(t, s, "task-review"); got != 1 {
		t.Fatalf("turns = %d, want one", got)
	}

	// A refusal over the same route leaves the task where the receipt put it.
	rec = post(`{"id":"req-1","task_id":"task-review","text":"다른 본문","author":"user-7"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("same id different words answered %d: %s", rec.Code, rec.Body.String())
	}
	if again, v := taskStatusAndVersion(t, s, "task-review"); again != "planned" || v != version {
		t.Fatalf("status/version = %q/%d after a refusal", again, v)
	}
}

// The reader outside the session sees the received status on its next read,
// over the same sidecar route the control plane calls.
func TestWorkRequestReceiptIsVisibleToTheReaderOutside(t *testing.T) {
	s, _ := newTestSessionDB(t)
	h := workViewHandler(t, s)
	newWorkTask(t, s, "task-review", "c1", map[string]any{"status": "review"})

	var page workTasksPage
	decodeWorkTasksPage(t, workViewGet(t, h, sessionDBWorkTasksPath, "status=review"), &page)
	if len(page.Tasks) != 1 {
		t.Fatalf("the review filter = %+v", page.Tasks)
	}

	if _, _, err := storeWorkRequest(t, s, "req-1", "task-review", "요청 하나", nil); err != nil {
		t.Fatal(err)
	}

	page = workTasksPage{}
	decodeWorkTasksPage(t, workViewGet(t, h, sessionDBWorkTasksPath, "status=review"), &page)
	if len(page.Tasks) != 0 {
		t.Fatalf("the task is still answered as review: %+v", page.Tasks)
	}
	page = workTasksPage{}
	decodeWorkTasksPage(t, workViewGet(t, h, sessionDBWorkTasksPath, "status=planned"), &page)
	if len(page.Tasks) != 1 || page.Tasks[0].Task.Status != "planned" {
		t.Fatalf("the planned filter = %+v", page.Tasks)
	}
	if page.Tasks[0].TurnsTotal != 1 || page.Tasks[0].LatestTurn == nil {
		t.Fatalf("the page does not carry the request that moved it: %+v", page.Tasks[0])
	}
}
