package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The helpers every test in this area shares. They read and write the database
// directly, because the thing under test is a database an *older* image left
// behind: the tools that wrote the ledger are not in this one, so raw SQL is
// the only way to build the state the move has to deal with.
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

// retireAt runs the move at a chosen time, so a test's own wall clock is never
// what a recorded step says happened.
func retireAt(t *testing.T, s *sessionDB, requestID string, dryRun bool, now time.Time) map[string]any {
	t.Helper()
	args := workArgs{RequestID: requestID}
	if dryRun {
		args.DryRun = &dryRun
	}
	out, err := s.runWorkRetire(context.Background(), args, now)
	if err != nil {
		t.Fatalf("%s: %v", workRetireTool, err)
	}
	return out.(map[string]any)
}

func retireFails(t *testing.T, s *sessionDB, requestID string, now time.Time) error {
	t.Helper()
	_, err := s.runWorkRetire(context.Background(), workArgs{RequestID: requestID}, now)
	if err == nil {
		t.Fatalf("%s was accepted, want a refusal", workRetireTool)
	}
	return err
}

// retirementPlan reads the plan out of either answer shape: a rehearsal returns
// the struct, and a committed run returns the JSON it recorded.
func retirementPlan(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	if plan, ok := out["plan"].(workRetirementPlan); ok {
		encoded, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	decoded, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("no plan in %v", out)
	}
	return decoded
}

func workTableIsThere(t *testing.T, s *sessionDB, name string) bool {
	t.Helper()
	row := workRow(t, s, "SELECT count(*) AS n FROM sqlite_master WHERE type='table' AND name=?", name)
	return row["n"].(int64) != 0
}

func workCount(t *testing.T, s *sessionDB, table string) int64 {
	t.Helper()
	return workRow(t, s, "SELECT count(*) AS n FROM "+table)["n"].(int64)
}

// assertWorkCode is the refusal class a caller branches on. A refusal with the
// wrong code is as bad as no refusal: the parent acts on the code.
func assertWorkCode(t *testing.T, err error, want string) {
	t.Helper()
	var known workError
	if !asWorkError(err, &known) {
		t.Fatalf("error = %v, want a workError with code %s", err, want)
	}
	if known.Code != want {
		t.Fatalf("code = %q, want %q (%s)", known.Code, want, known.ErrorText)
	}
}

func asWorkError(err error, into *workError) bool {
	known, ok := err.(workError)
	if ok {
		*into = known
	}
	return ok
}

