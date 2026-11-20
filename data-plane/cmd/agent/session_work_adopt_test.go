package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The ledger is not in this image any more, so a database that still has one is
// one an older image left behind. That is exactly what these fixtures build:
// the tables as that image wrote them, with raw SQL, and then the one-way move
// is asked to deal with them.
const legacyLedgerSchema = `CREATE TABLE sp_ledger_meta (version INTEGER NOT NULL);
INSERT INTO sp_ledger_meta VALUES (1);
CREATE TABLE sp_ledger_tasks (id TEXT PRIMARY KEY, version INTEGER NOT NULL, status TEXT NOT NULL, stage TEXT NOT NULL, owner TEXT NOT NULL, document TEXT NOT NULL);
CREATE TABLE sp_ledger_retired (id TEXT PRIMARY KEY);
CREATE TABLE sp_ledger_requests (id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, response TEXT NOT NULL);
CREATE TABLE sp_ledger_user_requests (
	id TEXT PRIMARY KEY, task_id TEXT NOT NULL, seq INTEGER NOT NULL, version INTEGER NOT NULL,
	text TEXT NOT NULL, author TEXT NOT NULL, created_at TEXT NOT NULL, status TEXT NOT NULL,
	claim_owner TEXT NOT NULL, claim_token TEXT NOT NULL, claim_expires_at TEXT NOT NULL,
	operation_key TEXT NOT NULL, operation_target TEXT NOT NULL, dispatch_state TEXT NOT NULL,
	dispatched_at TEXT NOT NULL, related_seq INTEGER, result TEXT NOT NULL, settled_at TEXT NOT NULL,
	choice_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE sp_ledger_task_events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL, at TEXT NOT NULL, kind TEXT NOT NULL,
	actor TEXT NOT NULL, summary TEXT NOT NULL, detail TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE sp_ledger_task_choices (
	task_id TEXT NOT NULL, choice_id TEXT NOT NULL, position INTEGER NOT NULL, label TEXT NOT NULL,
	request_text TEXT NOT NULL, basis_version INTEGER NOT NULL, created_at TEXT NOT NULL,
	PRIMARY KEY (task_id, choice_id)
);`

