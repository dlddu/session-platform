package main

import (
	"strings"
	"testing"
)

// bookWithDetails writes one task's bookkeeping with the details JSON a test
// wants on it. The rest of the record is the parent's ordinary notes: what is
// under test is the details, so the fields beside it are the same every time.
func bookWithDetails(t *testing.T, s *sessionDB, taskID, requestID string, details map[string]any) {
	t.Helper()
	mustWork(t, s, taskBookPutTool, map[string]any{
		"task_id": taskID, "request_id": requestID, "record": map[string]any{
			"stage": "collect", "owner": "부모 1", "sessions": []any{}, "details": details,
		},
	})
}

// backfilled indexes one run's answer by task id, so a test asserts about the
// task it means rather than about a position in a list.
func backfilled(t *testing.T, answer map[string]any, key string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, row := range answer[key].([]any) {
		entry := row.(map[string]any)
		out[entry["id"].(string)] = entry
	}
	return out
}

// cancelled is a status of its own, and it is additive: every status a slot
// already held still reads as itself, and the new one is selectable the same
// way the others are.
func TestCancelledIsItsOwnSelectableStatus(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": "cancelled"})
	newWorkTask(t, s, "task-b", "c2", map[string]any{"status": "completed"})
	page := mustWork(t, s, "task_list", map[string]any{"status": "cancelled"})
	tasks := page["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != "task-a" {
		t.Fatalf("status=cancelled selected %v, want task-a alone", tasks)
	}
	// Completed is untouched by the addition: a reader asking for delivered work
	// gets the task whose criteria were met and not the one that was called off.
	page = mustWork(t, s, "task_list", map[string]any{"status": "completed"})
	tasks = page["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != "task-b" {
		t.Fatalf("status=completed selected %v, want task-b alone", tasks)
	}
	// A status outside the domain is still refused, and the refusal names the
	// whole domain including the new member.
	decoded, code := workCall(t, s, "task_list", map[string]any{"status": "aborted"})
	if code != "invalid_argument" || !strings.Contains(decoded["error"].(string), "cancelled") {
		t.Fatalf("task_list status=aborted = %q %v, want the domain named", code, decoded)
	}
}

