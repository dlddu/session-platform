package main

import (
	"context"
	"strings"
	"testing"
)

// The helpers the tests in this area share. They read and write the database
// directly, because what is under test is a database an *older* image left
// behind: the tools that wrote the ledger and the move's own bookkeeping are
// not in this image, so raw SQL is the only way to build the state this one has
// to deal with.
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

func asWorkError(err error, into *workError) bool {
	known, ok := err.(workError)
	if ok {
		*into = known
	}
	return ok
}

// mustWorkView reads the work records the way a reader outside the session
// does.
func mustWorkView(t *testing.T, s *sessionDB, a workArgs) map[string]any {
	t.Helper()
	out, err := s.workTasksView(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}

func workTableIsThere(t *testing.T, s *sessionDB, name string) bool {
	t.Helper()
	return workRow(t, s, "SELECT count(*) AS n FROM sqlite_master WHERE type='table' AND name=?", name)["n"].(int64) != 0
}

func workColumnIsThere(t *testing.T, s *sessionDB, table, column string) bool {
	t.Helper()
	return workRow(t, s,
		"SELECT count(*) AS n FROM pragma_table_info(?) WHERE name=?", table, column)["n"].(int64) != 0
}

// seedRetiredSlot builds the database an older image's retirement left: the
// records are the work records, the meta row still carries the old two-column
// shape with a transition state in it, and beside them sit the move's own
// bookkeeping — the step logs and the verbatim copies it kept. This image's job
// is to drop exactly those and to stop keeping a state at all.
func seedRetiredSlot(t *testing.T, s *sessionDB) {
	t.Helper()
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "옮겨온 작업"})
	workExec(t, s, "ALTER TABLE sp_work_meta ADD COLUMN state TEXT NOT NULL DEFAULT 'retired'")
	workExec(t, s, "UPDATE sp_work_meta SET version=?, state='retired'", workSchemaVersionRetired)
	workExec(t, s, `CREATE TABLE sp_work_migration (
		seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, step TEXT NOT NULL,
		state TEXT NOT NULL, detail TEXT NOT NULL)`)
	workExec(t, s, "INSERT INTO sp_work_migration (at,step,state,detail) VALUES (?,?,?,?)",
		"2026-11-19T00:00:00Z", "migrate", "migrated", `{"tasks":1}`)
	workExec(t, s, `CREATE TABLE sp_work_retirement (
		seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, step TEXT NOT NULL,
		detail TEXT NOT NULL)`)
	workExec(t, s, "INSERT INTO sp_work_retirement (at,step,detail) VALUES (?,?,?)",
		"2026-11-20T12:00:00Z", "retire", `{"drop_tables":["sp_ledger_tasks"]}`)
	workExec(t, s, "CREATE TABLE sp_work_task_legacy (task_id TEXT PRIMARY KEY, document TEXT NOT NULL)")
	workExec(t, s, "INSERT INTO sp_work_task_legacy VALUES (?,?)", "task-a", `{"stage":"running"}`)
	workExec(t, s, "CREATE TABLE sp_work_turn_legacy (turn_id INTEGER PRIMARY KEY, document TEXT NOT NULL)")
}