func ledgerTaskDocument(t *testing.T, title, stage, owner string, sessions []workBookSession, details map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"title": title, "stage": stage, "owner": owner, "sessions": sessions, "details": details,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// seedLegacyLedger is the state an older image leaves: a person's task that has
// moved between sessions and carries both live bookkeeping and a record of
// things that already happened, a monitoring loop that is still running, one
// that finished, a request nobody has answered, a request that was settled, the
// history rows the ledger wrote, live and stale choices, and a retired id.
func seedLegacyLedger(t *testing.T, s *sessionDB) {
	t.Helper()
	workExec(t, s, legacyLedgerSchema)
	workExec(t, s, "INSERT INTO sp_ledger_tasks VALUES (?,?,?,?,?,?)",
		"task-a", int64(3), "blocked", "implementation", "parent",
		ledgerTaskDocument(t, "앱에 작업 패널 띄우기", "implementation", "parent",
			[]workBookSession{
				{ID: "worker-old", Ownership: "owned", Purpose: "첫 구현", Cleanup: "deleted"},
				{ID: "worker-now", Ownership: "owned", Purpose: "이어서 구현", Cleanup: "pending", NextOffset: 4096, Seq: int64Ptr(12)},
				{ID: "reviewer", Ownership: "reused", Purpose: "리뷰", Cleanup: "retained", Reason: "후속 리뷰", NextAction: "리뷰 회수"},
			},
			map[string]any{
				"approval":     "PR only",
				"preservation": "unpushed work on worker-now",
				"result":       "PR #41 을 올렸습니다",
				"pr":           "https://example.invalid/pull/41",
				"observed_at":  "2026-10-01T00:00:00Z",
				"wildcard":     "부모가 쓰는 무언가",
			}))
	workExec(t, s, "INSERT INTO sp_ledger_tasks VALUES (?,?,?,?,?,?)",
		"task-m", int64(1), "running", "watch", "parent",
		ledgerTaskDocument(t, "모니터 루프 기록", "watch", "parent",
			[]workBookSession{{ID: "monitor-1", Ownership: "owned", Purpose: "모니터", Cleanup: "pending"}},
			map[string]any{"kind": "monitor", "last_seen": "2026-11-01T00:00:00Z"}))
	workExec(t, s, "INSERT INTO sp_ledger_tasks VALUES (?,?,?,?,?,?)",
		"task-m-done", int64(4), "completed", "watch", "parent",
		ledgerTaskDocument(t, "끝난 모니터 루프", "watch", "parent", nil,
			map[string]any{"kind": "monitor"}))
	workExec(t, s, "INSERT INTO sp_ledger_retired VALUES ('task-gone')")
	workExec(t, s, "INSERT INTO sp_ledger_requests VALUES ('rq-1','fp','{}')")
	// A request nobody has answered, and one that was settled.
	workExec(t, s, `INSERT INTO sp_ledger_user_requests
		(id,task_id,seq,version,text,author,created_at,status,claim_owner,claim_token,claim_expires_at,
		 operation_key,operation_target,dispatch_state,dispatched_at,related_seq,result,settled_at,choice_id)
		VALUES ('req-open','task-a',1,1,'이거 먼저 해 줘','user-1','2026-11-02T00:00:00Z','pending',
		        '','','','','','','',NULL,'','','')`)
	workExec(t, s, `INSERT INTO sp_ledger_user_requests
		(id,task_id,seq,version,text,author,created_at,status,claim_owner,claim_token,claim_expires_at,
		 operation_key,operation_target,dispatch_state,dispatched_at,related_seq,result,settled_at,choice_id)
		VALUES ('req-done','task-a',2,4,'저기 PR 올려 줘','user-1','2026-11-01T00:00:00Z','processed',
		        'parent','tok','2026-11-01T01:00:00Z','op-pr-1','pull/41','dispatched','2026-11-01T00:30:00Z',
		        31,'PR #41 을 올렸고 CI 는 초록입니다','2026-11-01T02:00:00Z','')`)
	workExec(t, s, `INSERT INTO sp_ledger_task_events (task_id,at,kind,actor,summary,detail)
		VALUES ('task-a','2026-10-01T00:00:00Z','task_created','parent','만들었다','{}'),
		       ('task-a','2026-11-01T02:00:00Z','request_settled','parent','끝냈다','{}')`)
	workExec(t, s, `INSERT INTO sp_ledger_task_choices VALUES
		('task-a','c-live',0,'왜 막혔는지 물어보기','왜 막혔는지 알려 줘',3,'2026-11-02T00:00:00Z'),
		('task-a','c-stale',1,'낡은 선택지','옛 기준의 문장',2,'2026-10-02T00:00:00Z')`)
}

func int64Ptr(v int64) *int64 { return &v }

// adoptedBook reads one task's bookkeeping as the parent now gets it.
func adoptedBook(t *testing.T, s *sessionDB, taskID string) map[string]any {
	t.Helper()
	return mustWork(t, s, taskBookGetTool, map[string]any{"task_id": taskID})
}

func planNotes(t *testing.T, plan map[string]any, key string) []string {
	t.Helper()
	notes, _ := plan[key].([]any)
	out := []string{}
	for _, raw := range notes {
		note, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		record, _ := note["record"].(string)
		part, _ := note["part"].(string)
		out = append(out, record+" / "+part)
	}
	return out
}

// The whole correction in one test: what a person is still waiting on moves,
// what is live beside it moves, and everything that was a record of something
// already finished goes — with the ledger tables themselves.
func TestRetireAdoptsWhatIsNeededAndRemovesTheLedger(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	// The slot is one an older image never migrated: its records are in the
	// ledger, and this call is the only way they move now.
	if state := workStateOf(t, s); state != workStateReady {
		t.Fatalf("state = %q, want %q", state, workStateReady)
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	out := retireAt(t, s, "retire-1", false, now)
	if out["state"] != workStateRetired {
		t.Fatalf("state = %v, want %s", out["state"], workStateRetired)
	}

	// Every ledger table is gone, including the one that held the history.
	for _, table := range workRetiredTables {
		if workTableIsThere(t, s, table) {
			t.Fatalf("%s is still there after the move", table)
		}
	}

	// The person's task is a work task, with the times the history recorded —
	// not this run's clock.
	task := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})["task"].(map[string]any)
	if task["title"] != "앱에 작업 패널 띄우기" || task["status"] != "blocked" {
		t.Fatalf("task = %v", task)
	}
	if task["created_at"] != "2026-10-01T00:00:00Z" {
		t.Fatalf("created_at = %v, want the time the history recorded", task["created_at"])
	}
	if task["session_id"] != "worker-now" {
		t.Fatalf("session_id = %v, want the live owned relation", task["session_id"])
	}

	// The request nobody answered is an open turn; the settled one is not a
	// turn at all.
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want only the request nobody has answered", len(turns))
	}
	turn := turns[0].(map[string]any)
	if turn["request"] != "이거 먼저 해 줘" || turn["ended_at"] != nil {
		t.Fatalf("turn = %v", turn)
	}
	if turn["requested_at"] != "2026-11-02T00:00:00Z" {
		t.Fatalf("requested_at = %v, want the time the person asked", turn["requested_at"])
	}
	// The live choices are on that turn; the stale ones were never shown.
	choices, _ := turn["choices"].([]any)
	if len(choices) != 1 || choices[0].(map[string]any)["label"] != "왜 막혔는지 물어보기" {
		t.Fatalf("choices = %v, want the one still offered", choices)
	}

	// The bookkeeping that is live moved; the keys that recorded something
	// already finished did not.
	book := adoptedBook(t, s, "task-a")
	if book["stage"] != "implementation" || book["owner"] != "parent" {
		t.Fatalf("book = %v", book)
	}
	details := book["details"].(map[string]any)
	for _, key := range []string{"approval", "preservation", "wildcard"} {
		if _, ok := details[key]; !ok {
			t.Fatalf("details lost %q: %v", key, details)
		}
	}
	for _, key := range []string{"result", "pr", "observed_at"} {
		if _, ok := details[key]; ok {
			t.Fatalf("details kept %q, which the turn records hold now: %v", key, details)
		}
	}
	relations := book["sessions"].([]any)
	if len(relations) != 2 {
		t.Fatalf("relations = %v, want the two that are not deleted", relations)
	}

	// The monitoring loop that is running is a record of its own; the one that
	// finished is not kept, and neither is ever a task.
	monitor := mustWork(t, s, monitorGetTool, map[string]any{"id": "task-m"})
	if monitor["title"] != "모니터 루프 기록" || monitor["state"] != monitorStateRunning {
		t.Fatalf("monitor = %v", monitor)
	}
	if !strings.Contains(monitor["note"].(string), "last_seen") {
		t.Fatalf("monitor note = %v, want what the loop last saw", monitor["note"])
	}
	wantWorkCode(t, s, monitorGetTool, map[string]any{"id": "task-m-done"}, "not_found")
	listed := mustWork(t, s, "task_list", map[string]any{})["tasks"].([]any)
	if len(listed) != 1 {
		t.Fatalf("task_list = %v, want the person's one task and no monitoring row", listed)
	}

	// A deleted id stays deleted: the retired id came across, so recreating it
	// is a conflict rather than a revival.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-gone", "request_id": "revive-1", "expected_version": 0,
		"task": map[string]any{"session_id": nil, "title": "되살리기", "description": "",
			"completion_criteria": "", "status": "planned"},
	}, "conflict")

	// The report says what it deleted, by name. A cleanup nobody can audit is
	// the thing this is replacing.
	plan := retirementPlan(t, out)
	dropped := strings.Join(planNotes(t, plan, "dropped"), "\n")
	for _, want := range []string{"details.result", "details.pr", "details.observed_at",
		"session worker-old", "sp_ledger_user_requests:req-done", "sp_ledger_task_events"} {
		if !strings.Contains(dropped, want) {
			t.Fatalf("the report does not name %q as dropped:\n%s", want, dropped)
		}
	}
	carried := strings.Join(planNotes(t, plan, "carried"), "\n")
	for _, want := range []string{"sp_work_turns:task-a", "sp_work_task_book:task-a", "sp_work_monitor:task-m",
		"sp_work_retired:task-gone"} {
		if !strings.Contains(carried, want) {
			t.Fatalf("the report does not name %q as carried:\n%s", want, carried)
		}
	}
}

