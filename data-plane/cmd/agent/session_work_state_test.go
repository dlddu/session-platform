package main

import (
	"context"
	"encoding/json"
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

// seedRetiredSlot builds the database an older image's retirement left: the
// records are the work records, the version says retired, and the only things
// beside them are the move's own bookkeeping — the step logs and the verbatim
// copies it kept. This image's job is to drop exactly those.
func seedRetiredSlot(t *testing.T, s *sessionDB) {
	t.Helper()
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "옮겨온 작업"})
	workExec(t, s, "UPDATE sp_work_meta SET version=?, state=?", workSchemaVersionRetired, workStateRetired)
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

// AC-N10: the move is finished, so the records that existed only to manage it
// go. They are dropped on the one step to the final schema version, and the
// step is taken on open — a parent never has to call anything to get there.
func TestOpeningARetiredSlotDropsTheMovesOwnBookkeeping(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedRetiredSlot(t, s)
	for _, table := range workTransitionTables {
		if !workTableIsThere(t, s, table) {
			t.Fatalf("seed did not create %s", table)
		}
	}

	report := mustWork(t, s, workStateTool, map[string]any{})

	for _, table := range workTransitionTables {
		if workTableIsThere(t, s, table) {
			t.Fatalf("%s survived the step to the final schema", table)
		}
	}
	if version := workRow(t, s, "SELECT version AS v FROM sp_work_meta")["v"].(int64); version != workSchemaVersionFinal {
		t.Fatalf("schema version = %d, want %d", version, workSchemaVersionFinal)
	}
	// The work the move carried is untouched — only the bookkeeping about the
	// move went.
	if counts := report["counts"].(map[string]any); counts["work_tasks"] != float64(1) {
		t.Fatalf("counts = %+v, want the carried task still there", counts)
	}
	if report["state"] != workStateRetired {
		t.Fatalf("state = %v, want %q", report["state"], workStateRetired)
	}
	// Opening again is the same answer: the drop is idempotent, and a second
	// open must not report a version step it did not take.
	mustWork(t, s, workStateTool, map[string]any{})
	if version := workRow(t, s, "SELECT version AS v FROM sp_work_meta")["v"].(int64); version != workSchemaVersionFinal {
		t.Fatalf("second open moved the version to %d", version)
	}
}

// The protection for a slot the move never reached. This image has no door, so
// the honest answer is a refusal naming the image that still has one — not the
// empty page it would otherwise serve about rows that are sitting right there.
func TestASlotStillHoldingTheLedgerIsRefusedRatherThanServedEmpty(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "작업"})
	workExec(t, s, "CREATE TABLE sp_ledger_tasks (id TEXT PRIMARY KEY, document TEXT NOT NULL)")
	workExec(t, s, "INSERT INTO sp_ledger_tasks VALUES (?,?)", "ledger-1", `{"title":"아직 안 옮긴 작업"}`)

	decoded, code := workCall(t, s, "task_list", map[string]any{})
	if code != "schema_version" {
		t.Fatalf("task_list = %q %v, want schema_version", code, decoded)
	}
	if message, _ := decoded["error"].(string); !strings.Contains(message, workRetireTool) {
		t.Fatalf("refusal = %q, want it to name the call that can still move the records", message)
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
// rather than inventing a state.
func TestASlotLeftMidMoveIsRefused(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "작업"})
	workExec(t, s, "UPDATE sp_work_meta SET version=?, state=?", workSchemaVersion, workStateRetiring)

	wantWorkCode(t, s, "task_list", map[string]any{}, "schema_version")
}

// The removed tools are neither listed nor callable, and a parent holding an
// older tools/list is told what to call instead rather than "unknown tool".
func TestTheMovesToolsAreGoneAndNameWhatToCallInstead(t *testing.T) {
	s, _ := newTestSessionDB(t)
	removed := []string{workRetireTool, workBackupsTool, workBackupsPruneTool,
		"work_migrate", "work_migration_report", "work_rollback_export"}

	for _, name := range removed {
		if isWorkTool(name) {
			t.Fatalf("%s is still callable", name)
		}
		if !isRemovedWorkTool(name) || workRemovedToolSuccessor(name) == "" {
			t.Fatalf("%s is gone with no successor named", name)
		}
	}
	for _, definition := range sessionDBToolDefinitions(false) {
		name, _ := definition["name"].(string)
		for _, gone := range removed {
			if name == gone {
				t.Fatalf("tools/list still offers %s", gone)
			}
		}
	}
	// work_state is the one call from the transition that stays, and it reports
	// the removed names so a parent reads them once instead of discovering one
	// refusal at a time.
	if !isWorkTool(workStateTool) {
		t.Fatalf("%s must stay callable", workStateTool)
	}
	report := mustWork(t, s, workStateTool, map[string]any{})
	listed, err := json.Marshal(report["removed_tools"])
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range removed {
		if !strings.Contains(string(listed), `"`+gone+`"`) {
			t.Fatalf("removed_tools = %s, want it to name %s", listed, gone)
		}
	}
}

// A call to a removed tool arrives as a tools/call, not as a Go function. The
// refusal has to survive that path naming its successor, and it must not be
// the generic "unknown tool" — a parent acts on the difference.
func TestCallingARemovedToolIsRefusedWithItsSuccessor(t *testing.T) {
	_, h := newTestSessionDB(t)
	for _, name := range []string{workRetireTool, workBackupsTool, workBackupsPruneTool,
		"work_migrate", "work_migration_report", "work_rollback_export"} {
		resp := sessionDBCall(t, h, "tools/call", map[string]any{
			"name": name, "arguments": map[string]any{},
		})
		if resp.Error == nil {
			t.Fatalf("%s = %+v, want a refusal", name, resp.Result)
		}
		if strings.Contains(resp.Error.Message, "unknown tool") {
			t.Fatalf("%s = %q, want the successor rather than \"unknown tool\"", name, resp.Error.Message)
		}
		if !strings.Contains(resp.Error.Message, workRemovedToolSuccessor(name)) {
			t.Fatalf("%s = %q, want it to name %q", name, resp.Error.Message, workRemovedToolSuccessor(name))
		}
	}
}