func mustHandle(t *testing.T, s *sessionDB) *sql.DB {
	t.Helper()
	db, err := s.handle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// mustWorkView and mustWorkTurnsView read the work records the way a reader
// outside the session does.
func mustWorkView(t *testing.T, s *sessionDB, a workArgs) map[string]any {
	t.Helper()
	out, err := s.workTasksView(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func mustWorkTurnsView(t *testing.T, s *sessionDB, a workArgs) map[string]any {
	t.Helper()
	out, err := s.workTurnsView(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

// seedMigratedSlot is the other state an older image leaves: it moved the
// records into the work tables, kept the ledger standing, and wrote a verbatim
// copy of every moved row beside each task. Both have to go, and what the
// parent still reads has to come out of them first.
func seedMigratedSlot(t *testing.T, s *sessionDB) {
	t.Helper()
	seedLegacyLedger(t, s)
	// The work schema has to exist before the transition tables are added
	// beside it: a database with an sp_work_* table and no sp_work_meta is a
	// namespace collision, which is the right answer for a real one and would
	// only make this fixture look like a bug in the code under test.
	mustWork(t, s, workStateTool, map[string]any{})
	workExec(t, s, `CREATE TABLE sp_work_task_legacy (
		task_id TEXT PRIMARY KEY, legacy_version INTEGER NOT NULL, legacy_status TEXT NOT NULL, kind TEXT NOT NULL,
		stage TEXT NOT NULL, owner TEXT NOT NULL, sessions TEXT NOT NULL, details TEXT NOT NULL,
		provenance TEXT NOT NULL)`)
	workExec(t, s, `CREATE TABLE sp_work_turn_legacy (
		turn_id INTEGER PRIMARY KEY, source TEXT NOT NULL, source_id TEXT NOT NULL,
		claim_state TEXT NOT NULL, admin TEXT NOT NULL, provenance TEXT NOT NULL)`)
	workExec(t, s, `CREATE TABLE sp_work_migration (
		seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, step TEXT NOT NULL, state TEXT NOT NULL,
		detail TEXT NOT NULL)`)
	workExec(t, s, `INSERT INTO sp_work_migration (at,step,state,detail) VALUES
		('2026-11-10T00:00:00Z','snapshot','done','{"path":"/gone/pre-work.bak","sha256":"abc","bytes":4096}'),
		('2026-11-10T00:00:01Z','copy','done','{"counts":{"tasks":1}}')`)
	// The task as that image's migration left it: an owned work task, with the
	// copy of everything the agreed shape had no column for beside it.
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{
		"title": "앱에 작업 패널 띄우기", "status": "blocked", "session_id": "worker-now",
	})
	workExec(t, s, `INSERT INTO sp_work_task_legacy VALUES
		('task-a',3,'blocked','','implementation','parent',
		 '[{"id":"worker-old","ownership":"owned","purpose":"첫 구현","cleanup":"deleted"},
		   {"id":"worker-now","ownership":"owned","purpose":"이어서 구현","cleanup":"pending","nextOffset":4096}]',
		 '{"approval":"PR only","preservation":"unpushed work","result":"지난 결과","pr":"https://example.invalid/pull/41"}',
		 '{}')`)
	// A turn that came from a stored request, with the outside action the parent
	// recorded against it copied into both the owned record and the copy.
	turn := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "seed-turn", "expected_version": 1,
		"turn": map[string]any{"request": "이거 먼저 해 줘"},
	})["turn"].(map[string]any)
	turnID := int64(turn["id"].(float64))
	workExec(t, s, `INSERT INTO sp_work_turn_legacy VALUES (?,?,?,?,?,?)`,
		turnID, workTurnOriginUserRequest, "req-open", "",
		`{"operation_key":"op-pr-1","operation_target":"pull/41"}`, "{}")
	workExec(t, s,
		`INSERT INTO sp_work_turn_admin (turn_id,operation_key,operation_target,dispatch_state,dispatched_at)
		 VALUES (?,?,?,?,?)`,
		turnID, "op-pr-1", "pull/41", dispatchDispatched, "2026-11-01T00:30:00Z")
	// The ledger row this task came from is the one the work records now own, so
	// the adoption leaves it alone and the cleanup drops it.
	workExec(t, s, "UPDATE sp_work_meta SET state=?", workStateMigrated)
}

// A slot an older image migrated: the preserved copies and the ledger both go,
// and the live bookkeeping the parent still acts on comes out of the copy
// before it does.
func TestWorkRetireCarriesTheBookkeepingAndDropsTheCopies(t *testing.T) {
	s, h := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// A slot that still has a ledger offers the move; a retired one does not.
	if names := strings.Join(sessionDBToolNames(t, h), ","); names != workToolNames {
		t.Fatalf("tools before = %s, want %s", names, workToolNames)
	}

	out := retireAt(t, s, "r1", false, now)
	if out["state"] != workStateRetired || out["schema_version"] != workSchemaVersionRetired {
		t.Fatalf("retirement = %v", out)
	}
	plan := retirementPlan(t, out)
	counts := plan["counts"].(map[string]any)
	// The live ledger row is where task-a's bookkeeping comes from — it is the
	// one something could still have written — so the adoption carries it and
	// the preserved copy beside it correctly finds it already owned.
	adoption := counts["adoption"].(map[string]any)
	if adoption["bookkeeping_carried"].(float64) != 1 {
		t.Fatalf("adoption bookkeeping_carried = %v, want the live row's bookkeeping carried", adoption)
	}
	if counts["preserved_bookkeeping_already_owned"].(float64) != 1 {
		t.Fatalf("preserved_bookkeeping_already_owned = %v, want the copy recognised as the older of the two", counts)
	}
	if counts["turn_dispatch_verified"].(float64) != 1 {
		t.Fatalf("turn_dispatch_verified = %v, want the recorded dispatch checked before its copy went", counts)
	}

	// What the parent acts on is in the owned record, selected rather than
	// copied whole: the approval scope and the preservation note are there, the
	// account of something that already happened is not.
	book := mustWork(t, s, taskBookGetTool, map[string]any{"task_id": "task-a"})
	details := book["details"].(map[string]any)
	for _, key := range []string{"approval", "preservation"} {
		if _, ok := details[key]; !ok {
			t.Fatalf("the live bookkeeping lost %q: %v", key, details)
		}
	}
	for _, key := range []string{"result", "pr"} {
		if _, ok := details[key]; ok {
			t.Fatalf("a past-tense key was copied into the owned record: %q in %v", key, details)
		}
	}
	// A relation whose session is gone is a cursor into nothing, so it does not
	// come across either. The live ledger row carries three; one is deleted.
	relations := book["sessions"].([]any)
	if len(relations) != 2 {
		t.Fatalf("relations = %v, want the two that are not deleted", relations)
	}
	for _, raw := range relations {
		if raw.(map[string]any)["id"] == "worker-old" {
			t.Fatalf("a deleted session's cursor came across: %v", relations)
		}
	}

	// Every table the move removes is gone, and a later call does not put them
	// back as empty tables — which would read as "the transition is still here".
	for _, table := range workRetiredTables {
		if workTableIsThere(t, s, table) {
			t.Fatalf("%s survived the retirement", table)
		}
	}
	mustWork(t, s, "task_list", map[string]any{})
	mustWork(t, s, workStateTool, map[string]any{})
	if workTableIsThere(t, s, "sp_work_task_legacy") {
		t.Fatal("a later call recreated the dropped transition tables")
	}
	if row := workRow(t, s, "SELECT version AS n FROM sp_work_meta"); row["n"].(int64) != workSchemaVersionRetired {
		t.Fatalf("schema version = %v, want the version an older image refuses", row)
	}
	if names := strings.Join(sessionDBToolNames(t, h), ","); names != workToolNamesRetired {
		t.Fatalf("tools after = %s, want the retired surface %s", names, workToolNamesRetired)
	}
}

// The bookkeeping stays writable afterwards, under its own counter: the record
// moved, so the parent's next write has to land on the owned one.
func TestRetiredSlotKeepsWritingItsBookkeeping(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))

	before := mustWork(t, s, taskBookGetTool, map[string]any{"task_id": "task-a"})
	after := mustWork(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-a", "request_id": "b1",
		"expected_book_version": before["book_version"],
		"record": map[string]any{
			"stage": "review", "owner": "parent",
			"sessions": before["sessions"],
			"details":  map[string]any{"approval": "PR only", "cancelled": "no"},
		},
	})
	if after["details"].(map[string]any)["cancelled"] != "no" {
		t.Fatalf("bookkeeping write after retirement = %v", after)
	}
	// And the stale version is refused rather than silently overwriting.
	wantWorkCode(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-a", "request_id": "b2",
		"expected_book_version": before["book_version"],
		"record": map[string]any{
			"stage": "review", "owner": "parent", "sessions": []any{}, "details": map[string]any{},
		},
	}, "conflict")
}