// A request nobody has answered, on a task the work records do not have, has
// nowhere to go — and deleting it would delete a question a person is waiting
// on. So the whole run stops and says which one.
func TestRetireRefusesAnUnansweredRequestWithNoTask(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	workExec(t, s, `INSERT INTO sp_ledger_user_requests
		(id,task_id,seq,version,text,author,created_at,status,claim_owner,claim_token,claim_expires_at,
		 operation_key,operation_target,dispatch_state,dispatched_at,related_seq,result,settled_at,choice_id)
		VALUES ('req-monitor','task-m',3,1,'이 모니터 멈춰 줘','user-1','2026-11-03T00:00:00Z','pending',
		        '','','','','','','',NULL,'','','')`)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	err := retireFails(t, s, "retire-stranded", now)
	if !strings.Contains(err.Error(), "req-monitor") {
		t.Fatalf("refusal does not name the request: %v", err)
	}
	// Nothing was changed: the ledger is still there and the request with it.
	if !workTableIsThere(t, s, "sp_ledger_user_requests") {
		t.Fatal("the ledger went away on a run that refused")
	}
	row := workRow(t, s, "SELECT count(*) AS n FROM sp_ledger_user_requests WHERE id='req-monitor'")
	if row["n"].(int64) != 1 {
		t.Fatal("the request a person is waiting on was deleted by a refused run")
	}
	if state := workStateOf(t, s); state != workStateRetiring && state != workStateReady {
		t.Fatalf("state = %q after a refusal", state)
	}
}

