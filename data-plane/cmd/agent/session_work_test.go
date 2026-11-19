package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The work tools are exercised through tools/call arguments rather than through
// the Go structs: which fields a caller named is part of the contract (an
// absent field is left alone, an explicit null clears it), and only the JSON
// path carries that.
// fillWorkCASArgs supplies the compare-and-swap counters a write needs when the
// caller has not named them. It exists so a test about something else does not
// have to read the counters first; the exclusion contract itself is pinned by
// session_work_cas_test.go, which always names them explicitly, and by the
// validation test that shows a missing counter is refused.
func fillWorkCASArgs(t *testing.T, s *sessionDB, name string, args map[string]any) {
	t.Helper()
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return
	}
	lane := ""
	switch name {
	case "turn_append", "turn_patch", "turn_dispatch":
		lane = "expected_turn_version"
	case workLegacyPutTool:
		lane = "expected_book_version"
	default:
		return
	}
	if _, named := args[lane]; named {
		return
	}
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := migrateWork(ctx, tx); err != nil {
		t.Fatal(err)
	}
	turn, book, err := workGuards(ctx, tx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if lane == "expected_turn_version" {
		args[lane] = turn
	} else {
		args[lane] = book
	}
}

func workCall(t *testing.T, s *sessionDB, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	fillWorkCASArgs(t, s, name, args)
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, rpcErr := s.callWork(context.Background(), name, raw)
	if rpcErr != nil {
		t.Fatalf("%s: JSON-RPC error %+v", name, rpcErr)
	}
	result := out.(map[string]any)
	text := result["content"].([]map[string]any)[0]["text"].(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("%s: %v (%s)", name, err, text)
	}
	if result["isError"] == true {
		code, _ := decoded["code"].(string)
		return decoded, code
	}
	return decoded, ""
}

func mustWork(t *testing.T, s *sessionDB, name string, args map[string]any) map[string]any {
	t.Helper()
	decoded, code := workCall(t, s, name, args)
	if code != "" {
		t.Fatalf("%s: %s %v", name, code, decoded["error"])
	}
	return decoded
}

func wantWorkCode(t *testing.T, s *sessionDB, name string, args map[string]any, want string) {
	t.Helper()
	decoded, code := workCall(t, s, name, args)
	if code != want {
		t.Fatalf("%s = %q %v, want %s", name, code, decoded, want)
	}
}

func newWorkTask(t *testing.T, s *sessionDB, id, requestID string, fields map[string]any) map[string]any {
	t.Helper()
	task := map[string]any{
		"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
		"completion_criteria": "PR 이 머지되면 끝", "status": "planned",
	}
	for key, value := range fields {
		task[key] = value
	}
	return mustWork(t, s, "task_put", map[string]any{
		"id": id, "request_id": requestID, "expected_version": 0, "task": task,
	})["task"].(map[string]any)
}