// AC-N10: the move is over, so everything that existed to manage it goes — the
// step logs, the verbatim copies, and the transition state itself. The step is
// taken on open, so a parent never has to call anything to reach it, and there
// is no management tool left to call.
func TestOpeningAPastSlotEndsTheTransitionRecords(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedRetiredSlot(t, s)
	for _, table := range workTransitionTables {
		if !workTableIsThere(t, s, table) {
			t.Fatalf("seed did not create %s", table)
		}
	}
	if !workColumnIsThere(t, s, "sp_work_meta", "state") {
		t.Fatal("seed did not create the old two-column meta shape")
	}

	// An ordinary read takes the step — no transition tool is involved.
	page := mustWork(t, s, "task_list", map[string]any{})

	for _, table := range workTransitionTables {
		if workTableIsThere(t, s, table) {
			t.Fatalf("%s survived the step to the final schema", table)
		}
	}
	if workColumnIsThere(t, s, "sp_work_meta", "state") {
		t.Fatal("the transition state column survived; nothing can change it, so it is not a fact")
	}
	if version := workRow(t, s, "SELECT version AS v FROM sp_work_meta")["v"].(int64); version != workSchemaVersionFinal {
		t.Fatalf("schema version = %d, want %d", version, workSchemaVersionFinal)
	}
	// The work the move carried is untouched — only the bookkeeping about the
	// move went.
	if tasks, _ := page["tasks"].([]any); len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want the carried task still there", page["tasks"])
	}
	// Writes keep working after the step, against the rebuilt meta row.
	newWorkTask(t, s, "task-b", "after-step", map[string]any{"title": "단계 뒤의 작업"})
	if n := workRow(t, s, "SELECT count(*) AS n FROM sp_work_meta")["n"].(int64); n != 1 {
		t.Fatalf("meta rows = %d, want exactly one", n)
	}
	// Opening again is the same answer: the step is idempotent, and a second
	// open must not move a version it did not step.
	mustWork(t, s, "task_list", map[string]any{})
	if version := workRow(t, s, "SELECT version AS v FROM sp_work_meta")["v"].(int64); version != workSchemaVersionFinal {
		t.Fatalf("second open moved the version to %d", version)
	}
}

// A new slot is born past the move: the final version, no transition tables,
// no state column.
func TestANewSlotIsBornPastTheMove(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "작업"})

	if version := workRow(t, s, "SELECT version AS v FROM sp_work_meta")["v"].(int64); version != workSchemaVersionFinal {
		t.Fatalf("new slot version = %d, want %d", version, workSchemaVersionFinal)
	}
	if workColumnIsThere(t, s, "sp_work_meta", "state") {
		t.Fatal("a new slot was given a transition state")
	}
	for _, table := range workTransitionTables {
		if workTableIsThere(t, s, table) {
			t.Fatalf("a new slot was given %s", table)
		}
	}
}

// The protection for a slot the move never reached. This image has no door, so
// the honest answer is a refusal that names the upgrade path — not the empty
// page it would otherwise serve about rows that are sitting right there.
func TestASlotStillHoldingTheLedgerIsRefusedRatherThanServedEmpty(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "작업"})
	workExec(t, s, "CREATE TABLE sp_ledger_tasks (id TEXT PRIMARY KEY, document TEXT NOT NULL)")
	workExec(t, s, "INSERT INTO sp_ledger_tasks VALUES (?,?)", "ledger-1", `{"title":"아직 안 옮긴 작업"}`)

	decoded, code := workCall(t, s, "task_list", map[string]any{})
	if code != "schema_version" {
		t.Fatalf("task_list = %q %v, want schema_version", code, decoded)
	}
	message, _ := decoded["error"].(string)
	// The refusal has to carry the upgrade path, because there is no state
	// report left to look it up in.
	if !strings.Contains(message, workRetireTool) {
		t.Fatalf("refusal = %q, want it to name the image that can still move the records", message)
	}
	if !strings.Contains(message, "nothing was read or changed") {
		t.Fatalf("refusal = %q, want it to say the database was left alone", message)
	}
	// Refused means refused for writes too, and the ledger is left exactly as
	// it is for the image that can read it.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-b", "request_id": "r1", "expected_version": 0,
		"task": map[string]any{
			"title": "새 작업", "description": "목적", "completion_criteria": "끝나는 조건",
			"status": "planned",
		},
	}, "schema_version")
	if !workTableIsThere(t, s, "sp_ledger_tasks") {
		t.Fatal("the refusal dropped the ledger it refused to read")
	}
	if n := workRow(t, s, "SELECT count(*) AS n FROM sp_ledger_tasks")["n"].(int64); n != 1 {
		t.Fatalf("ledger rows = %d, want them untouched", n)
	}
}