// The migrated task keeps telling a reader where it came from after the copy
// that used to carry that fact is gone.
func TestWorkRetireKeepsWhereRecordsCameFrom(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))

	page := mustWorkView(t, s, workArgs{})
	tasks := page["tasks"].([]workTaskView)
	if len(tasks) != 1 || !tasks[0].Migrated {
		t.Fatalf("work tasks = %+v, want the migrated task still marked migrated", tasks)
	}
	views := mustWorkTurnsView(t, s, workArgs{TaskID: "task-a"})["turns"].([]workTurnView)
	found := false
	for _, view := range views {
		if view.Origin == workTurnOriginUserRequest {
			found = true
		}
	}
	if !found {
		t.Fatalf("turns = %+v, want the carried-over turn still named %s", views, workTurnOriginUserRequest)
	}
	// The author beside a person's words survives too — it was in the copy.
	row := workRow(t, s, "SELECT author FROM sp_work_turn_origin WHERE origin=?", workTurnOriginUserRequest)
	if row == nil || row["author"] != "" && row["author"] != "user-1" {
		t.Fatalf("author = %v, want it carried rather than invented", row)
	}
}

// A task created in this image has no ledger history, and writing its
// bookkeeping must not say otherwise: before the bookkeeping had a record of
// its own, recording an approval scope made a task read as migrated and sent a
// reader to a history that does not exist.
func TestBookkeepingWriteDoesNotMakeATaskLookMigrated(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-new", "c1", nil)
	mustWork(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-new", "request_id": "b1",
		"record": map[string]any{
			"stage": "implementation", "owner": "parent", "sessions": []any{},
			"details": map[string]any{"approval": "PR only"},
		},
	})
	for _, view := range mustWorkView(t, s, workArgs{})["tasks"].([]workTaskView) {
		if view.Task.ID == "task-new" && view.Migrated {
			t.Fatal("a bookkeeping write made a task created here read as migrated")
		}
	}
}

