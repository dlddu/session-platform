package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Handing a running session's work records to its monitor, and taking them
// back, through the product's own surfaces.
//
// The session here is the hard case an environment variable cannot serve: it
// was started before any monitor existed, so its process was told it owns its
// work and goes on believing that, and nothing restarts it. What stops it
// writing is the seal in its own records, checked inside the transaction that
// would do the writing (session_work_seal.go), reached through the sidecar's
// loopback routes (session_work_seal_http.go) — the routes the control plane
// relays through the agent.
//
// Nothing is faked: a real SQLite slot, the real MCP tools/call entry for the
// session's own model, the real HTTP handlers for the control plane's side.

// handoverSlot is one session's work surface, reachable both ways: as its own
// model through the slot MCP, and as the control plane over loopback.
type handoverSlot struct {
	t    *testing.T
	db   *sessionDB
	base string
}

// openHandoverSlot opens a slot directory. Calling it again on the same
// directory is a restart: a new process, the same records, and no memory of
// what the last one was told.
func openHandoverSlot(t *testing.T, dir, role string) *handoverSlot {
	t.Helper()
	db, err := newSessionDB(dir, false, role, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(withSessionDBMCP(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "not the work surface", http.StatusNotFound)
		}), db))
	t.Cleanup(srv.Close)
	return &handoverSlot{t: t, db: db, base: srv.URL}
}

// own is a call by the session's own model, through the slot MCP.
func (s *handoverSlot) own(name string, args map[string]any) (map[string]any, string) {
	s.t.Helper()
	return workCall(s.t, s.db, name, args)
}

func (s *handoverSlot) mustOwn(name string, args map[string]any) map[string]any {
	s.t.Helper()
	return mustWork(s.t, s.db, name, args)
}

// post sends one loopback request, the way the agent relays the control plane's.
func (s *handoverSlot) post(path string, body any) (int, map[string]any) {
	s.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		s.t.Fatal(err)
	}
	resp, err := http.Post(s.base+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		s.t.Fatalf("POST %s: %v", path, err)
	}
	return resp.StatusCode, decodeSealBody(s.t, resp)
}

func (s *handoverSlot) get(path string) (int, map[string]any) {
	s.t.Helper()
	resp, err := http.Get(s.base + path)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	return resp.StatusCode, decodeSealBody(s.t, resp)
}

func decodeSealBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if json.Unmarshal(body, &out) != nil {
		return map[string]any{"text": strings.TrimSpace(string(body))}
	}
	return out
}

// seal is the control plane's handover or rollback.
func (s *handoverSlot) seal(writer, holder string, expectedEpoch *int64) (int, map[string]any) {
	s.t.Helper()
	body := map[string]any{"writer": writer, "holder": holder}
	if expectedEpoch != nil {
		body["expected_epoch"] = *expectedEpoch
	}
	return s.post(sessionDBWorkWriterPath, body)
}

// writerNow is the fact a reader can check at any time: who may write, at which
// epoch, held by whom.
func (s *handoverSlot) writerNow() map[string]any {
	s.t.Helper()
	status, body := s.get(sessionDBWorkWriterPath)
	if status != http.StatusOK {
		s.t.Fatalf("GET writer answered %d: %+v", status, body)
	}
	writer, ok := body["writer"].(map[string]any)
	if !ok {
		s.t.Fatalf("writer view carried no writer: %+v", body)
	}
	return writer
}

// asMonitor is one work tool call made by the bound monitor through the control
// plane. It returns the tool's own result and its error code, exactly as the
// session's own model would see them.
func (s *handoverSlot) asMonitor(name string, args map[string]any) (map[string]any, string) {
	s.t.Helper()
	status, body := s.post(sessionDBWorkCallPath, map[string]any{"name": name, "args": args})
	if status != http.StatusOK && status != http.StatusConflict {
		s.t.Fatalf("monitor call %s answered %d: %+v", name, status, body)
	}
	content, _ := body["content"].([]any)
	if len(content) == 0 {
		s.t.Fatalf("monitor call %s carried no content: %+v", name, body)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var decoded map[string]any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		s.t.Fatalf("monitor call %s result was not JSON: %s", name, text)
	}
	if body["isError"] == true {
		code, _ := decoded["code"].(string)
		return decoded, code
	}
	return decoded, ""
}

func (s *handoverSlot) mustAsMonitor(name string, args map[string]any) map[string]any {
	s.t.Helper()
	decoded, code := s.asMonitor(name, args)
	if code != "" {
		s.t.Fatalf("monitor call %s: %s %v", name, code, decoded["error"])
	}
	return decoded
}