// The one-off writes the status only where the bookkeeping says so in a
// structured field. Everything else — prose, a string that reads as
// cancellation, a title prefix, two fields that disagree — is left alone, and
// the ones that are a decision rather than silence are reported.
func TestCancelBackfillWritesOnlyStructuredStatements(t *testing.T) {
	s, _ := newTestSessionDB(t)
	// Recorded as a boolean, on a task that had been written as delivered and
	// decorated so a person could see it anyway. This is the case the one-off
	// exists for.
	newWorkTask(t, s, "t1-flag", "c1", map[string]any{
		"status": "completed", "title": "[취소] 결제 재시도",
	})
	bookWithDetails(t, s, "t1-flag", "b1", map[string]any{"cancelled": true})
	// Recorded as an object beside the task, on work that was still running.
	newWorkTask(t, s, "t2-note", "c2", map[string]any{
		"status": "running", "title": "영수증 메일 재발송",
	})
	bookWithDetails(t, s, "t2-note", "b2", map[string]any{
		"cancellation": map[string]any{"at": "2026-11-20T01:00:00Z", "by": "부모 1", "reason": "요구가 바뀜"},
	})
	// An explicit no is a statement too, and it is not silence.
	newWorkTask(t, s, "t3-not", "c3", map[string]any{"status": "running"})
	bookWithDetails(t, s, "t3-not", "b3", map[string]any{"cancelled": false})
	// Prose is not a statement. This is the line the rule is drawn at: a
	// sentence a parent wrote for itself is not a machine-readable flag.
	newWorkTask(t, s, "t4-prose", "c4", map[string]any{"status": "blocked"})
	bookWithDetails(t, s, "t4-prose", "b4", map[string]any{"note": "사용자가 취소했다고 함"})
	// A title prefix is not a statement either, and this is the task that would
	// be wrecked if it were: it is *about* cancellation.
	newWorkTask(t, s, "t5-title", "c5", map[string]any{
		"status": "review", "title": "[취소] 취소 흐름 점검",
	})
	// A string that reads as cancellation is the wrong shape, so it is reported
	// rather than read.
	newWorkTask(t, s, "t6-string", "c6", map[string]any{"status": "running"})
	bookWithDetails(t, s, "t6-string", "b6", map[string]any{"cancelled": "true"})
	// Two fields that disagree are a disagreement to be told about, not one to
	// pick a side in.
	newWorkTask(t, s, "t7-both", "c7", map[string]any{"status": "running"})
	bookWithDetails(t, s, "t7-both", "b7", map[string]any{
		"cancelled": false, "cancellation": map[string]any{"by": "부모 2"},
	})
	// A title that is nothing but the prefix: a task has to have a title, so
	// this one keeps the one it has and the run says it did.
	newWorkTask(t, s, "t8-bare", "c8", map[string]any{"status": "planned", "title": "[취소]"})
	bookWithDetails(t, s, "t8-bare", "b8", map[string]any{"cancelled": true})

	answer := mustWork(t, s, taskCancelBackfillTool, map[string]any{"request_id": "run-1"})
	if answer["scanned"].(float64) != 8 {
		t.Fatalf("scanned = %v, want every task on the page", answer["scanned"])
	}
	wrote := backfilled(t, answer, "cancelled")
	if len(wrote) != 3 {
		t.Fatalf("cancelled = %v, want t1-flag, t2-note and t8-bare alone", wrote)
	}
	// The decorated title is cleaned and the title it had is in the answer.
	flag := wrote["t1-flag"]
	if flag["from_status"] != "completed" || flag["evidence"] != "details.cancelled" {
		t.Fatalf("t1-flag = %v", flag)
	}
	if flag["title"] != "결제 재시도" || flag["original_title"] != "[취소] 결제 재시도" {
		t.Fatalf("t1-flag title = %v", flag)
	}
	// The object says which field spoke, and an undecorated title is not
	// reported as having had something taken off it.
	note := wrote["t2-note"]
	if note["from_status"] != "running" || note["evidence"] != "details.cancellation" {
		t.Fatalf("t2-note = %v", note)
	}
	if note["title"] != "영수증 메일 재발송" || note["original_title"] != nil {
		t.Fatalf("t2-note title = %v", note)
	}
	bare := wrote["t8-bare"]
	if bare["title"] != "[취소]" || bare["title_kept"] != true || bare["original_title"] != nil {
		t.Fatalf("t8-bare = %v", bare)
	}
	// One of the three had been counted as delivered. That number is the whole
	// reason this status is not a flavour of completed.
	if answer["counted_as_delivered"].(float64) != 1 {
		t.Fatalf("counted_as_delivered = %v, want the one written as completed", answer["counted_as_delivered"])
	}
	refused := backfilled(t, answer, "refused")
	if len(refused) != 2 {
		t.Fatalf("refused = %v, want the wrong shape and the disagreement", refused)
	}
	if !strings.Contains(refused["t6-string"]["reason"].(string), "JSON boolean") {
		t.Fatalf("t6-string = %v", refused["t6-string"])
	}
	if !strings.Contains(refused["t7-both"]["reason"].(string), "says false") {
		t.Fatalf("t7-both = %v", refused["t7-both"])
	}
	// What was left alone is still exactly as it was — including the status and
	// the title of the task that is merely about cancellation.
	for id, want := range map[string]string{
		"t3-not": "running", "t4-prose": "blocked", "t5-title": "review",
		"t6-string": "running", "t7-both": "running",
	} {
		task := mustWork(t, s, "task_get", map[string]any{"id": id})["task"].(map[string]any)
		if task["status"] != want || task["version"].(float64) != 1 {
			t.Fatalf("%s = %v, want %s at version 1", id, task, want)
		}
	}
	titled := mustWork(t, s, "task_get", map[string]any{"id": "t5-title"})
	if titled["task"].(map[string]any)["title"] != "[취소] 취소 흐름 점검" {
		t.Fatalf("a title was read as a flag: %v", titled)
	}
	if titled["original_title"] != nil {
		t.Fatalf("a task nothing was taken off carries an original title: %v", titled)
	}
	// The written task says cancelled, its version moved because a status is a
	// new state of the task, and the title it had is answered beside it.
	written := mustWork(t, s, "task_get", map[string]any{"id": "t1-flag"})
	task := written["task"].(map[string]any)
	if task["status"] != "cancelled" || task["title"] != "결제 재시도" {
		t.Fatalf("t1-flag after the run = %v", task)
	}
	if task["version"].(float64) != 2 {
		t.Fatalf("the status was written without moving the version: %v", task)
	}
	if written["original_title"] != "[취소] 결제 재시도" {
		t.Fatalf("the title it had was not kept: %v", written)
	}
}