// The copy of a recorded outside action may go only because the owned dispatch
// record has it. Without that, the parent's reconciliation breaks silently: a
// resend after a crash would look like a first send.
func TestWorkRetireRefusesWhenADispatchWasNotCarried(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	workExec(t, s, "DELETE FROM sp_work_turn_admin")

	err := retireFails(t, s, "r1", time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	assertWorkCode(t, err, "conflict")
	if !strings.Contains(err.Error(), "nothing else holds") {
		t.Fatalf("refusal = %v, want it to say what would have been lost", err)
	}
	// Nothing was dropped, and the rehearsal names the row.
	if !workTableIsThere(t, s, "sp_work_turn_legacy") {
		t.Fatal("a refused retirement dropped the preserved copies")
	}
	out := retireAt(t, s, "r2", true, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	refusals := retirementPlan(t, out)["refusals"].([]any)
	if len(refusals) == 0 || !strings.Contains(refusals[0].(string), "op-pr-1") {
		t.Fatalf("refusals = %v, want the operation key named", refusals)
	}
}

// A preserved copy of a monitoring loop whose ledger row is gone is the one
// case where dropping the copy would lose something, so it becomes a monitoring
// record instead of going.
func TestWorkRetireRescuesTheLastCopyOfAMonitorLoop(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	// The parent deleted the monitor row from the ledger after the cutover; the
	// copy beside the work records knows nothing about it.
	workExec(t, s, "DELETE FROM sp_ledger_tasks WHERE id='task-m'")
	workExec(t, s, `INSERT INTO sp_work_task_legacy VALUES
		('task-m',1,'running',?,'모니터 루프 기록','parent','[]','{"kind":"monitor","last_seen":"2026-11-01T00:00:00Z"}','{}')`,
		adoptMonitorKind)

	out := retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	if retirementPlan(t, out)["counts"].(map[string]any)["monitors_rescued_from_copies"].(float64) != 1 {
		t.Fatalf("counts = %v, want the orphaned copy rescued", retirementPlan(t, out)["counts"])
	}
	monitor := mustWork(t, s, monitorGetTool, map[string]any{"id": "task-m"})
	if monitor["state"] != monitorStateRunning {
		t.Fatalf("monitor = %v, want the loop still running", monitor)
	}
	if !strings.Contains(monitor["note"].(string), "last_seen") {
		t.Fatalf("note = %v, want what the loop last saw", monitor["note"])
	}
	// It is a monitoring record, never a task.
	if listed := mustWork(t, s, "task_list", map[string]any{})["tasks"].([]any); len(listed) != 1 {
		t.Fatalf("task_list = %v, want the person's one task", listed)
	}
}

// A run interrupted after its snapshot leaves the fence up. The next one
// resumes from the snapshot already recorded rather than taking a second.
func TestWorkRetireResumesAfterAnInterruption(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	snapshot, err := s.snapshotSessionDB(context.Background(), mustHandle(t, s), workRetireSnapshotLabel, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	workExec(t, s, "INSERT INTO sp_work_retirement (at,step,detail) VALUES (?,?,?)",
		workNow(now), workRetirementStepSnapshot, string(encoded))
	workExec(t, s, "UPDATE sp_work_meta SET state=?", workStateRetiring)

	// While the fence is up a write is a retry rather than a loss, and reading
	// keeps working.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-x", "request_id": "x1", "expected_version": 0,
		"task": map[string]any{
			"session_id": nil, "title": "x", "description": "", "completion_criteria": "", "status": "planned",
		},
	}, "conflict")
	mustWork(t, s, "task_list", map[string]any{})

	out := retireAt(t, s, "r1", false, now)
	if out["state"] != workStateRetired {
		t.Fatalf("resumed retirement = %v", out)
	}
	if resumed := out["snapshot"].(map[string]any); resumed["sha256"] != snapshot["sha256"] {
		t.Fatalf("snapshot = %v, want the one the interrupted run recorded (%v)", resumed, snapshot)
	}
	if count := workCount(t, s, "sp_work_retirement WHERE step='"+workRetirementStepSnapshot+"'"); count != 1 {
		t.Fatalf("snapshot steps = %d, want the first one reused", count)
	}
	// Replaying the same request_id answers what was committed.
	if replay := mustWork(t, s, workRetireTool, map[string]any{"request_id": "r1"}); replay["already_retired"] != true {
		t.Fatalf("replay = %v", replay)
	}
}

// A fence with no snapshot is a state this code cannot produce, and dropping
// rows with no recorded archive of them has nothing to go back to.
func TestWorkRetireRefusesAFenceWithNoSnapshot(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	workExec(t, s, "UPDATE sp_work_meta SET state=?", workStateRetiring)
	err := retireFails(t, s, "r1", time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	assertWorkCode(t, err, "conflict")
	if !strings.Contains(err.Error(), "no snapshot is recorded") {
		t.Fatalf("refusal = %v", err)
	}
	if !workTableIsThere(t, s, "sp_work_task_legacy") {
		t.Fatal("a refused retirement dropped the preserved copies")
	}
}

// The move touches this namespace and nothing else: a user's own table and the
// file's user_version are not the move's to change.
func TestWorkRetireLeavesUserTablesAlone(t *testing.T) {
	s, _ := newTestSessionDB(t)
	workExec(t, s, "CREATE TABLE mine (a TEXT)")
	workExec(t, s, "INSERT INTO mine VALUES ('keep')")
	workExec(t, s, "PRAGMA user_version=7")
	seedMigratedSlot(t, s)
	retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	if row := workRow(t, s, "SELECT a FROM mine"); row == nil || row["a"] != "keep" {
		t.Fatalf("user table = %v", row)
	}
	if row := workRow(t, s, "PRAGMA user_version"); row["user_version"].(int64) != 7 {
		t.Fatalf("user_version = %v", row)
	}
}

// work_state is the one read in this area that outlives the move: what the
// older image's migration recorded is still readable through it afterwards,
// because the step log was folded in before its table went.
func TestWorkStateReportsTheRetirementAndTheMigrationItFolded(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))

	report := mustWork(t, s, workStateTool, map[string]any{})
	if report["state"] != workStateRetired {
		t.Fatalf("report = %v", report)
	}
	if report["schema_version"].(float64) != workSchemaVersionRetired {
		t.Fatalf("schema_version = %v", report["schema_version"])
	}
	if report["migration_steps"] != nil {
		t.Fatalf("migration_steps = %v, want the table gone", report["migration_steps"])
	}
	// The migration's own snapshot checksum is in the retirement record, so a
	// full restore still knows where it would start from.
	folded := ""
	for _, step := range report["retirement"].([]any) {
		entry := step.(map[string]any)
		if entry["step"] != workRetirementStepRetire {
			continue
		}
		for _, migrated := range entry["detail"].(map[string]any)["migration_steps"].([]any) {
			detail, ok := migrated.(map[string]any)["detail"].(map[string]any)
			if !ok {
				continue
			}
			if sum, ok := detail["sha256"].(string); ok {
				folded = sum
			}
		}
	}
	if folded != "abc" {
		t.Fatalf("folded migration steps = %q, want the migration's snapshot checksum", folded)
	}
	// A table that is gone is not counted at all: a zero would read as "the
	// table is there and empty".
	counts := report["counts"].(map[string]any)
	for name := range counts {
		if strings.HasPrefix(name, "sp_ledger_") || name == "sp_work_task_legacy" {
			t.Fatalf("counts = %v, want no count of a table that is gone", counts)
		}
	}
	for _, field := range []string{"work_tasks", "work_turns", "bookkeeping_records", "tasks_from_ledger",
		"turn_origins", "retired_ids"} {
		if _, ok := counts[field]; !ok {
			t.Fatalf("counts = %v, want %s", counts, field)
		}
	}
}

// The namespace refuses a database it was not written for rather than reading
// it with this version's columns. The retired version is one this image *does*
// read — the gate is for the older image, which is the point of the one-way
// door.
func TestWorkSchemaVersionIsRefusedWhenUnknown(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	mustWork(t, s, "task_get", map[string]any{"id": "task-a"})
	workExec(t, s, "UPDATE sp_work_meta SET version=?", workSchemaVersionRetired+1)
	wantWorkCode(t, s, "task_get", map[string]any{"id": "task-a"}, "schema_version")
}

// Two parents racing on one task: one wins the version, the other is told to
// read again. The database is opened with one connection, so this also covers
// two calls arriving at once.
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
