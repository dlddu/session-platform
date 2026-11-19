package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func legacyTaskArgs(id, requestID string, version int64, title, status string, sessions []ledgerSession, details map[string]any) ledgerArgs {
	return ledgerArgs{ID: id, RequestID: requestID, ExpectedVersion: lv(version), Task: &ledgerTask{
		Title: title, Status: status, Stage: "implementation", Owner: "parent",
		Sessions: sessions, Details: details,
	}}
}

func workRow(t *testing.T, s *sessionDB, query string, args ...any) map[string]any {
	t.Helper()
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		return nil
	}
	cells := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range cells {
		pointers[i] = &cells[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for i, name := range columns {
		out[name] = cells[i]
	}
	return out
}

func workExec(t *testing.T, s *sessionDB, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// seedLedger builds the fixture the migration is judged on: a task that has
// moved between sessions, a request nobody has picked up yet, a request that
// was dispatched and settled, and a monitor row that is the parent's own
// bookkeeping rather than a person's work.
func seedLedger(t *testing.T, s *sessionDB) {
	t.Helper()
	ctx := context.Background()
	mustLedger(t, s, "ledger_put", legacyTaskArgs("task-a", "la1", 0, "앱에 작업 패널 띄우기", "blocked",
		[]ledgerSession{
			{ID: "worker-old", Ownership: "owned", Purpose: "첫 구현", Cleanup: "deleted"},
			{ID: "worker-now", Ownership: "owned", Purpose: "이어서 구현", Cleanup: "pending", NextOffset: 4096, Seq: lv(12)},
			{ID: "reviewer", Ownership: "reused", Purpose: "리뷰", Cleanup: "retained", Reason: "후속 리뷰", NextAction: "리뷰 회수"},
		},
		map[string]any{"approval": "PR only", "preservation": "unpushed work on worker-now"}))
	mustLedger(t, s, "ledger_put", legacyTaskArgs("task-m", "lm1", 0, "모니터 루프 기록", "running",
		[]ledgerSession{{ID: "monitor-1", Ownership: "owned", Purpose: "모니터", Cleanup: "pending"}},
		map[string]any{"kind": "monitor"}))

	// A request nobody has taken yet.
	if _, _, err := s.appendLedgerRequest(ctx, "req-open", "task-a", "이거 먼저 해 줘", "user-1", "", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A request that was taken, dispatched and settled.
	if _, _, err := s.appendLedgerRequest(ctx, "req-done", "task-a", "저기 PR 올려 줘", "user-1", "", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	claim := mustLedger(t, s, "ledger_requests_claim", ledgerArgs{
		ID: "req-done", RequestID: "c-done", ExpectedVersion: lv(1), ClaimOwner: "parent",
	}).(map[string]any)
	token := claim["claim_token"].(string)
	mustLedger(t, s, "ledger_requests_dispatch", ledgerArgs{
		ID: "req-done", RequestID: "d-done", ExpectedVersion: lv(2), ClaimToken: token,
		OperationKey: "op-pr-1", OperationTarget: "pull/42", DispatchState: dispatchDispatched,
	})
	mustLedger(t, s, "ledger_requests_ack", ledgerArgs{
		ID: "req-done", RequestID: "a-done", ExpectedVersion: lv(3), ClaimToken: token,
		Outcome: requestProcessed, Result: "PR #42 를 올렸고 CI 는 초록입니다", RelatedSeq: lv(31),
	})
	// A request about the monitor row: it has no task to belong to after the
	// migration, which the report has to say rather than drop.
	if _, _, err := s.appendLedgerRequest(ctx, "req-monitor", "task-m", "이 모니터 멈춰 줘", "user-1", "", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func runMigration(t *testing.T, s *sessionDB, requestID string, dryRun bool) map[string]any {
	t.Helper()
	args := map[string]any{"request_id": requestID}
	if dryRun {
		args["dry_run"] = true
	}
	return mustWork(t, s, "work_migrate", args)
}

func TestWorkMigrationMapsOnlyWhatTheLedgerRecorded(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	report := runMigration(t, s, "m1", false)
	if report["state"] != workStateMigrated {
		t.Fatalf("report = %v", report)
	}
	plan := report["plan"].(map[string]any)
	counts := plan["counts"].(map[string]any)
	for field, want := range map[string]float64{
		"legacy_tasks": 2, "monitor_preserved": 1, "tasks_created": 1,
		"legacy_requests": 3, "turns_created": 2, "requests_unmapped": 1,
	} {
		if counts[field].(float64) != want {
			t.Fatalf("counts[%s] = %v, want %v (counts=%v)", field, counts[field], want, counts)
		}
	}
	if unmapped := plan["unmapped"].([]any); len(unmapped) != 1 ||
		unmapped[0].(map[string]any)["request_id"] != "req-monitor" {
		t.Fatalf("unmapped = %v", plan["unmapped"])
	}
	checks := plan["checks"].(map[string]any)
	if checks["orphan_turns"].(float64) != 0 || checks["legacy_tasks_accounted"] != true ||
		checks["legacy_requests_accounted"] != true {
		t.Fatalf("checks = %v", checks)
	}

	// The task: the title is the ledger's, the status has not moved, the version
	// carried over, and the session is the one relation still live.
	task := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})["task"].(map[string]any)
	if task["title"] != "앱에 작업 패널 띄우기" || task["status"] != "blocked" || task["version"].(float64) != 1 {
		t.Fatalf("task = %v", task)
	}
	if task["session_id"] != "worker-now" {
		t.Fatalf("session_id = %v, want the latest owned live relation", task["session_id"])
	}
	// What the ledger never recorded is null, not an invention.
	if task["description"] != nil || task["completion_criteria"] != nil {
		t.Fatalf("task = %v, want null description/completion_criteria", task)
	}
	// The relations, cursors, stage, owner, approval and preservation notes are
	// preserved verbatim beside the task rather than dropped to fit it.
	preserved := workRow(t, s, "SELECT stage,owner,sessions,details,kind FROM sp_work_task_legacy WHERE task_id='task-a'")
	if preserved["stage"] != "implementation" || preserved["owner"] != "parent" {
		t.Fatalf("preserved = %v", preserved)
	}
	var relations []ledgerSession
	if err := json.Unmarshal([]byte(preserved["sessions"].(string)), &relations); err != nil {
		t.Fatal(err)
	}
	if len(relations) != 3 || relations[1].NextOffset != 4096 || relations[1].Seq == nil || *relations[1].Seq != 12 {
		t.Fatalf("relations = %+v, want all three with their cursors", relations)
	}
	if relations[2].Reason == "" || relations[2].NextAction == "" {
		t.Fatalf("retention notes lost: %+v", relations[2])
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(preserved["details"].(string)), &details); err != nil {
		t.Fatal(err)
	}
	if details["approval"] != "PR only" || details["preservation"] == nil {
		t.Fatalf("details = %v", details)
	}

	// The monitor row is preserved and is not a task.
	wantWorkCode(t, s, "task_get", map[string]any{"id": "task-m"}, "not_found")
	monitor := workRow(t, s, "SELECT kind,legacy_status FROM sp_work_task_legacy WHERE task_id='task-m'")
	if monitor == nil || monitor["kind"] != "monitor" {
		t.Fatalf("monitor row = %v", monitor)
	}

	// The turns: a person's words verbatim, in arrival order, with nothing
	// invented for what the ledger never stored.
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	if len(turns) != 2 {
		t.Fatalf("turns = %v", turns)
	}
	open := turns[0].(map[string]any)
	done := turns[1].(map[string]any)
	if open["request"] != "이거 먼저 해 줘" || done["request"] != "저기 PR 올려 줘" {
		t.Fatalf("turn order/text = %v", turns)
	}
	for _, turn := range []map[string]any{open, done} {
		if turn["instruction"] != nil {
			t.Fatalf("instruction was invented: %v", turn)
		}
		if turn["response"] != nil {
			t.Fatalf("a legacy summary was dressed up as the session's final words: %v", turn)
		}
		if turn["question"] != nil {
			t.Fatalf("question was invented: %v", turn)
		}
	}
	if open["explanation"] != nil || open["started_at"] != nil || open["ended_at"] != nil || open["session_seq"] != nil {
		t.Fatalf("an unprocessed request did not stay unprocessed: %v", open)
	}
	if done["explanation"] != "PR #42 를 올렸고 CI 는 초록입니다" {
		t.Fatalf("the parent's own sentence was lost: %v", done)
	}
	if done["session_seq"].(float64) != 31 || done["started_at"] == nil || done["ended_at"] == nil {
		t.Fatalf("recorded seq/times were lost: %v", done)
	}
	// The dispatch that was recorded against the request still fences a resend.
	dispatch := workRow(t, s, "SELECT operation_key,operation_target FROM sp_work_turn_admin WHERE turn_id=?", int64(done["id"].(float64)))
	if dispatch == nil || dispatch["operation_key"] != "op-pr-1" || dispatch["operation_target"] != "pull/42" {
		t.Fatalf("dispatch record lost: %v", dispatch)
	}
	// The claim state and the author have no column in the agreed turn, so they
	// are preserved beside it.
	origin := workRow(t, s, "SELECT source,source_id,claim_state,admin FROM sp_work_turn_legacy WHERE turn_id=?", int64(open["id"].(float64)))
	if origin["source"] != "user_request" || origin["source_id"] != "req-open" || origin["claim_state"] != requestPending {
		t.Fatalf("origin = %v", origin)
	}
	var admin map[string]any
	if err := json.Unmarshal([]byte(origin["admin"].(string)), &admin); err != nil {
		t.Fatal(err)
	}
	if admin["author"] != "user-1" {
		t.Fatalf("admin = %v", admin)
	}

	// Nothing was taken from the ledger: it still answers for both tasks, and
	// the request rows a person may look for are still there.
	for _, id := range []string{"task-a", "task-m"} {
		if _, err := s.ledger(context.Background(), "ledger_get", ledgerArgs{ID: id}); err != nil {
			t.Fatalf("ledger_get %s after the migration: %v", id, err)
		}
	}
	listed := mustLedger(t, s, "ledger_requests_list", ledgerArgs{}).(map[string]any)
	if len(listed["requests"].([]ledgerRequest)) != 3 {
		t.Fatalf("ledger requests = %v", listed)
	}

	// The snapshot the run started from is on disk with its checksum recorded.
	snapshot := report["snapshot"].(map[string]any)
	info, err := os.Stat(snapshot["path"].(string))
	if err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if len(snapshot["sha256"].(string)) != 64 || info.Size() == 0 {
		t.Fatalf("snapshot = %v", snapshot)
	}
	if snapshot["integrity_check"] != "ok" {
		t.Fatalf("snapshot = %v", snapshot)
	}
}

// The ledger's own task history is where the only task times it ever recorded
// live. A task older than that history has none, and the migration says so
// instead of stamping it with the migration's own clock.
func TestWorkMigrationTaskTimesComeFromRecordedHistory(t *testing.T) {
	s, _ := newTestSessionDB(t)
	mustLedger(t, s, "ledger_put", legacyTaskArgs("with-history", "h1", 0, "이력 있는 작업", "planned",
		[]ledgerSession{}, map[string]any{}))
	mustLedger(t, s, "ledger_put", legacyTaskArgs("no-history", "n1", 0, "이력 없는 작업", "planned",
		[]ledgerSession{}, map[string]any{}))
	// An image older than the task history wrote no rows for its tasks.
	workExec(t, s, "DELETE FROM sp_ledger_task_events WHERE task_id='no-history'")
	runMigration(t, s, "m1", false)
	with := mustWork(t, s, "task_get", map[string]any{"id": "with-history"})["task"].(map[string]any)
	if with["created_at"] == nil || with["updated_at"] == nil {
		t.Fatalf("recorded times were dropped: %v", with)
	}
	without := mustWork(t, s, "task_get", map[string]any{"id": "no-history"})["task"].(map[string]any)
	if without["created_at"] != nil || without["updated_at"] != nil {
		t.Fatalf("times were invented for a task that has none: %v", without)
	}
}

// The choices a session still offers are about the task's current state, so
// they move to the turn they belong to. A task with no turn has nowhere to hold
// them — inventing a turn for them would invent an exchange — so they stay in
// the ledger and the report counts them.
func TestWorkMigrationMovesFreshChoicesOntoTheLatestTurn(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	mustLedger(t, s, "ledger_put", legacyTaskArgs("task-a", "la1", 0, "턴 있는 작업", "review",
		[]ledgerSession{}, map[string]any{}))
	mustLedger(t, s, "ledger_put", legacyTaskArgs("task-b", "lb1", 0, "턴 없는 작업", "review",
		[]ledgerSession{}, map[string]any{}))
	if _, _, err := s.appendLedgerRequest(ctx, "req-1", "task-a", "첫 요청", "user-1", "", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"task-a", "task-b"} {
		mustLedger(t, s, "ledger_choices_put", ledgerArgs{
			ID: id, RequestID: "ch-" + id, ExpectedVersion: lv(1),
			Choices: []ledgerChoiceArg{
				{ChoiceID: "yes", Label: "그대로 진행", RequestText: "그대로 진행해 줘"},
				{ChoiceID: "stop", Label: "잠깐 멈춰", RequestText: "잠깐 멈춰 줘"},
			},
		})
	}
	report := runMigration(t, s, "m1", false)
	counts := report["plan"].(map[string]any)["counts"].(map[string]any)
	if counts["choices_attached"].(float64) != 1 || counts["choices_unattached"].(float64) != 1 {
		t.Fatalf("counts = %v", counts)
	}
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	choices := turns[0].(map[string]any)["choices"].([]any)
	if len(choices) != 2 || choices[0].(map[string]any)["label"] != "그대로 진행" {
		t.Fatalf("choices = %v", choices)
	}
	// The agreed choice shape is a label and the sentence it sends; a choice id
	// is not part of it.
	if _, ok := choices[0].(map[string]any)["id"]; ok {
		t.Fatalf("a choice carries an id it should not: %v", choices[0])
	}
}

func TestWorkMigrationOnEmptyAndLedgerlessDatabase(t *testing.T) {
	s, _ := newTestSessionDB(t)
	report := runMigration(t, s, "m1", false)
	counts := report["plan"].(map[string]any)["counts"].(map[string]any)
	if counts["legacy_tasks"].(float64) != 0 || counts["turns_created"].(float64) != 0 {
		t.Fatalf("counts = %v", counts)
	}
	if report["state"] != workStateMigrated {
		t.Fatalf("report = %v", report)
	}
	page := mustWork(t, s, "task_list", map[string]any{})
	if len(page["tasks"].([]any)) != 0 {
		t.Fatalf("tasks = %v", page)
	}
}

func TestWorkMigrationDryRunChangesNothing(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	report := runMigration(t, s, "m-dry", true)
	if report["dry_run"] != true || report["state"] != workStateReady {
		t.Fatalf("dry run = %v", report)
	}
	counts := report["plan"].(map[string]any)["counts"].(map[string]any)
	if counts["tasks_created"].(float64) != 1 || counts["turns_created"].(float64) != 2 {
		t.Fatalf("a dry run reported a different mapping than a run performs: %v", counts)
	}
	if page := mustWork(t, s, "task_list", map[string]any{}); len(page["tasks"].([]any)) != 0 {
		t.Fatalf("a dry run wrote tasks: %v", page)
	}
	// A dry run leaves the ledger writable: nothing has moved yet.
	mustLedger(t, s, "ledger_put", legacyTaskArgs("task-a", "la2", 1, "제목 변경", "running",
		[]ledgerSession{}, map[string]any{}))
}

func TestWorkMigrationIsIdempotentAndResumable(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	runMigration(t, s, "m1", false)
	before := mustWork(t, s, "work_migration_report", map[string]any{})["counts"].(map[string]any)
	// A second run under a new request id is a no-op that says so.
	again := runMigration(t, s, "m2", false)
	if again["already_migrated"] != true {
		t.Fatalf("second run = %v", again)
	}
	after := mustWork(t, s, "work_migration_report", map[string]any{})["counts"].(map[string]any)
	for field := range before {
		if before[field] != after[field] {
			t.Fatalf("%s moved on a second run: %v -> %v", field, before[field], after[field])
		}
	}
	// A run interrupted after its snapshot leaves the fence up. The next run
	// resumes from the snapshot it already recorded and adds nothing twice.
	workExec(t, s, "UPDATE sp_work_meta SET state=?", workStateMigrating)
	resumed := runMigration(t, s, "m3", false)
	if resumed["state"] != workStateMigrated {
		t.Fatalf("resumed = %v", resumed)
	}
	counts := resumed["plan"].(map[string]any)["counts"].(map[string]any)
	if counts["tasks_created"].(float64) != 0 || counts["turns_created"].(float64) != 0 {
		t.Fatalf("a resumed run copied rows twice: %v", counts)
	}
	if counts["tasks_present"].(float64) != 1 || counts["turns_present"].(float64) != 2 {
		t.Fatalf("a resumed run did not recognise what was already there: %v", counts)
	}
	final := mustWork(t, s, "work_migration_report", map[string]any{})["counts"].(map[string]any)
	if final["work_turns"] != after["work_turns"] {
		t.Fatalf("turn count moved: %v -> %v", after["work_turns"], final["work_turns"])
	}
}

// A fence with no recorded snapshot is not something this code can produce, and
// copying without a recorded starting point would leave nothing to roll back
// to. It is refused rather than guessed at.
func TestWorkMigrationRefusesAFenceWithNoSnapshot(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	runMigration(t, s, "m-dry", true)
	workExec(t, s, "UPDATE sp_work_meta SET state=?", workStateMigrating)
	wantWorkCode(t, s, "work_migrate", map[string]any{"request_id": "m1"}, "conflict")
}

// While the fence is up every write is refused — a write admitted in the middle
// of one copying transaction would be copied twice or not at all.
func TestWorkMigrationFenceRefusesWrites(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	seedLedger(t, s)
	runMigration(t, s, "m-dry", true)
	workExec(t, s, "UPDATE sp_work_meta SET state=?", workStateMigrating)
	_, err := s.ledger(ctx, "ledger_put", legacyTaskArgs("task-a", "fenced", 1, "x", "running", []ledgerSession{}, map[string]any{}))
	assertLedgerCode(t, err, "conflict")
	_, _, err = s.appendLedgerRequest(ctx, "req-fenced", "task-a", "요청", "user-1", "", 0, time.Now())
	assertLedgerCode(t, err, "conflict")
	newWorkTaskFenced, code := workCall(t, s, "task_put", map[string]any{
		"id": "new", "request_id": "n1", "expected_version": 0, "task": map[string]any{
			"session_id": nil, "title": "x", "description": "", "completion_criteria": "", "status": "planned",
		},
	})
	if code != "conflict" {
		t.Fatalf("task_put under the fence = %q %v", code, newWorkTaskFenced)
	}
	// Reading is never fenced: a person's panel keeps working through a
	// migration.
	if _, err := s.ledger(ctx, "ledger_get", ledgerArgs{ID: "task-a"}); err != nil {
		t.Fatalf("ledger_get under the fence: %v", err)
	}
}

// After the cutover the parent writes turns, and a ledger write is refused
// rather than silently kept in a second place. Reading the ledger still works:
// the monitor rows and the task history the parent uses live only there.
func TestWorkCutoverRefusesLedgerWritesAndKeepsReads(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	seedLedger(t, s)
	runMigration(t, s, "m1", false)
	for _, name := range []string{"ledger_put", "ledger_delete", "ledger_choices_put", "ledger_choices_clear"} {
		args := ledgerArgs{ID: "task-a", RequestID: "after-" + name, ExpectedVersion: lv(1)}
		if name == "ledger_put" {
			args.Task = legacyTaskArgs("task-a", "x", 1, "x", "running", []ledgerSession{}, map[string]any{}).Task
		}
		if name == "ledger_choices_put" {
			args.Choices = []ledgerChoiceArg{{ChoiceID: "a", Label: "a", RequestText: "a"}}
		}
		_, err := s.ledger(ctx, name, args)
		assertLedgerCode(t, err, "conflict")
	}
	for _, name := range []string{"ledger_get", "ledger_list", "ledger_events_list", "ledger_requests_list"} {
		args := ledgerArgs{}
		if name == "ledger_get" || name == "ledger_events_list" {
			args.ID = "task-a"
		}
		if _, err := s.ledger(ctx, name, args); err != nil {
			t.Fatalf("%s after the cutover: %v", name, err)
		}
	}
}

// An app that still speaks the ledger keeps working after the cutover: its
// stored request becomes the next turn, its retry is still a retry, and the
// task page it reads is the same shape with the preserved notes back in place.
func TestWorkCutoverBridgesTheAppsRequestsAndPage(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	seedLedger(t, s)
	runMigration(t, s, "m1", false)
	stored, replayed, err := s.appendLedgerRequest(ctx, "req-new", "task-a", "다음 요청", "user-1", "", 0, time.Now())
	if err != nil || replayed {
		t.Fatalf("store = %v, %v, %v", stored, replayed, err)
	}
	if stored.Status != requestPending || stored.Text != "다음 요청" {
		t.Fatalf("stored = %+v", stored)
	}
	again, replayed, err := s.appendLedgerRequest(ctx, "req-new", "task-a", "다음 요청", "user-1", "", 0, time.Now())
	if err != nil || !replayed || again.Seq != stored.Seq {
		t.Fatalf("replay = %+v, %v, %v", again, replayed, err)
	}
	if _, _, err = s.appendLedgerRequest(ctx, "req-new", "task-a", "다른 말", "user-1", "", 0, time.Now()); err == nil {
		t.Fatal("the same id with different words was accepted")
	}
	if _, _, err = s.appendLedgerRequest(ctx, "req-absent", "no-such-task", "요청", "user-1", "", 0, time.Now()); err == nil {
		t.Fatal("a request about a task this slot does not have was accepted")
	}
	// It became a turn, and it is open work.
	open := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a", "open": true})["turns"].([]any)
	if len(open) != 2 {
		t.Fatalf("open turns = %v", open)
	}
	// The page an un-updated app reads: the ledger's shape, the preserved stage
	// and relation notes, and a count of what is waiting.
	page, err := s.tasksView(ctx, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	records := page.(map[string]any)["tasks"].([]ledgerRecord)
	if len(records) != 1 || records[0].ID != "task-a" {
		t.Fatalf("projected page = %v", records)
	}
	if records[0].Task.Stage != "implementation" || records[0].Task.Owner != "parent" {
		t.Fatalf("projected task = %+v", records[0].Task)
	}
	if len(records[0].Task.Sessions) != 3 || records[0].Task.Details["approval"] != "PR only" {
		t.Fatalf("projected task lost the preserved notes: %+v", records[0].Task)
	}
	if records[0].Requests == nil || records[0].Requests.Pending != 2 {
		t.Fatalf("projected requests = %+v", records[0].Requests)
	}
}

// A screen opened before the parent moved on still holds the older question and
// its choices. Hiding them on the read side is not enough — that screen can
// still send one — so the store refuses the pick.
func TestWorkCutoverRefusesAPickFromAnOlderTurn(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	seedLedger(t, s)
	runMigration(t, s, "m1", false)
	task := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})
	latest := task["latest_turn"].(map[string]any)
	version := task["task"].(map[string]any)["version"].(float64)
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": latest["id"], "request_id": "q1", "expected_version": version,
		"turn": map[string]any{"question": "어느 쪽?", "choices": []any{
			map[string]any{"label": "A", "request_text": "A 로 해 줘"},
		}},
	})
	offered := compatChoiceID(int64(latest["id"].(float64)), 0)
	stored, _, err := s.appendLedgerRequest(ctx, "pick-1", "task-a", "A 로 해 줘", "user-1", offered, int64(version), time.Now())
	if err != nil {
		t.Fatalf("a current pick was refused: %v", err)
	}
	// That pick appended a turn, so the one it came from is no longer the
	// latest: the same screen cannot send it twice.
	_, _, err = s.appendLedgerRequest(ctx, "pick-2", "task-a", "A 로 해 줘", "user-1", offered, int64(version), time.Now())
	assertLedgerCode(t, err, "stale_choice")
	// A choice position that the turn does not offer is refused the same way.
	_, _, err = s.appendLedgerRequest(ctx, "pick-3", "task-a", "A 로 해 줘", "user-1",
		compatChoiceID(stored.Seq, 3), int64(version), time.Now())
	assertLedgerCode(t, err, "stale_choice")
}

// The namespace refuses a database it was not written for rather than reading
// it with this version's columns.
func TestWorkSchemaVersionIsRefusedWhenUnknown(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	workExec(t, s, "UPDATE sp_work_meta SET version=?", workSchemaVersion+1)
	wantWorkCode(t, s, "task_get", map[string]any{"id": "task-a"}, "schema_version")
	// A table in the reserved namespace that this schema did not write is a
	// collision, not something to repair destructively.
	workExec(t, s, "UPDATE sp_work_meta SET version=?", workSchemaVersion)
	workExec(t, s, "DROP TABLE sp_work_meta")
	wantWorkCode(t, s, "task_get", map[string]any{"id": "task-a"}, "schema_version")
	if row := workRow(t, s, "SELECT id FROM sp_work_tasks WHERE id='task-a'"); row == nil {
		t.Fatal("a refused schema check dropped a user row")
	}
}

// Rolling the image back is survivable: the ledger tables are exactly as they
// were, so an image that never heard of the work tables still serves them, and
// what was written after the cutover is still on disk to be reconciled.
func TestWorkCutoverLeavesTheLedgerIntactForARollback(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	seedLedger(t, s)
	beforeTasks := workRow(t, s, "SELECT count(*) AS n FROM sp_ledger_tasks")
	beforeRequests := workRow(t, s, "SELECT count(*) AS n FROM sp_ledger_user_requests")
	beforeEvents := workRow(t, s, "SELECT count(*) AS n FROM sp_ledger_task_events")
	runMigration(t, s, "m1", false)
	if _, _, err := s.appendLedgerRequest(ctx, "req-after", "task-a", "컷오버 뒤 요청", "user-1", "", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, before := range map[string]map[string]any{
		"sp_ledger_tasks": beforeTasks, "sp_ledger_user_requests": beforeRequests,
		"sp_ledger_task_events": beforeEvents,
	} {
		after := workRow(t, s, "SELECT count(*) AS n FROM "+name)
		if after["n"] != before["n"] {
			t.Fatalf("%s changed across the cutover: %v -> %v", name, before["n"], after["n"])
		}
	}
	// The ledger's own version is untouched, which is what lets the older image
	// open this database at all.
	if row := workRow(t, s, "SELECT version AS n FROM sp_ledger_meta"); row["n"].(int64) != 1 {
		t.Fatalf("sp_ledger_meta = %v", row)
	}
	// The request written after the cutover is a turn, so a rollback has it to
	// reconcile rather than nothing.
	row := workRow(t, s, "SELECT source_id FROM sp_work_turn_legacy WHERE source='app_request'")
	if row == nil || row["source_id"] != "req-after" {
		t.Fatalf("post-cutover request = %v", row)
	}
}

// PRAGMA user_version and user tables are not this namespace's to touch.
func TestWorkMigrationLeavesUserTablesAlone(t *testing.T) {
	s, _ := newTestSessionDB(t)
	workExec(t, s, "CREATE TABLE mine (a TEXT)")
	workExec(t, s, "INSERT INTO mine VALUES ('keep')")
	workExec(t, s, "PRAGMA user_version=7")
	seedLedger(t, s)
	runMigration(t, s, "m1", false)
	if row := workRow(t, s, "SELECT a FROM mine"); row == nil || row["a"] != "keep" {
		t.Fatalf("user table = %v", row)
	}
	if row := workRow(t, s, "PRAGMA user_version"); row["user_version"].(int64) != 7 {
		t.Fatalf("user_version = %v", row)
	}
}

// Two parents racing on the same task: one wins the version, the other is told
// to read again. The database is opened with one connection, so this also
// covers two calls arriving at once.
func TestWorkTaskWritesHaveASingleWinner(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	wins, conflicts := 0, 0
	codes := make(chan string, 2)
	for _, id := range []string{"w1", "w2"} {
		go func(id string) {
			_, code := workCall(t, s, "task_put", map[string]any{
				"id": "task-a", "request_id": id, "expected_version": 1, "task": map[string]any{
					"session_id": nil, "title": id, "description": "", "completion_criteria": "", "status": "running",
				},
			})
			codes <- code
		}(id)
	}
	for range []int{0, 1} {
		switch code := <-codes; code {
		case "":
			wins++
		case "conflict":
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestWorkMigrationReportReadsCurrentCounts(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	before := mustWork(t, s, "work_migration_report", map[string]any{})
	if before["state"] != workStateReady || before["schema_version"].(float64) != workSchemaVersion {
		t.Fatalf("report before = %v", before)
	}
	if len(before["steps"].([]any)) != 0 {
		t.Fatalf("steps before = %v", before["steps"])
	}
	runMigration(t, s, "m1", false)
	after := mustWork(t, s, "work_migration_report", map[string]any{})
	steps := after["steps"].([]any)
	if len(steps) != 3 {
		t.Fatalf("steps = %v", steps)
	}
	counts := after["counts"].(map[string]any)
	if counts["work_tasks"].(float64) != 1 || counts["work_turns"].(float64) != 2 ||
		counts["monitor_preserved"].(float64) != 1 || counts["preserved_turns"].(float64) != 2 {
		t.Fatalf("counts = %v", counts)
	}
}

// A ledger row written by a hand that this code would not produce — a document
// missing its session list — is still migrated as far as it can be, with the
// task's own fields intact.
func TestWorkMigrationHandlesIncompleteLegacyRows(t *testing.T) {
	s, _ := newTestSessionDB(t)
	workExec(t, s, "CREATE TABLE sp_ledger_meta (version INTEGER NOT NULL)")
	workExec(t, s, "INSERT INTO sp_ledger_meta VALUES (1)")
	workExec(t, s, "CREATE TABLE sp_ledger_tasks (id TEXT PRIMARY KEY, version INTEGER NOT NULL, status TEXT NOT NULL, stage TEXT NOT NULL, owner TEXT NOT NULL, document TEXT NOT NULL)")
	workExec(t, s, "CREATE TABLE sp_ledger_retired (id TEXT PRIMARY KEY)")
	workExec(t, s, "CREATE TABLE sp_ledger_requests (id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, response TEXT NOT NULL)")
	workExec(t, s, `INSERT INTO sp_ledger_tasks VALUES ('bare',3,'completed','','', '{"title":"오래된 작업","status":"completed"}')`)
	report := runMigration(t, s, "m1", false)
	counts := report["plan"].(map[string]any)["counts"].(map[string]any)
	if counts["tasks_created"].(float64) != 1 {
		t.Fatalf("counts = %v", counts)
	}
	task := mustWork(t, s, "task_get", map[string]any{"id": "bare"})["task"].(map[string]any)
	if task["title"] != "오래된 작업" || task["status"] != "completed" || task["version"].(float64) != 3 {
		t.Fatalf("task = %v", task)
	}
	if task["session_id"] != nil || task["created_at"] != nil {
		t.Fatalf("task = %v, want nulls where the ledger has nothing", task)
	}
}

func TestWorkMigrationRefusesADamagedDatabase(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	// A snapshot path that already exists is the only way this run refuses
	// without corrupting anything, so it stands in for "the snapshot could not
	// be taken".
	fixed := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	original := ledgerNow
	ledgerNow = func() time.Time { return fixed }
	defer func() { ledgerNow = original }()
	runMigration(t, s, "m1", false)
	workExec(t, s, "UPDATE sp_work_meta SET state=?", workStateReady)
	workExec(t, s, "DELETE FROM sp_work_migration")
	wantWorkCode(t, s, "work_migrate", map[string]any{"request_id": "m2"}, "conflict")
}

func TestWorkTurnLegacySourceIsUnique(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	runMigration(t, s, "m1", false)
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var turnID int64
	if err := db.QueryRowContext(ctx,
		"SELECT turn_id FROM sp_work_turn_legacy WHERE source_id='req-open'").Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO sp_work_turn_legacy (turn_id,source,source_id,claim_state,admin,provenance) VALUES (?,?,?,'','{}','{}')",
		turnID+1, "user_request", "req-open")
	if err == nil {
		t.Fatal("the same ledger request was mapped to a second turn")
	}
	var missing sql.NullInt64
	if err := db.QueryRowContext(ctx,
		"SELECT max(turn_id) FROM sp_work_turn_legacy").Scan(&missing); err != nil {
		t.Fatal(err)
	}
}