// The rehearsal is the part a person approves on, so it has to describe the run
// exactly and write nothing.
func TestRetireRehearsalChangesNothing(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	out := retireAt(t, s, "rehearse-1", true, now)
	if out["dry_run"] != true {
		t.Fatalf("dry run = %v", out["dry_run"])
	}
	plan := retirementPlan(t, out)
	if len(planNotes(t, plan, "dropped")) == 0 || len(planNotes(t, plan, "carried")) == 0 {
		t.Fatalf("the rehearsal reported nothing: %v", plan)
	}
	for _, table := range []string{"sp_ledger_tasks", "sp_ledger_user_requests", "sp_ledger_task_events"} {
		if !workTableIsThere(t, s, table) {
			t.Fatalf("the rehearsal dropped %s", table)
		}
	}
	if state := workStateOf(t, s); state != workStateReady {
		t.Fatalf("state = %q after a rehearsal, want %q", state, workStateReady)
	}
	row := workRow(t, s, "SELECT count(*) AS n FROM sp_work_tasks")
	if row["n"].(int64) != 0 {
		t.Fatal("the rehearsal created tasks")
	}
}

// A second run adds nothing and takes nothing twice: every insert is keyed, and
// the state says it is done.
func TestRetireIsIdempotent(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	retireAt(t, s, "retire-1", false, now)
	again := retireAt(t, s, "retire-2", false, now.Add(time.Minute))
	if again["already_retired"] != true {
		t.Fatalf("second run = %v, want the report with already_retired", again)
	}
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("turns = %d after a second run, want 1", len(turns))
	}
}