// Cancelling says where a task stopped. It does not rewrite how it got there:
// an open turn stays open, and what the parent already handed to another system
// stays recorded.
func TestCancelBackfillLeavesTurnsAndDispatchAlone(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", map[string]any{"status": "running"})
	appended := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "t1", "expected_version": 1,
		"turn": map[string]any{"request": "이거 해 줘"},
	})["turn"].(map[string]any)
	turnID := appended["id"].(float64)
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "take-1", "expected_version": 1,
		"turn": map[string]any{"started_at": "2026-11-20T00:00:00Z"},
	})
	mustWork(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d1", "expected_version": 1,
		"operation_key": "op-1", "operation_target": "session/worker-1", "dispatch_state": "dispatched",
	})
	bookWithDetails(t, s, "task-a", "b1", map[string]any{"cancellation": map[string]any{}})
	mustWork(t, s, taskCancelBackfillTool, map[string]any{"request_id": "run-1"})

	task := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})["task"].(map[string]any)
	if task["status"] != "cancelled" {
		t.Fatalf("task = %v", task)
	}
	// The turn nobody answered is still a turn nobody answered. Ending it here
	// would write an end nobody gave.
	open := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a", "open": true})
	if len(open["turns"].([]any)) != 1 {
		t.Fatalf("cancelling closed an open turn: %v", open)
	}
	turn := open["turns"].([]any)[0].(map[string]any)
	if turn["ended_at"] != nil || turn["started_at"] == nil {
		t.Fatalf("the open turn changed: %v", turn)
	}
	// The operation the parent handed over happened, and calling the task off
	// does not unhappen it.
	row := workRow(t, s,
		`SELECT count(*) AS n FROM sp_work_turn_admin
		 WHERE turn_id=? AND operation_key='op-1' AND dispatch_state='dispatched'`, int64(turnID))
	if row == nil || row["n"].(int64) != 1 {
		t.Fatalf("the dispatch record was touched: %v", row)
	}
}