func TestWorkTaskCRUDVersionAndRetire(t *testing.T) {
	s, _ := newTestSessionDB(t)
	created := newWorkTask(t, s, "task-a", "c1", nil)
	if created["version"].(float64) != 1 || created["created_at"] == nil || created["updated_at"] == nil {
		t.Fatalf("created = %v", created)
	}
	if created["session_id"] != "worker-1" || created["completion_criteria"] != "PR 이 머지되면 끝" {
		t.Fatalf("created = %v", created)
	}
	// A create is replayed, not repeated.
	again := mustWork(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "c1", "expected_version": 0, "task": map[string]any{
			"session_id": "worker-1", "title": "작업 '하나'", "description": "목적과 요구",
			"completion_criteria": "PR 이 머지되면 끝", "status": "planned",
		},
	})["task"].(map[string]any)
	if again["version"].(float64) != 1 {
		t.Fatalf("replay = %v", again)
	}
	// The same request id with different arguments is a mistake, not a second
	// write.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "c1", "expected_version": 0, "task": map[string]any{
			"session_id": "worker-1", "title": "다른 제목", "description": "", "completion_criteria": "", "status": "planned",
		},
	}, "conflict")
	// An unassigned task is null, not an empty string.
	unassigned := mustWork(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "u1", "expected_version": 1, "task": map[string]any{
			"session_id": nil, "title": "작업 '하나'", "description": "", "completion_criteria": "", "status": "running",
		},
	})["task"].(map[string]any)
	if unassigned["session_id"] != nil || unassigned["version"].(float64) != 2 || unassigned["status"] != "running" {
		t.Fatalf("unassigned = %v", unassigned)
	}
	if unassigned["created_at"] != created["created_at"] {
		t.Fatalf("created_at moved on replacement: %v -> %v", created["created_at"], unassigned["created_at"])
	}
	// A stale version loses.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "stale", "expected_version": 1, "task": map[string]any{
			"session_id": nil, "title": "x", "description": "", "completion_criteria": "", "status": "running",
		},
	}, "conflict")
	mustWork(t, s, "task_delete", map[string]any{"id": "task-a", "request_id": "d1", "expected_version": 2})
	wantWorkCode(t, s, "task_get", map[string]any{"id": "task-a"}, "not_found")
	// A retired id is not revivable: a person removed that task.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-a", "request_id": "revive", "expected_version": 0, "task": map[string]any{
			"session_id": nil, "title": "x", "description": "", "completion_criteria": "", "status": "planned",
		},
	}, "conflict")
}

func TestWorkTaskListFilters(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": "running"})
	newWorkTask(t, s, "task-b", "c2", map[string]any{"session_id": nil, "status": "blocked"})
	newWorkTask(t, s, "task-c", "c3", map[string]any{"session_id": "worker-2"})
	page := mustWork(t, s, "task_list", map[string]any{"session_id": "none"})
	tasks := page["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != "task-b" {
		t.Fatalf("session_id=none selected %v", tasks)
	}
	page = mustWork(t, s, "task_list", map[string]any{"status": "running"})
	if len(page["tasks"].([]any)) != 1 {
		t.Fatalf("status filter = %v", page["tasks"])
	}
	// One page at a time, cursor by id.
	first := mustWork(t, s, "task_list", map[string]any{"limit": 2})
	if len(first["tasks"].([]any)) != 2 || first["next_cursor"] != "task-b" {
		t.Fatalf("first page = %v", first)
	}
	second := mustWork(t, s, "task_list", map[string]any{"limit": 2, "after_id": first["next_cursor"]})
	if len(second["tasks"].([]any)) != 1 || second["next_cursor"] != "" {
		t.Fatalf("second page = %v", second)
	}
	wantWorkCode(t, s, "task_list", map[string]any{"limit": 101}, "invalid_argument")
	wantWorkCode(t, s, "task_list", map[string]any{"status": "almost-done"}, "invalid_argument")
}

