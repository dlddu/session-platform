package main

import (
	"context"
	"os"
	"testing"
)

// AC-N9 rollback: "the older image still opens the database and still serves
// the ledger" is not enough. Everything written after the cutover lives in
// tables that image does not read, so a rollback without this step shows a
// person work that silently stopped at the cutover. This is the rehearsal, on
// an isolated fixture database: migrate, work after the cutover, export, and
// check the older image's own view — the ledger — actually holds it.
func TestWorkRollbackCarriesPostCutoverWorkBackIntoTheLedger(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	seedLedger(t, s)
	runMigration(t, s, "m1", false)

	// Work done after the cutover, in all three ways it can arrive.
	newWorkTask(t, s, "task-after", "a1", map[string]any{
		"title": "커토버 뒤에 생긴 작업", "description": "무엇을 why", "completion_criteria": "머지되면 끝",
	})
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-after", "request_id": "t1", "expected_version": 1,
		"turn": map[string]any{"request": "세션이 받아 적은 사람의 말"},
	})
	// A turn with no request at all: the ledger shape has nowhere to hold it,
	// which has to be reported rather than dropped quietly.
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-after", "request_id": "t2", "expected_version": 1,
		"turn": map[string]any{"question": "이렇게 할까요?"},
	})
	// And a person's request through the route an un-updated app uses.
	if _, _, err := s.appendLedgerRequest(
		ctx, "req-after", "task-after", "앱에서 보낸 말", "user-9", "", 0, ledgerNow()); err != nil {
		t.Fatal(err)
	}

	// One of the turns is answered before the export, so the export has both an
	// answered and an unanswered exchange to carry.
	answered := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-after"})["turns"].([]any)[0].(map[string]any)
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-after", "turn_id": int64(answered["id"].(float64)),
		"request_id": "ans-1", "expected_version": 1,
		"turn": map[string]any{
			"response": "세션이 스스로 쓴 원문 답", "explanation": "부모가 사람에게 쓴 설명",
			"ended_at": "2026-11-19T03:00:00Z",
		},
	})

	// The rehearsal changes nothing and says what a run would write.
	before := countRows(t, s, "SELECT count(*) FROM sp_ledger_tasks")
	dry := mustWork(t, s, workRollbackTool, map[string]any{"request_id": "rb-dry", "dry_run": true})
	if dry["dry_run"] != true {
		t.Fatalf("dry run = %v", dry)
	}
	if countRows(t, s, "SELECT count(*) FROM sp_ledger_tasks") != before {
		t.Fatal("the rehearsal wrote to the ledger")
	}
	plan := dry["plan"].(map[string]any)["counts"].(map[string]any)
	if plan["tasks_written"].(float64) != 1 || plan["requests_written"].(float64) != 2 {
		t.Fatalf("rehearsal counts = %v", plan)
	}
	if plan["turns_unexportable"].(float64) != 1 {
		t.Fatalf("the turn with no request was not reported: %v", plan)
	}

	// The real export.
	out := mustWork(t, s, workRollbackTool, map[string]any{"request_id": "rb-1"})
	if out["state"] != workStateRolledBack {
		t.Fatalf("export = %v", out)
	}
	snapshot := out["snapshot"].(map[string]any)
	path := snapshot["path"].(string)
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("the export took no snapshot: %v", err)
	}
	if snapshot["sha256"] == "" {
		t.Fatal("the snapshot has no checksum")
	}

	// What the older image reads is the ledger, so that is where the check is.
	// The task created after the cutover is there, with its version.
	row := workRow(t, s, "SELECT version,status,document FROM sp_ledger_tasks WHERE id='task-after'")
	if row == nil {
		t.Fatal("work created after the cutover is invisible to the older image")
	}
	if row["version"].(int64) != 1 {
		t.Fatalf("exported version = %v", row["version"])
	}
	document := row["document"].(string)
	for _, want := range []string{"커토버 뒤에 생긴 작업", "무엇을 why", "머지되면 끝"} {
		if !contains(document, want) {
			t.Fatalf("exported document lost %q: %s", want, document)
		}
	}
	// Both people's requests are readable as ledger user requests, with their
	// words verbatim and the author the ledger carried.
	for id, want := range map[string]string{
		"req-after": "앱에서 보낸 말",
	} {
		stored := workRow(t, s, "SELECT text,author,task_id,status FROM sp_ledger_user_requests WHERE id=?", id)
		if stored == nil || stored["text"] != want {
			t.Fatalf("request %s = %v", id, stored)
		}
		if stored["author"] != "user-9" {
			t.Fatalf("request %s lost its author: %v", id, stored)
		}
	}
	// The session-written turn is there too, under a derived id.
	if n := countRows(t, s, "SELECT count(*) FROM sp_ledger_user_requests WHERE task_id='task-after'"); n != 2 {
		t.Fatalf("exported requests for task-after = %d, want 2", n)
	}
	// The migrated task's original request rows were not duplicated.
	if n := countRows(t, s,
		"SELECT count(*) FROM sp_ledger_user_requests WHERE task_id='task-a'"); n != 2 {
		t.Fatalf("migrated requests were rewritten: %d", n)
	}

	// While rolled back, the ledger owns the database: work writes are refused
	// and say which direction to go, and ledger writes are allowed again — so
	// the older image can keep working without diverging from a second writer.
	wantWorkCode(t, s, "task_put", map[string]any{
		"id": "task-after", "request_id": "nope", "expected_version": 1,
		"task": map[string]any{
			"session_id": nil, "title": "t", "description": "d",
			"completion_criteria": "c", "status": "planned",
		},
	}, "conflict")
	if _, err := s.ledger(ctx, "ledger_put", ledgerArgs{
		ID: "task-after", RequestID: "old-image-1", ExpectedVersion: lv(1),
		Task: &ledgerTask{
			Title: "옛 이미지가 이어서 고친 제목", Status: "running", Stage: "implementation",
			Owner: "parent", Sessions: []ledgerSession{}, Details: map[string]any{},
		},
	}); err != nil {
		t.Fatalf("the older image cannot write the ledger it now owns: %v", err)
	}
	// A person's request still lands, as a ledger row rather than a turn.
	if _, _, err := s.appendLedgerRequest(
		ctx, "req-while-back", "task-after", "롤백 중에 보낸 말", "user-9", "", 0, ledgerNow()); err != nil {
		t.Fatalf("a request during the rollback was refused: %v", err)
	}

	// Evidence the review asked for, point by point, read from the ledger the
	// older image would be reading.
	//
	// (a) The original turn id survives the export: an exported turn records
	// which turn it is, so coming forward it is recognised as that turn rather
	// than imported as a new one.
	exported := workRow(t, s,
		`SELECT l.turn_id, l.source_id, l.source, r.text, r.seq, r.status
		   FROM sp_work_turn_legacy l JOIN sp_ledger_user_requests r ON r.id = l.source_id
		  WHERE l.source='exported_turn'`)
	if exported == nil {
		t.Fatal("the export recorded no mapping back to the turn it wrote")
	}
	exportedTurnID := exported["turn_id"].(int64)
	if exported["text"] != "세션이 받아 적은 사람의 말" {
		t.Fatalf("(b) exported text is not verbatim: %v", exported["text"])
	}
	// (d) An answered turn exports with the parent's sentence as the ledger
	// result — never the session's own words, which a ledger result never was.
	if exported["status"] != requestProcessed {
		t.Fatalf("an answered turn exported as %v", exported["status"])
	}
	settled := workRow(t, s,
		"SELECT result FROM sp_ledger_user_requests WHERE id=?", exported["source_id"])
	if settled["result"] != "부모가 사람에게 쓴 설명" {
		t.Fatalf("(d) exported result = %v, want the parent's sentence", settled["result"])
	}
	if settled["result"] == "세션이 스스로 쓴 원문 답" {
		t.Fatal("(d) the session's own words were dressed up as a ledger result")
	}
	// The app-stored request keeps the very id it was stored under, so the
	// older image sees the same request it would have had.
	appRow := workRow(t, s,
		"SELECT text,author,status FROM sp_ledger_user_requests WHERE id='req-after'")
	if appRow["text"] != "앱에서 보낸 말" || appRow["author"] != "user-9" {
		t.Fatalf("(b) app request not verbatim: %v", appRow)
	}
	if appRow["status"] != requestPending {
		t.Fatalf("(c) an unclaimed app request exported as %v", appRow["status"])
	}
	// Coming forward again: keyed, so nothing is duplicated, and what the older
	// image wrote while it owned the database is imported.
	again := runMigration(t, s, "m2", false)
	if again["state"] != workStateMigrated {
		t.Fatalf("forward again = %v", again)
	}
	task := mustWork(t, s, "task_get", map[string]any{"id": "task-after"})["task"].(map[string]any)
	if task["title"] != "옛 이미지가 이어서 고친 제목" {
		t.Fatalf("the older image's edit was not imported: %v", task)
	}
	// The request written while rolled back is a turn now, and the turns that
	// were already there were not duplicated.
	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-after", "limit": 100})["turns"].([]any)
	words := map[string]int{}
	for _, raw := range turns {
		turn := raw.(map[string]any)
		if request, ok := turn["request"].(string); ok {
			words[request]++
		}
	}
	if words["롤백 중에 보낸 말"] != 1 {
		t.Fatalf("the request written while rolled back did not come forward: %v", words)
	}
	for _, said := range []string{"앱에서 보낸 말", "세션이 받아 적은 사람의 말"} {
		if words[said] != 1 {
			t.Fatalf("%q appears %d times after the round trip (duplication or loss): %v",
				said, words[said], words)
		}
	}
	// The question-only turn survived the round trip in the work records even
	// though the ledger could not hold it.
	questions := 0
	for _, raw := range turns {
		if _, ok := raw.(map[string]any)["question"].(string); ok {
			questions++
		}
	}
	if questions != 1 {
		t.Fatalf("the question-only turn was lost or duplicated: %d", questions)
	}
	// (a) again, after the round trip: the turn kept its original id rather
	// than being re-created under a new one.
	stillThere := workRow(t, s,
		"SELECT id,request FROM sp_work_turns WHERE id=?", exportedTurnID)
	if stillThere == nil || stillThere["request"] != "세션이 받아 적은 사람의 말" {
		t.Fatalf("(a) the original turn id did not survive the round trip: %v", stillThere)
	}
	// (e) and nothing was duplicated under a second id.
	if n := countRows(t, s,
		"SELECT count(*) FROM sp_work_turns WHERE request='세션이 받아 적은 사람의 말'"); n != 1 {
		t.Fatalf("(e) the exported turn came back %d times", n)
	}
	// (c) the request nobody answered is still open work after the round trip,
	// not quietly closed by the trip itself.
	open := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-after", "open": true})["turns"].([]any)
	stillOpen := map[string]bool{}
	for _, raw := range open {
		if request, ok := raw.(map[string]any)["request"].(string); ok {
			stillOpen[request] = true
		}
	}
	if !stillOpen["앱에서 보낸 말"] || !stillOpen["롤백 중에 보낸 말"] {
		t.Fatalf("(c) unprocessed requests stopped being open work after the round trip: %v", stillOpen)
	}
	if stillOpen["세션이 받아 적은 사람의 말"] {
		t.Fatal("(c) an answered turn came back as open work")
	}

	// And work writes are allowed again.
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-after", "request_id": "t3",
		"expected_version": task["version"],
		"turn":             map[string]any{"request": "다시 앞으로"},
	})
}

// A rollback export on a slot that never migrated is refused rather than
// writing the ledger over itself.
func TestWorkRollbackRefusedBeforeTheCutover(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLedger(t, s)
	wantWorkCode(t, s, workRollbackTool, map[string]any{"request_id": "rb-early"}, "conflict")
}

func countRows(t *testing.T, s *sessionDB, query string, args ...any) int64 {
	t.Helper()
	row := workRow(t, s, query, args...)
	for _, v := range row {
		if n, ok := v.(int64); ok {
			return n
		}
	}
	t.Fatalf("not a count: %v", row)
	return 0
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