// turnTexts is the task's history as a reader sees it, oldest first.
func (s *handoverSlot) turnTexts(taskID string) []string {
	s.t.Helper()
	listed := s.mustOwn("turn_list", map[string]any{"task_id": taskID})
	out := []string{}
	for _, row := range monitorRows(listed["turns"]) {
		request, _ := row["request"].(string)
		out = append(out, request)
	}
	return out
}

// handoverFixture is a session that already has work: one task and two turns,
// written by its own model before any monitor existed.
func handoverFixture(t *testing.T, dir string) *handoverSlot {
	t.Helper()
	// The empty role is the already-running session: the control plane put no
	// role on this pod, because when it started there was no pair.
	slot := openHandoverSlot(t, dir, "")
	slot.mustOwn("task_put", map[string]any{
		"id": "task-old", "request_id": "put-old", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-session", "title": "이전부터 하던 일",
			"description": "왜: 전환 전에 이미 등록돼 있었다", "completion_criteria": "원래 기준",
			"status": "running",
		},
	})
	for i, request := range []string{"예전 지시 1", "예전 지시 2"} {
		appended := slot.mustOwn("turn_append", map[string]any{
			"task_id": "task-old", "request_id": fmt.Sprintf("old-turn-%d", i+1),
			"turn": map[string]any{"request": request},
		})
		if got := monitorInt(t, appended["turn"].(map[string]any), "id"); got != int64(i+1) {
			t.Fatalf("turn %d got id %d", i, got)
		}
	}
	return slot
}

func TestHandoverSealsTheRunningSessionOutOfOneLedgerItKeepsReading(t *testing.T) {
	dir := t.TempDir()
	slot := handoverFixture(t, dir)

	// Before the handover the session owns its records, and says so.
	if writer := slot.writerNow(); writer["writer"] != workWriterSession {
		t.Fatalf("a session with no monitor reads as %v, want %q", writer["writer"], workWriterSession)
	}
	// A monitor may not write before the control plane hands the records over.
	if _, code := slot.asMonitor("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "early", "expected_version": 1, "expected_turn_version": 3,
		"turn": map[string]any{"request": "너무 이른 지시"},
	}); code != "permission_denied" {
		t.Fatalf("a monitor wrote before the handover: %q", code)
	}

	// The handover. One call, one transaction.
	status, sealed := slot.seal(workWriterMonitor, "mon-1", nil)
	if status != http.StatusOK || sealed["changed"] != true {
		t.Fatalf("handover answered %d %+v", status, sealed)
	}
	if writer := slot.writerNow(); writer["writer"] != workWriterMonitor ||
		writer["holder"] != "mon-1" || monitorInt(t, writer, "epoch") != 1 {
		t.Fatalf("after the handover the writer is %+v", writer)
	}

	// The session's own model is refused from here on, with no restart and no
	// wait — and the refusal names who holds the records.
	before := slot.turnTexts("task-old")
	decoded, code := slot.own("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "after-seal", "expected_version": 1, "expected_turn_version": 3,
		"turn": map[string]any{"request": "전환 뒤 부모가 쓴 것"},
	})
	if code != "permission_denied" {
		t.Fatalf("the sealed session still wrote: %q %+v", code, decoded)
	}
	if message, _ := decoded["error"].(string); !strings.Contains(message, "mon-1") {
		t.Fatalf("the refusal does not say who holds the records: %q", message)
	}
	if after := slot.turnTexts("task-old"); len(after) != len(before) {
		t.Fatalf("the refused write still landed: %v -> %v", before, after)
	}

	// Its reads are untouched: the history from before the handover is still
	// its own records, read through its own tools.
	if got := slot.turnTexts("task-old"); len(got) != 2 || got[0] != "예전 지시 1" || got[1] != "예전 지시 2" {
		t.Fatalf("the session lost sight of its own history: %v", got)
	}

	// The monitor continues the **same** task in the **same** records. Nothing
	// was copied anywhere: the new turn lands next to the old ones.
	listed := slot.mustOwn("turn_list", map[string]any{"task_id": "task-old"})
	appended := slot.mustAsMonitor("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "mon-turn-1",
		"expected_version":      monitorInt(t, slot.mustOwn("task_get", map[string]any{"id": "task-old"})["task"].(map[string]any), "version"),
		"expected_turn_version": monitorInt(t, listed, "turn_version"),
		"turn":                  map[string]any{"request": "전환 뒤 모니터가 이어서 쓴 것"},
	})
	if got := monitorInt(t, appended["turn"].(map[string]any), "id"); got != 3 {
		t.Fatalf("the monitor's turn got id %d, want the next id of the same task", got)
	}
	if got := slot.turnTexts("task-old"); len(got) != 3 ||
		got[0] != "예전 지시 1" || got[2] != "전환 뒤 모니터가 이어서 쓴 것" {
		t.Fatalf("the ledger is not one history: %v", got)
	}

	// The same handover again is the same handover: no second epoch, so a
	// retried or replayed control-plane call cannot make the records look as if
	// they changed hands twice.
	if _, again := slot.seal(workWriterMonitor, "mon-1", nil); again["changed"] != false {
		t.Fatalf("a repeated handover reported a change: %+v", again)
	}
	if got := monitorInt(t, slot.writerNow(), "epoch"); got != 1 {
		t.Fatalf("a repeated handover moved the epoch to %d", got)
	}

	// A caller acting on a stale epoch is refused rather than overwriting the
	// seal somebody else moved.
	stale := int64(0)
	if status, body := slot.seal(workWriterSession, "mon-1", &stale); status != http.StatusConflict {
		t.Fatalf("a stale-epoch rollback answered %d %+v", status, body)
	}
	if writer := slot.writerNow(); writer["writer"] != workWriterMonitor {
		t.Fatalf("the stale call moved the writer: %+v", writer)
	}

	// A restart of the session's own process changes nothing — the seal is in
	// the records, not in the environment it was started with.
	restarted := openHandoverSlot(t, dir, "")
	if _, code := restarted.own("turn_patch", map[string]any{
		"task_id": "task-old", "turn_id": 1, "request_id": "after-restart",
		"expected_version": 1, "expected_turn_version": 4,
		"turn": map[string]any{"response": "재시작 뒤 부모가 쓴 것"},
	}); code != "permission_denied" {
		t.Fatalf("a restarted session wrote its sealed records: %q", code)
	}
}

