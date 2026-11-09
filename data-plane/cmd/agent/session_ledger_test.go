package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func lv(n int64) *int64 { return &n }
func taskArgs(id, request string, version int64) ledgerArgs {
	return ledgerArgs{ID: id, RequestID: request, ExpectedVersion: lv(version), Task: &ledgerTask{Title: "한글 ' ?", Status: "planned", Stage: "implementation", Owner: "parent", Sessions: []ledgerSession{{ID: "worker", Ownership: "owned", Purpose: "test", Cleanup: "pending", NextOffset: 123}}, Details: map[string]any{"approval": "PR only"}}}
}
func assertLedgerCode(t *testing.T, err error, code string) {
	t.Helper()
	var e ledgerError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error=%v want %s", err, code)
	}
}
func TestLedgerCRUDReplayAndCAS(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	a := taskArgs("a", "create", 0)
	first, err := s.ledger(ctx, "ledger_put", a)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ledger(ctx, "ledger_put", a)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(first)
	y, _ := json.Marshal(replay)
	if string(x) != string(y) {
		t.Fatal("replay differs")
	}
	a.Task.Title = "different"
	_, err = s.ledger(ctx, "ledger_put", a)
	assertLedgerCode(t, err, "conflict")
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for _, id := range []string{"u1", "u2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, e := s.ledger(ctx, "ledger_put", taskArgs("a", id, 1))
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				wins++
			} else {
				var le ledgerError
				if errors.As(e, &le) && le.Code == "conflict" {
					conflicts++
				}
			}
		}(id)
	}
	wg.Wait()
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	_, err = s.ledger(ctx, "ledger_put", taskArgs("missing", "missing", 1))
	assertLedgerCode(t, err, "not_found")
	del := ledgerArgs{ID: "a", RequestID: "delete", ExpectedVersion: lv(2)}
	if _, err = s.ledger(ctx, "ledger_delete", del); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ledger(ctx, "ledger_delete", del); err != nil {
		t.Fatal("delete replay", err)
	}
	_, err = s.ledger(ctx, "ledger_get", ledgerArgs{ID: "a"})
	assertLedgerCode(t, err, "not_found")
	_, err = s.ledger(ctx, "ledger_put", taskArgs("a", "reuse", 0))
	assertLedgerCode(t, err, "conflict")
}
func TestLedgerMigrationRollbackIsolationReopen(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSessionDB(t)
	if _, err := s.exec(ctx, sessionDBStatement{SQL: "CREATE TABLE user_notes(body TEXT); INSERT INTO user_notes VALUES ('keep'); PRAGMA user_version=42"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ledger(ctx, "ledger_put", taskArgs("a", "create", 0)); err != nil {
		t.Fatal(err)
	}
	// Fail after the task write to prove the mutation and receipt are one transaction.
	if _, err := s.exec(ctx, sessionDBStatement{SQL: "CREATE TRIGGER fail_receipt BEFORE INSERT ON sp_ledger_requests BEGIN SELECT RAISE(ABORT,'injected'); END"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ledger(ctx, "ledger_put", taskArgs("b", "fail", 0)); err == nil {
		t.Fatal("expected failure")
	}
	_, err := s.ledger(ctx, "ledger_get", ledgerArgs{ID: "b"})
	assertLedgerCode(t, err, "not_found")
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ledger(ctx, "ledger_get", ledgerArgs{ID: "a"}); err != nil {
		t.Fatal("reopen", err)
	}
	q, err := s.query(ctx, sessionDBStatement{SQL: "SELECT body FROM user_notes"})
	if err != nil || q.Rows[0][0] != "keep" {
		t.Fatal(q, err)
	}
	q, err = s.query(ctx, sessionDBStatement{SQL: "PRAGMA user_version"})
	if err != nil || q.Rows[0][0] != int64(42) {
		t.Fatal(q, err)
	}
	other, _ := newTestSessionDB(t)
	_, err = other.ledger(ctx, "ledger_get", ledgerArgs{ID: "a"})
	assertLedgerCode(t, err, "not_found")
	if _, err = s.exec(ctx, sessionDBStatement{SQL: "UPDATE sp_ledger_meta SET version=99"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.ledger(ctx, "ledger_put", taskArgs("c", "future", 0))
	assertLedgerCode(t, err, "schema_version")
}
func TestLedgerPaginationAndValidation(t *testing.T) {
	s, h := newTestSessionDB(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if _, e := s.ledger(ctx, "ledger_put", taskArgs(id, id, 0)); e != nil {
			t.Fatal(e)
		}
	}
	p, e := s.ledger(ctx, "ledger_list", ledgerArgs{Limit: 2, Status: "planned", SessionID: "worker"})
	if e != nil {
		t.Fatal(e)
	}
	page := p.(map[string]any)
	if page["next_cursor"] != "b" || len(page["tasks"].([]ledgerRecord)) != 2 {
		t.Fatal(page)
	}
	p, e = s.ledger(ctx, "ledger_list", ledgerArgs{Limit: 2, AfterID: "b"})
	if e != nil || len(p.(map[string]any)["tasks"].([]ledgerRecord)) != 1 {
		t.Fatal(p, e)
	}
	bad := taskArgs("bad", "bad", 0)
	bad.Task.Sessions[0].NextOffset = -1
	_, e = s.ledger(ctx, "ledger_put", bad)
	assertLedgerCode(t, e, "invalid_argument")
	bad = taskArgs("bad", "bad", 0)
	bad.Task.Sessions[0].Cleanup = "retained"
	_, e = s.ledger(ctx, "ledger_put", bad)
	assertLedgerCode(t, e, "invalid_argument")
	for _, args := range []any{map[string]any{"id": "a", "sql": "DROP TABLE user_notes"}, map[string]any{"id": "../escape"}, map[string]any{"id": "a", "expected_version": 1.2}} {
		r := sessionDBCall(t, h, "tools/call", map[string]any{"name": "ledger_get", "arguments": args})
		if r.Error != nil || !r.Result.(map[string]any)["isError"].(bool) {
			t.Fatalf("invalid accepted %+v", r)
		}
	}
}
func TestRawTransactionsDoNotLeak(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	for _, stmt := range []string{"CREATE TABLE raw_notes(n)", "BEGIN; INSERT INTO raw_notes VALUES(1); ROLLBACK", "BEGIN; INSERT INTO raw_notes VALUES(2)", "BEGIN; INSERT INTO raw_notes VALUES(3); SELECT * FROM missing"} {
		s.exec(ctx, sessionDBStatement{SQL: stmt})
	}
	q, e := s.query(ctx, sessionDBStatement{SQL: "SELECT count(*) FROM raw_notes"})
	if e != nil || q.Rows[0][0] != int64(0) {
		t.Fatal(q, e)
	}
	if _, e = s.ledger(ctx, "ledger_put", taskArgs("a", "a", 0)); e != nil {
		t.Fatal("raw transaction leaked", e)
	}
}

func TestLedgerSchemaCollisionPreserved(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	if _, err := s.exec(ctx, sessionDBStatement{SQL: "CREATE TABLE sp_ledger_tasks (note TEXT); INSERT INTO sp_ledger_tasks VALUES ('existing')"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ledger(ctx, "ledger_put", taskArgs("a", "a", 0))
	assertLedgerCode(t, err, "schema_version")
	q, err := s.query(ctx, sessionDBStatement{SQL: "SELECT note FROM sp_ledger_tasks"})
	if err != nil || q.Rows[0][0] != "existing" {
		t.Fatal(q, err)
	}
}

func TestLedgerMCPRoundTripAndRequiredCursor(t *testing.T) {
	_, h := newTestSessionDB(t)
	r := sessionDBCall(t, h, "tools/call", map[string]any{"name": "ledger_put", "arguments": taskArgs("api", "api", 0)})
	if r.Error != nil || r.Result.(map[string]any)["isError"].(bool) {
		t.Fatalf("put failed %+v", r)
	}
	r = sessionDBCall(t, h, "tools/call", map[string]any{"name": "ledger_get", "arguments": ledgerArgs{ID: "api"}})
	if r.Error != nil || r.Result.(map[string]any)["isError"].(bool) {
		t.Fatalf("get failed %+v", r)
	}
	b, _ := json.Marshal(taskArgs("missing-cursor", "missing-cursor", 0))
	var args map[string]any
	json.Unmarshal(b, &args)
	rel := args["task"].(map[string]any)["sessions"].([]any)[0].(map[string]any)
	delete(rel, "nextOffset")
	r = sessionDBCall(t, h, "tools/call", map[string]any{"name": "ledger_put", "arguments": args})
	if !r.Result.(map[string]any)["isError"].(bool) {
		t.Fatal("missing cursor accepted")
	}
}