// An older image that migrated this slot left the ledger standing and a
// verbatim copy of every moved row beside it. Both go, and what the parent
// still reads comes out of them first.
func TestRetireCleansUpAfterAnOlderImagesMigration(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	// The work records already hold the task, as that image's migration left
	// them, with the preserved copy beside it. The work schema has to exist
	// before a transition table is added beside it: an sp_work_* table with no
	// sp_work_meta is a namespace collision, which is the right answer for a
	// real one.
	mustWork(t, s, workStateTool, map[string]any{})
	workExec(t, s, `CREATE TABLE sp_work_task_legacy (
		task_id TEXT PRIMARY KEY, legacy_version INTEGER NOT NULL, legacy_status TEXT NOT NULL, kind TEXT NOT NULL,
		stage TEXT NOT NULL, owner TEXT NOT NULL, sessions TEXT NOT NULL, details TEXT NOT NULL,
		provenance TEXT NOT NULL)`)
	workExec(t, s, `CREATE TABLE sp_work_migration (
		seq INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, step TEXT NOT NULL, state TEXT NOT NULL,
		detail TEXT NOT NULL)`)
	workExec(t, s, "INSERT INTO sp_work_migration (at,step,state,detail) VALUES ('2026-11-10T00:00:00Z','copy','done','{\"counts\":{}}')")
	newWorkTask(t, s, "task-a", "seed-a", map[string]any{"title": "앱에 작업 패널 띄우기", "status": "blocked"})
	workExec(t, s, `INSERT INTO sp_work_task_legacy VALUES
		('task-a',3,'blocked','','implementation','parent','[]','{"approval":"PR only","result":"지난 결과"}','{}')`)
	workExec(t, s, "UPDATE sp_work_meta SET state='migrated'")

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	out := retireAt(t, s, "retire-after-migration", false, now)
	if out["state"] != workStateRetired {
		t.Fatalf("state = %v", out["state"])
	}
	for _, table := range []string{"sp_work_task_legacy", "sp_work_migration", "sp_ledger_tasks"} {
		if workTableIsThere(t, s, table) {
			t.Fatalf("%s survived", table)
		}
	}
	// The task the work records already had is not overwritten from a ledger
	// row nothing has written since.
	task := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})["task"].(map[string]any)
	if task["description"] != "목적과 요구" {
		t.Fatalf("the adopted row overwrote the work record: %v", task)
	}
	book := adoptedBook(t, s, "task-a")
	details := book["details"].(map[string]any)
	if _, ok := details["approval"]; !ok {
		t.Fatalf("the approval scope was lost: %v", details)
	}
	if _, ok := details["result"]; ok {
		t.Fatalf("a past result was carried: %v", details)
	}
	// How this slot got here stays readable, because the step log was folded in
	// before its table went.
	report := mustWork(t, s, workStateTool, map[string]any{})
	steps, _ := report["retirement"].([]any)
	if len(steps) == 0 {
		t.Fatalf("the retirement is not in the report: %v", report)
	}
}