func TestRollbackClosesTheMonitorBeforeTheSessionWritesAgain(t *testing.T) {
	dir := t.TempDir()
	slot := handoverFixture(t, dir)
	if status, _ := slot.seal(workWriterMonitor, "mon-1", nil); status != http.StatusOK {
		t.Fatalf("handover answered %d", status)
	}

	// Another pair's monitor cannot hand these records back: the rollback has
	// to name the monitor that holds them.
	if status, body := slot.seal(workWriterSession, "mon-other", nil); status != http.StatusConflict {
		t.Fatalf("a rollback by the wrong holder answered %d %+v", status, body)
	}
	if writer := slot.writerNow(); writer["holder"] != "mon-1" {
		t.Fatalf("the wrong-holder rollback moved the seal: %+v", writer)
	}

	// The rollback. When it answers, the monitor is already closed — the next
	// writer is not opened while the previous one can still write, because both
	// checks are the same row in the same database.
	epoch := int64(1)
	status, rolled := slot.seal(workWriterSession, "mon-1", &epoch)
	if status != http.StatusOK || rolled["changed"] != true {
		t.Fatalf("rollback answered %d %+v", status, rolled)
	}
	// holder is absent once the records are the session's own again: there is
	// no monitor holding them.
	if writer := slot.writerNow(); writer["writer"] != workWriterSession ||
		writer["holder"] != nil || monitorInt(t, writer, "epoch") != 2 {
		t.Fatalf("after the rollback the writer is %+v", writer)
	}
	if _, code := slot.asMonitor("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "after-rollback", "expected_version": 1,
		"expected_turn_version": 3, "turn": map[string]any{"request": "롤백 뒤 모니터가 쓴 것"},
	}); code != "permission_denied" {
		t.Fatalf("the monitor still wrote after the rollback: %q", code)
	}

	// And the session has its records back, with every turn from both sides of
	// the handover still in place.
	listed := slot.mustOwn("turn_list", map[string]any{"task_id": "task-old"})
	slot.mustOwn("turn_append", map[string]any{
		"task_id": "task-old", "request_id": "own-again",
		"expected_turn_version": monitorInt(t, listed, "turn_version"),
		"turn":                  map[string]any{"request": "롤백 뒤 부모가 쓴 것"},
	})
	if got := slot.turnTexts("task-old"); len(got) != 3 ||
		got[0] != "예전 지시 1" || got[2] != "롤백 뒤 부모가 쓴 것" {
		t.Fatalf("history did not survive the round trip: %v", got)
	}
}