// A dry run writes nothing, a second real run has nothing left to write, and a
// page is resumed with the cursor it was given.
func TestCancelBackfillDryRunsPagesAndRepeats(t *testing.T) {
	s, _ := newTestSessionDB(t)
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		newWorkTask(t, s, id, "c-"+id, map[string]any{
			"status": "completed", "title": "[취소] " + id,
		})
		bookWithDetails(t, s, id, "b-"+id, map[string]any{"cancelled": true})
	}
	// A dry run reports exactly what it would do and leaves the records alone.
	dry := mustWork(t, s, taskCancelBackfillTool, map[string]any{
		"request_id": "dry-1", "dry_run": true,
	})
	if dry["dry_run"] != true || len(dry["cancelled"].([]any)) != 3 {
		t.Fatalf("dry run = %v", dry)
	}
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		task := mustWork(t, s, "task_get", map[string]any{"id": id})["task"].(map[string]any)
		if task["status"] != "completed" || task["title"] != "[취소] "+id {
			t.Fatalf("a dry run wrote %s: %v", id, task)
		}
	}
	// One task at a time, resumed with the cursor the page gave back.
	first := mustWork(t, s, taskCancelBackfillTool, map[string]any{"request_id": "page-1", "limit": 1})
	if len(first["cancelled"].([]any)) != 1 || first["next_cursor"] != "task-a" {
		t.Fatalf("first page = %v", first)
	}
	second := mustWork(t, s, taskCancelBackfillTool, map[string]any{
		"request_id": "page-2", "limit": 1, "after_id": first["next_cursor"],
	})
	if len(second["cancelled"].([]any)) != 1 || second["next_cursor"] != "task-b" {
		t.Fatalf("second page = %v", second)
	}
	// The last page reaches the end, so it hands back no cursor.
	last := mustWork(t, s, taskCancelBackfillTool, map[string]any{
		"request_id": "page-3", "limit": 1, "after_id": second["next_cursor"],
	})
	if len(last["cancelled"].([]any)) != 1 || last["next_cursor"] != "" {
		t.Fatalf("last page = %v", last)
	}
	// Run again over everything: all three are already cancelled, nothing is
	// written a second time, and the title kept for the first run is still the
	// decorated one rather than the cleaned one.
	repeat := mustWork(t, s, taskCancelBackfillTool, map[string]any{"request_id": "run-again"})
	if repeat["already_cancelled"].(float64) != 3 || len(repeat["cancelled"].([]any)) != 0 {
		t.Fatalf("second run = %v", repeat)
	}
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		answer := mustWork(t, s, "task_get", map[string]any{"id": id})
		task := answer["task"].(map[string]any)
		if task["status"] != "cancelled" || task["title"] != id {
			t.Fatalf("%s after two runs = %v", id, task)
		}
		if task["version"].(float64) != 2 {
			t.Fatalf("%s was written twice: %v", id, task)
		}
		if answer["original_title"] != "[취소] "+id {
			t.Fatalf("%s lost the title it had: %v", id, answer)
		}
	}
	// The same request_id replays the answer it committed; the same id with
	// different arguments is a mistake rather than a second run.
	again := mustWork(t, s, taskCancelBackfillTool, map[string]any{"request_id": "run-again"})
	if again["already_cancelled"].(float64) != 3 {
		t.Fatalf("replay = %v", again)
	}
	wantWorkCode(t, s, taskCancelBackfillTool, map[string]any{
		"request_id": "run-again", "limit": 1,
	}, "conflict")
	// Pagination and the cursor are validated the way every other page is.
	wantWorkCode(t, s, taskCancelBackfillTool, map[string]any{
		"request_id": "bad-1", "limit": workListLimitMax + 1,
	}, "invalid_argument")
	wantWorkCode(t, s, taskCancelBackfillTool, map[string]any{
		"request_id": "bad-2", "after_id": "has space",
	}, "invalid_argument")
}

// A task with no bookkeeping at all is silence rather than a decision, so it is
// scanned and passed over without being reported as refused.
func TestCancelBackfillSaysNothingAboutTasksWithNoBookkeeping(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	answer := mustWork(t, s, taskCancelBackfillTool, map[string]any{"request_id": "run-1"})
	if answer["scanned"].(float64) != 1 {
		t.Fatalf("scanned = %v", answer["scanned"])
	}
	if len(answer["cancelled"].([]any)) != 0 || len(answer["refused"].([]any)) != 0 {
		t.Fatalf("a task with no bookkeeping was acted on or reported: %v", answer)
	}
	if answer["next_cursor"] != "" {
		t.Fatalf("a finished scan handed back a cursor: %v", answer)
	}
}