// A turn is written as it happens: the request arrives, the parent passes an
// instruction on, the session answers, and the parent explains it. Each step
// touches its own fields and leaves the rest alone.
func TestWorkTurnLifecycleAndOpenQueue(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	appended := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "t1", "expected_version": 1,
		"turn": map[string]any{"request": "이거 해 줘"},
	})["turn"].(map[string]any)
	turnID := appended["id"].(float64)
	if turnID < 1 || appended["instruction"] != nil || appended["response"] != nil ||
		appended["started_at"] != nil || appended["ended_at"] != nil {
		t.Fatalf("appended = %v", appended)
	}
	if len(appended["choices"].([]any)) != 0 || len(appended["artifacts"].([]any)) != 0 {
		t.Fatalf("appended = %v", appended)
	}
	open := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a", "open": true})
	if len(open["turns"].([]any)) != 1 {
		t.Fatalf("open queue = %v", open)
	}
	// The parent takes the turn first: only whoever holds it may act for it.
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "take-1", "expected_version": 1,
		"turn": map[string]any{"started_at": "2026-11-19T00:00:00Z"},
	})
	// Dispatch is recorded beside the turn, before the outside action.
	mustWork(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d1", "expected_version": 1,
		"operation_key": "op-1", "operation_target": "session/worker-1", "dispatch_state": "intended",
	})
	// A different key on a turn that already recorded one is refused: the first
	// may have landed outside this database.
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d2", "expected_version": 1,
		"operation_key": "op-2", "dispatch_state": "dispatched",
	}, "conflict")
	mustWork(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d3", "expected_version": 1,
		"operation_key": "op-2", "observed": "not_accepted", "dispatch_state": "intended",
	})
	patched := mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "p1", "expected_version": 1,
		"turn": map[string]any{
			// started_at is not written again: the turn was taken above, and
			// taking a turn somebody already holds is refused.
			"instruction": "담당 세션에게 전달한 지시",
			"question":    "둘 중 어느 쪽?", "choices": []any{
				map[string]any{"label": "A 로", "request_text": "A 로 해 줘"},
				map[string]any{"label": "B 로", "request_text": "B 로 해 줘"},
			},
		},
	})["turn"].(map[string]any)
	if patched["request"] != "이거 해 줘" || patched["instruction"] != "담당 세션에게 전달한 지시" {
		t.Fatalf("patched = %v", patched)
	}
	if len(patched["choices"].([]any)) != 2 || patched["question"] != "둘 중 어느 쪽?" {
		t.Fatalf("patched = %v", patched)
	}
	// Filling the answer in does not erase the question asked on the same turn.
	answered := mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "p2", "expected_version": 1,
		"turn": map[string]any{
			"response": "담당 세션의 최종 응답 원문", "explanation": "부모의 사용자 설명",
			"session_seq": 7, "artifacts": []any{"https://github.com/dlddu/x/pull/1"},
			"ended_at": "2026-11-19T12:30:00Z",
		},
	})["turn"].(map[string]any)
	if answered["question"] != "둘 중 어느 쪽?" || len(answered["choices"].([]any)) != 2 {
		t.Fatalf("answering cleared the question or its choices: %v", answered)
	}
	if answered["response"] != "담당 세션의 최종 응답 원문" || answered["explanation"] != "부모의 사용자 설명" {
		t.Fatalf("answered = %v", answered)
	}
	if answered["session_seq"].(float64) != 7 || len(answered["artifacts"].([]any)) != 1 {
		t.Fatalf("answered = %v", answered)
	}
	// An ended turn has left the queue.
	open = mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a", "open": true})
	if len(open["turns"].([]any)) != 0 {
		t.Fatalf("open queue after the answer = %v", open)
	}
	// An explicit null clears one field and only that one.
	cleared := mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "p3", "expected_version": 1,
		"turn": map[string]any{"question": nil, "choices": nil},
	})["turn"].(map[string]any)
	if cleared["question"] != nil || len(cleared["choices"].([]any)) != 0 || cleared["response"] == nil {
		t.Fatalf("cleared = %v", cleared)
	}
	// A turn write holds the task's version but does not spend it.
	task := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})
	if task["task"].(map[string]any)["version"].(float64) != 1 {
		t.Fatalf("a turn write moved the task version: %v", task)
	}
	if task["turns_total"].(float64) != 1 || task["latest_turn"] == nil {
		t.Fatalf("task_get = %v", task)
	}
	wantWorkCode(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "t9", "expected_version": 2,
		"turn": map[string]any{"request": "x"},
	}, "conflict")
	wantWorkCode(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID + 1000, "request_id": "p9", "expected_version": 1,
		"turn": map[string]any{"response": "x"},
	}, "not_found")
}

// Turn ids rise across the whole database, which is what makes ordering by id
// the order things happened — in one task and between tasks.
func TestWorkTurnIDsAreGlobalAndOrdered(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	newWorkTask(t, s, "task-b", "c2", nil)
	ids := []float64{}
	for i, task := range []string{"task-a", "task-b", "task-a", "task-b"} {
		appended := mustWork(t, s, "turn_append", map[string]any{
			"task_id": task, "request_id": "t" + string(rune('0'+i)), "expected_version": 1,
			"turn": map[string]any{"request": "r"},
		})["turn"].(map[string]any)
		ids = append(ids, appended["id"].(float64))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("turn ids did not rise across tasks: %v", ids)
		}
	}
	page := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})
	turns := page["turns"].([]any)
	if len(turns) != 2 ||
		turns[0].(map[string]any)["id"].(float64) != ids[0] ||
		turns[1].(map[string]any)["id"].(float64) != ids[2] {
		t.Fatalf("task-a turns = %v", turns)
	}
	// A turn id that belongs to another task is not on this one.
	wantWorkCode(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": ids[1], "request_id": "cross", "expected_version": 1,
		"turn": map[string]any{"response": "x"},
	}, "not_found")
}