// A slot left at version 1 with no ledger beside it is a move an older image
// did not finish. This image cannot reconstruct what was adopted, so it refuses
// rather than inventing a state — and names the image that can.
func TestASlotLeftMidMoveIsRefusedWithAnUpgradePath(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "작업"})
	workExec(t, s, "UPDATE sp_work_meta SET version=?", workSchemaVersion)

	decoded, code := workCall(t, s, "task_list", map[string]any{})
	if code != "schema_version" {
		t.Fatalf("task_list = %q %v, want schema_version", code, decoded)
	}
	if message, _ := decoded["error"].(string); !strings.Contains(message, workRetireTool) {
		t.Fatalf("refusal = %q, want the upgrade path named", message)
	}
}

// A version this image does not know is refused rather than read with the
// columns of this one — the same gate, in the other direction, that makes an
// older image refuse a database this one has stepped forward.
func TestAnUnknownSchemaVersionIsRefused(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "작업"})
	workExec(t, s, "UPDATE sp_work_meta SET version=?", workSchemaVersionFinal+1)

	wantWorkCode(t, s, "task_list", map[string]any{}, "schema_version")
}

// Every tool the transition offered is gone, and **none is re-offered under
// another name**: the surface is the agreed records plus the parent's own
// bookkeeping and its monitoring loops, and nothing else.
func TestNoTransitionToolSurvivesUnderAnyName(t *testing.T) {
	_, h := newTestSessionDB(t)
	gone := []string{workRetireTool, workStateTool, workBackupsTool, workBackupsPruneTool,
		"work_migrate", "work_migration_report", "work_rollback_export"}

	for _, name := range gone {
		if isWorkTool(name) {
			t.Fatalf("%s is still callable", name)
		}
		if !isRemovedWorkTool(name) || workRemovedToolSuccessor(name) == "" {
			t.Fatalf("%s is gone with no answer for a parent that calls it", name)
		}
	}
	// The names are the whole surface, so a renamed replacement would show up
	// here.
	if names := strings.Join(sessionDBToolNames(t, h), ","); names != workToolNames {
		t.Fatalf("tools = %s, want exactly %s", names, workToolNames)
	}
	// And no surviving tool advertises itself as the way to reach one of the
	// removed calls, which is how a replacement would arrive under a new name.
	for _, definition := range sessionDBToolDefinitions(false, workCaller{}) {
		name, _ := definition["name"].(string)
		text, _ := definition["description"].(string)
		for _, dead := range gone {
			if strings.Contains(text, dead) {
				t.Fatalf("%s's description names %s; a removed call must not be reachable through a listed tool", name, dead)
			}
		}
	}
}

// A call to a removed name arrives as a tools/call, not as a Go function. The
// refusal has to survive that path, must not be the generic "unknown tool", and
// must not point at a management call — there is none left to point at.
func TestCallingARemovedToolIsAnsweredWithoutANewTool(t *testing.T) {
	_, h := newTestSessionDB(t)
	gone := []string{workRetireTool, workStateTool, workBackupsTool, workBackupsPruneTool,
		"work_migrate", "work_migration_report", "work_rollback_export"}
	for _, name := range gone {
		resp := sessionDBCall(t, h, "tools/call", map[string]any{
			"name": name, "arguments": map[string]any{},
		})
		if resp.Error == nil {
			t.Fatalf("%s = %+v, want a refusal", name, resp.Result)
		}
		if strings.Contains(resp.Error.Message, "unknown tool") {
			t.Fatalf("%s = %q, want the real answer rather than \"unknown tool\"", name, resp.Error.Message)
		}
		if !strings.Contains(resp.Error.Message, workRemovedToolSuccessor(name)) {
			t.Fatalf("%s = %q, want it to carry %q", name, resp.Error.Message, workRemovedToolSuccessor(name))
		}
		// Whatever it says, it must not send the parent to another dead name.
		// The refusal quotes the name it was called with, and one dead name can
		// be a prefix of another (work_backups / work_backups_prune), so the
		// quote itself is what gets excluded here rather than any substring.
		answer := strings.TrimPrefix(resp.Error.Message, name)
		for _, dead := range gone {
			if dead != name && !strings.HasPrefix(name, dead) && strings.Contains(answer, dead) {
				t.Fatalf("%s points at %s, which is also gone: %q", name, dead, resp.Error.Message)
			}
		}
	}
}