func TestInstructionStoredWhileTheMonitorIsAwayIsDrainedOnceAfterRestart(t *testing.T) {
	dir := t.TempDir()
	slot := handoverFixture(t, dir)
	if status, _ := slot.seal(workWriterMonitor, "mon-1", nil); status != http.StatusOK {
		t.Fatalf("handover answered %d", status)
	}

	// A new instruction arrives while the monitor is frozen. It is stored where
	// the work is — as the next turn of the task — by the route the control
	// plane already uses, which wakes nothing and needs no monitor at all.
	request := map[string]any{
		"id": "req-frozen-1", "task_id": "task-old",
		"text": "동결 중에 사용자가 보낸 새 지시", "author": "parent-session",
	}
	status, stored := slot.post(sessionDBWorkRequestsPath, request)
	if status != http.StatusCreated {
		t.Fatalf("the instruction was not stored: %d %+v", status, stored)
	}
	// The same instruction again — a retry, a reconnect — is the same turn.
	status, replayed := slot.post(sessionDBWorkRequestsPath, request)
	if status != http.StatusOK || replayed["replayed"] != true {
		t.Fatalf("a resend of the same id was not replayed: %d %+v", status, replayed)
	}
	if got := slot.turnTexts("task-old"); len(got) != 3 {
		t.Fatalf("the resend made a second turn: %v", got)
	}

	// The monitor comes back — a new process over the same records — and drains
	// it: it finds the open turn, takes it, records the outside call, and
	// writes the result. All of it through the control plane's route.
	resumed := openHandoverSlot(t, dir, "")
	listed := resumed.mustAsMonitor("turn_list", map[string]any{"task_id": "task-old", "open": true})
	// Every unanswered turn is the monitor's queue; the held instruction is the
	// one that arrived while it was away, and it is in there exactly once.
	held := []map[string]any{}
	for _, row := range monitorRows(listed["turns"]) {
		if row["request"] == request["text"] {
			held = append(held, row)
		}
	}
	if len(held) != 1 {
		t.Fatalf("the held instruction appears %d times among the open turns", len(held))
	}
	turnID := monitorInt(t, held[0], "id")
	version := monitorInt(t, resumed.mustAsMonitor("task_get",
		map[string]any{"id": "task-old"})["task"].(map[string]any), "version")
	taken := resumed.mustAsMonitor("turn_patch", map[string]any{
		"task_id": "task-old", "turn_id": turnID, "request_id": "drain-claim",
		"expected_version": version, "expected_turn_version": monitorInt(t, listed, "turn_version"),
		"turn": map[string]any{"started_at": "2026-11-24T02:00:00Z"},
	})
	token, _ := taken["claim_token"].(string)
	if token == "" {
		t.Fatalf("taking the held turn minted no claim token: %+v", taken)
	}
	dispatched := resumed.mustAsMonitor("turn_dispatch", map[string]any{
		"task_id": "task-old", "turn_id": turnID, "request_id": "drain-dispatch",
		"expected_version": version, "expected_turn_version": monitorInt(t, taken, "turn_version"),
		"claim_token": token, "operation_key": "op-req-frozen-1",
		"operation_target": "worker-session", "dispatch_state": dispatchDispatched,
	})

	// One instruction, one outside call: a second key for the same turn is
	// refused, so a drain that runs twice cannot delegate twice.
	if _, code := resumed.asMonitor("turn_dispatch", map[string]any{
		"task_id": "task-old", "turn_id": turnID, "request_id": "drain-dispatch-again",
		"expected_version": version, "expected_turn_version": monitorInt(t, dispatched, "turn_version"),
		"claim_token": token, "operation_key": "op-req-frozen-1-second",
		"operation_target": "worker-session", "dispatch_state": dispatchIntended,
	}); code != "conflict" {
		t.Fatalf("a second operation key for the same instruction answered %q", code)
	}

	// The result is recorded on that same turn, and the session reads it with
	// its own tools — the history it had before the handover and this, together.
	resumed.mustAsMonitor("turn_patch", map[string]any{
		"task_id": "task-old", "turn_id": turnID, "request_id": "drain-response",
		"expected_version": version, "expected_turn_version": monitorInt(t, dispatched, "turn_version"),
		"turn": map[string]any{
			"response": "동결 중 들어온 지시를 재개 후 처리했습니다", "ended_at": "2026-11-24T02:05:00Z",
			"session_seq": 7,
		},
	})
	rows := monitorRows(resumed.mustOwn("turn_list", map[string]any{"task_id": "task-old"})["turns"])
	if len(rows) != 3 {
		t.Fatalf("turns = %d, want the two from before plus the drained one", len(rows))
	}
	drained := rows[2]
	if drained["response"] != "동결 중 들어온 지시를 재개 후 처리했습니다" ||
		monitorInt(t, drained, "session_seq") != 7 || drained["ended_at"] == nil {
		t.Fatalf("the drained turn did not keep its result: %+v", drained)
	}
	for _, row := range monitorRows(resumed.mustOwn("turn_list",
		map[string]any{"task_id": "task-old"})["dispatches"]) {
		if monitorInt(t, row, "turn_id") == turnID && row["operation_key"] != "op-req-frozen-1" {
			t.Fatalf("the drained turn records key %v", row["operation_key"])
		}
	}
}