// A turn needs a task: the database refuses an orphan rather than keeping a
// record no reader can place.
func TestWorkTurnRequiresItsTask(t *testing.T) {
	s, _ := newTestSessionDB(t)
	wantWorkCode(t, s, "turn_append", map[string]any{
		"task_id": "absent", "request_id": "t1", "expected_version": 1,
		"turn": map[string]any{"request": "r"},
	}, "not_found")
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newWorkTask(t, s, "task-a", "c1", nil)
	if _, err := db.ExecContext(ctx,
		"INSERT INTO sp_work_turns (task_id,requested_at) VALUES ('ghost','2026-11-19T00:00:00Z')"); err == nil {
		t.Fatal("a turn with no task was accepted")
	}
}

func TestWorkArgumentValidation(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	for name, args := range map[string]map[string]any{
		"unknown field": {"id": "task-a", "nonsense": 1},
		"missing id":    {},
	} {
		if _, code := workCall(t, s, "task_get", args); code != "invalid_argument" {
			t.Fatalf("%s = %q", name, code)
		}
	}
	// A turn field the schema does not have is refused rather than dropped.
	wantWorkCode(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "t1", "expected_version": 1,
		"turn": map[string]any{"request": "r", "wait_reason": "nope"},
	}, "invalid_argument")
	// A patch has to say what it changes.
	wantWorkCode(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": 1, "request_id": "p1", "expected_version": 1,
		"turn": map[string]any{},
	}, "invalid_argument")
	for _, turn := range []map[string]any{
		{"requested_at": "yesterday"},
		{"choices": []any{map[string]any{"label": "", "request_text": "x"}}},
		{"choices": []any{
			map[string]any{"label": "1", "request_text": "x"}, map[string]any{"label": "2", "request_text": "x"},
			map[string]any{"label": "3", "request_text": "x"}, map[string]any{"label": "4", "request_text": "x"},
			map[string]any{"label": "5", "request_text": "x"}, map[string]any{"label": "6", "request_text": "x"},
			map[string]any{"label": "7", "request_text": "x"},
		}},
		{"session_seq": -1},
	} {
		wantWorkCode(t, s, "turn_append", map[string]any{
			"task_id": "task-a", "request_id": "bad", "expected_version": 1, "turn": turn,
		}, "invalid_argument")
	}
	// The agreed task shape is a full replacement: every field is named.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-b", "request_id": "c2", "expected_version": 0,
		"task": map[string]any{"title": "x", "status": "planned"},
	}, "invalid_argument")
}

// The task and turn rows carry the agreed columns and nothing else: a field
// that was deliberately left out of the schema must not reappear.
func TestWorkSchemaHasOnlyTheAgreedColumns(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]string{
		"sp_work_tasks": "id,session_id,title,description,completion_criteria,status,version,created_at,updated_at",
		"sp_work_turns": "id,task_id,session_seq,request,instruction,response,explanation,question,choices," +
			"artifacts,requested_at,started_at,ended_at",
	} {
		rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		if err != nil {
			t.Fatal(err)
		}
		columns := []string{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			columns = append(columns, name)
		}
		rows.Close()
		if got := strings.Join(columns, ","); got != want {
			t.Fatalf("%s columns = %s, want %s", table, got, want)
		}
	}
	// The index the agreed shape asks for: a task's turns in id order.
	var indexed int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type='index' AND name='sp_work_turns_task'").Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != 1 {
		t.Fatal("sp_work_turns has no (task_id, id) index")
	}
}