// A retired slot does not offer the tools that moved its records, and a parent
// that still calls one by name is told what to call instead rather than
// "unknown tool".
func TestRemovedToolsNameTheirSuccessor(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	retireAt(t, s, "retire-1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))

	listed := map[string]bool{}
	for _, definition := range s.toolDefinitions(context.Background()) {
		listed[definition["name"].(string)] = true
	}
	for _, gone := range []string{
		"ledger_get", "ledger_list", "ledger_put", "ledger_delete", "ledger_choices_put",
		"ledger_requests_list", "ledger_requests_claim", "ledger_events_list",
		"work_migrate", "work_migration_report", workRollbackTool, workLegacyGetTool, workLegacyPutTool,
		workRetireTool,
	} {
		if listed[gone] {
			t.Fatalf("%s is still listed on a retired slot", gone)
		}
	}
	for _, kept := range []string{
		"task_get", "task_list", "task_put", "turn_append", "turn_list", "turn_dispatch",
		taskBookGetTool, taskBookPutTool, monitorListTool, monitorPutTool,
		workStateTool, workBackupsTool, workBackupsPruneTool,
	} {
		if !listed[kept] {
			t.Fatalf("%s is missing from a retired slot's tools", kept)
		}
	}

	for name, want := range map[string]string{
		"ledger_put":            "task_put",
		"ledger_requests_list":  "turn_list",
		"ledger_events_list":    "turn_list",
		"work_migrate":          workRetireTool,
		workLegacyPutTool:       taskBookPutTool,
		"work_migration_report": workStateTool,
		workRollbackTool:        workRetireTool,
	} {
		raw, _ := json.Marshal(map[string]any{})
		_, rpcErr := s.callTool(context.Background(), mustToolCall(t, name, raw))
		if rpcErr == nil {
			t.Fatalf("%s was accepted on a retired slot", name)
		}
		if !strings.Contains(rpcErr.Message, want) {
			t.Fatalf("%s refusal = %q, want it to name %q", name, rpcErr.Message, want)
		}
	}
}

func mustToolCall(t *testing.T, name string, args json.RawMessage) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A new slot has no ledger and nothing to move, so it starts where a retired
// slot ends rather than claiming there is a move to make.
func TestANewSlotStartsWithNothingToMove(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-1", "r1", nil)
	report := mustWork(t, s, workStateTool, map[string]any{})
	if report["state"] != workStateRetired {
		t.Fatalf("state = %v, want %q for a slot that never had a ledger", report["state"], workStateRetired)
	}
	if report["schema_version"].(float64) != workSchemaVersionRetired {
		t.Fatalf("schema_version = %v", report["schema_version"])
	}
	counts := report["counts"].(map[string]any)
	for name := range counts {
		if strings.HasPrefix(name, "sp_ledger_") {
			t.Fatalf("a new slot reports a ledger table: %s", name)
		}
	}
}

// The monitoring record is the parent's, with a lifecycle the ledger row never
// had: one row per loop, replaced under its own counter, and deleted when the
// loop is finished instead of kept as a row nothing reads.
func TestMonitorRecordIsOwnedAndEnds(t *testing.T) {
	s, _ := newTestSessionDB(t)
	first := mustWork(t, s, monitorPutTool, map[string]any{
		"id": "loop-1", "request_id": "m1", "expected_version": 0,
		"monitor": map[string]any{"title": "PR 감시", "state": monitorStateRunning, "note": "마지막 확인 10:00"},
	})
	if first["version"].(float64) != 1 {
		t.Fatalf("version = %v", first["version"])
	}
	// Two parents holding the same version: exactly one wins.
	mustWork(t, s, monitorPutTool, map[string]any{
		"id": "loop-1", "request_id": "m2", "expected_version": 1,
		"monitor": map[string]any{"title": "PR 감시", "state": monitorStateRunning, "note": "마지막 확인 10:05"},
	})
	wantWorkCode(t, s, monitorPutTool, map[string]any{
		"id": "loop-1", "request_id": "m3", "expected_version": 1,
		"monitor": map[string]any{"title": "PR 감시", "state": monitorStateRunning, "note": "경합"},
	}, "conflict")
	// A finished loop leaves rather than becoming a "completed" row.
	wantWorkCode(t, s, monitorPutTool, map[string]any{
		"id": "loop-1", "request_id": "m4", "expected_version": 2,
		"monitor": map[string]any{"title": "PR 감시", "state": "completed", "note": "끝"},
	}, "invalid_argument")
	mustWork(t, s, monitorDeleteTool, map[string]any{"id": "loop-1", "request_id": "m5", "expected_version": 2})
	wantWorkCode(t, s, monitorGetTool, map[string]any{"id": "loop-1"}, "not_found")
	// Unlike a task id, a monitor id is not retired: the same loop can be
	// recorded again.
	mustWork(t, s, monitorPutTool, map[string]any{
		"id": "loop-1", "request_id": "m6", "expected_version": 0,
		"monitor": map[string]any{"title": "PR 감시", "state": monitorStateRunning, "note": "다시 시작"},
	})
	// A monitoring loop is not a task and never shows up as one.
	if listed := mustWork(t, s, "task_list", map[string]any{})["tasks"].([]any); len(listed) != 0 {
		t.Fatalf("task_list = %v, want no monitoring rows", listed)
	}
}

func workStateOf(t *testing.T, s *sessionDB) string {
	t.Helper()
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.workRecordsState(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return state
}
