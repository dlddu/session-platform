package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// One flow through the product's own work surface: a real SQLite slot database,
// the real MCP tools/call entry (`callWork`, which validates a caller's
// arguments the way a caller's JSON is validated), and an external work session
// behind HTTP.
//
// What is product code here: everything under `callWork` — the task and turn
// records, the compare-and-swap on both lanes, the claim token minted by taking
// a turn, the dispatch bookkeeping and its refusals, and the role gate that
// stops a paired parent. Those rules are the product's, not this file's.
//
// What this file is: the **driver** that makes the calls the monitor's role
// skill makes, in the order that skill states (plugin-marketplace
// `monitor-pair-runtime` §2–3): reconcile → claim → intended → one real call →
// dispatched → read the work session's own words at that seq → record them.
// Each step is a single existing tool call whose contract the product enforces;
// the driver adds no behaviour the product lacks, and it fails if a step's
// contract is not met.
//
// What is faked: only the boundary. The work session is an httptest server,
// because `session_write`/`session_read` reach it through the K3s MCP and the
// cluster is not part of an isolated fixture. Nothing about the records is
// faked and no result is injected: the final text asserted below is the text
// the fake work session produced and the product read back and stored.
//
// The parent's first read after the monitor freezes is the control plane's half
// of the same flow and is covered there
// (control-plane/internal/api/monitor_test.go).

// fakeWorker is the external work session. It counts writes, so "delegated
// exactly once" is measured at the boundary that would really run the work.
type fakeWorker struct {
	mu         sync.Mutex
	writes     []string
	transcript string
	seq        int
	final      string
}

func (w *fakeWorker) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /write", func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			Payload string `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.writes = append(w.writes, body.Payload)
		w.seq++
		// The framing the platform delimits a session's turns with.
		w.transcript += fmt.Sprintf("\x1esession-platform input seq=%d\n%s\n"+
			"\x1esession-platform output seq=%d\n%s\n"+
			"\x1esession-platform end seq=%d status=ok\n",
			w.seq, body.Payload, w.seq, w.final, w.seq)
		_, _ = rw.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /read", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		_ = json.NewEncoder(rw).Encode(map[string]any{"payload": w.transcript})
	})
	return mux
}

func (w *fakeWorker) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

// workerClient is how the monitor reaches the work session: the two calls the
// K3s MCP exposes as session_write and session_read.
type workerClient struct {
	base string
	t    *testing.T
}

func (c workerClient) write(payload string) {
	c.t.Helper()
	raw, _ := json.Marshal(map[string]any{"payload": payload})
	resp, err := http.Post(c.base+"/write", "application/json", bytes.NewReader(raw))
	if err != nil {
		c.t.Fatalf("session_write: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("session_write status %d", resp.StatusCode)
	}
}

func (c workerClient) read() string {
	c.t.Helper()
	resp, err := http.Post(c.base+"/read", "application/json", strings.NewReader("{}"))
	if err != nil {
		c.t.Fatalf("session_read: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Payload string `json:"payload"`
	}
	if json.Unmarshal(body, &out) != nil {
		c.t.Fatalf("session_read reply: %s", body)
	}
	return out.Payload
}

// workerFrame is one delimited block of a session's transcript.
type workerFrame struct {
	kind string
	seq  int
	body string
}

func workerFramesOf(transcript string) []workerFrame {
	frames := []workerFrame{}
	for _, part := range strings.Split(transcript, "\x1e") {
		head, body, _ := strings.Cut(part, "\n")
		fields := strings.Fields(head)
		if len(fields) < 3 || fields[0] != "session-platform" {
			continue
		}
		seq, err := strconv.Atoi(strings.TrimPrefix(fields[2], "seq="))
		if err != nil {
			continue
		}
		frames = append(frames, workerFrame{kind: fields[1], seq: seq, body: strings.TrimSuffix(body, "\n")})
	}
	return frames
}

// seqOfInstruction is the comparison the skill makes when a dispatch's reply
// was lost: does the work session's own transcript already carry this
// instruction, and at which seq. It establishes "already accepted" from the
// outside world rather than from memory.
func (c workerClient) seqOfInstruction(instruction string) (int, bool) {
	for _, frame := range workerFramesOf(c.read()) {
		if frame.kind == "input" && frame.body == instruction {
			return frame.seq, true
		}
	}
	return 0, false
}

// finalAt is the work session's own words for one seq, and only once that seq
// is closed by its end marker. An open turn answers false, which is what keeps
// unfinished work from being written down as finished.
func (c workerClient) finalAt(seq int) (string, bool) {
	output, closed := "", false
	for _, frame := range workerFramesOf(c.read()) {
		if frame.seq != seq {
			continue
		}
		switch frame.kind {
		case "output":
			output = frame.body
		case "end":
			closed = true
		}
	}
	return output, closed && output != ""
}

// monitorDriver makes the role skill's calls against the product's work
// surface. Every decision is read back out of the records first, so calling run
// again — after a lost reply, after a restart — is the same code path.
type monitorDriver struct {
	t      *testing.T
	db     *sessionDB
	worker workerClient
	taskID string
	// stopAfter cuts a run short at a named step, which is how a lost reply and
	// a crash mid-flight are reproduced without special-casing the driver.
	stopAfter string
	// attempts numbers this driver's tool calls. A tool call's request_id is the
	// receipt for that one call, and a retry writes against versions that have
	// moved since, so reusing it would be a conflicting receipt rather than
	// idempotence. What makes the delegation idempotent is operation_key, which
	// stays stable across attempts.
	attempts int
}

func (d *monitorDriver) rid(requestID, step string) string {
	d.attempts++
	return fmt.Sprintf("%s-%s-%d", requestID, step, d.attempts)
}

func (d *monitorDriver) must(name string, args map[string]any) map[string]any {
	d.t.Helper()
	return mustWork(d.t, d.db, name, args)
}

// taskVersion is the task's compare-and-swap value, which every turn write
// needs. Writing a turn moves the task's updated_at and leaves this alone.
func (d *monitorDriver) taskVersion() int64 {
	d.t.Helper()
	got := d.must("task_get", map[string]any{"id": d.taskID})
	task, ok := got["task"].(map[string]any)
	if !ok {
		d.t.Fatalf("task_get carried no task: %+v", got)
	}
	return monitorInt(d.t, task, "version")
}

// monitorLedger is what the monitor knows before it acts: the turn carrying
// this instruction, the turn lane's counter to write against, and what was
// already recorded as having gone outside for it.
type monitorLedger struct {
	turnID        int64
	turnVersion   int64
	startedAt     string
	response      string
	operationKey  string
	dispatchState string
}

// readLedger is the skill's first step: ask the records what already happened.
// It is a read, and it is what makes the duplicate send and the restart decide
// correctly — possible only because turn_list answers the dispatch bookkeeping
// beside the turns.
func (d *monitorDriver) readLedger(instruction string) monitorLedger {
	t := d.t
	t.Helper()
	listed := d.must("turn_list", map[string]any{"task_id": d.taskID})
	at := monitorLedger{turnVersion: monitorInt(t, listed, "turn_version")}
	for _, row := range monitorRows(listed["turns"]) {
		if row["request"] != instruction {
			continue
		}
		at.turnID = monitorInt(t, row, "id")
		at.startedAt, _ = row["started_at"].(string)
		at.response, _ = row["response"].(string)
	}
	for _, row := range monitorRows(listed["dispatches"]) {
		if monitorInt(t, row, "turn_id") != at.turnID {
			continue
		}
		at.operationKey, _ = row["operation_key"].(string)
		at.dispatchState, _ = row["dispatch_state"].(string)
	}
	return at
}

// claim takes the turn and returns the token turn_dispatch requires. A turn an
// earlier run left taken is handed back first: the token is minted on taking,
// and the one that run held went down with it.
func (d *monitorDriver) claim(requestID string, at monitorLedger) (string, int64) {
	t := d.t
	t.Helper()
	version, turnVersion := d.taskVersion(), at.turnVersion
	if at.startedAt != "" {
		handed := d.must("turn_patch", map[string]any{
			"task_id": d.taskID, "turn_id": at.turnID, "request_id": d.rid(requestID, "handback"),
			"expected_version": version, "expected_turn_version": turnVersion,
			"turn": map[string]any{"started_at": nil},
		})
		turnVersion = monitorInt(t, handed, "turn_version")
	}
	taken := d.must("turn_patch", map[string]any{
		"task_id": d.taskID, "turn_id": at.turnID, "request_id": d.rid(requestID, "claim"),
		"expected_version": version, "expected_turn_version": turnVersion,
		"turn": map[string]any{"started_at": "2026-11-23T21:00:00Z"},
	})
	token, _ := taken["claim_token"].(string)
	if token == "" {
		t.Fatalf("taking the turn minted no claim token: %+v", taken)
	}
	return token, monitorInt(t, taken, "turn_version")
}

func (d *monitorDriver) dispatch(at monitorLedger, requestID, token, key, state string) int64 {
	d.t.Helper()
	recorded := d.must("turn_dispatch", map[string]any{
		"task_id": d.taskID, "turn_id": at.turnID, "request_id": requestID,
		"expected_version": d.taskVersion(), "expected_turn_version": at.turnVersion,
		"claim_token": token, "operation_key": key,
		"operation_target": "worker-session", "dispatch_state": state,
	})
	return monitorInt(d.t, recorded, "turn_version")
}

// run is one drain of one instruction, in the order monitor-pair-runtime
// states. It is safe to call again at any point it was cut short.
func (d *monitorDriver) run(requestID, instruction, operationKey string) {
	t := d.t
	t.Helper()

	at := d.readLedger(instruction)
	if at.turnID == 0 {
		appended := d.must("turn_append", map[string]any{
			"task_id": d.taskID, "request_id": d.rid(requestID, "append"),
			"expected_version": d.taskVersion(), "expected_turn_version": at.turnVersion,
			"turn": map[string]any{"request": instruction},
		})
		at.turnID = monitorInt(t, appended["turn"].(map[string]any), "id")
		at.turnVersion = monitorInt(t, appended, "turn_version")
	}
	if at.response != "" {
		return // an earlier run recorded the result; this request is finished
	}

	// A recorded `dispatched` means it already left. Anything short of that is
	// re-established against the work session itself, never assumed.
	if at.dispatchState != dispatchDispatched {
		token, turnVersion := d.claim(requestID, at)
		at.turnVersion = turnVersion
		at.turnVersion = d.dispatch(at, d.rid(requestID, "intended"), token, operationKey, dispatchIntended)
		if d.stopAfter == "intended" {
			return
		}

		// The one real call — and only if the work session does not already
		// carry this instruction. That comparison is what a lost acceptance
		// reply costs: a re-read, not a second delegation.
		if _, accepted := d.worker.seqOfInstruction(instruction); !accepted {
			d.worker.write(instruction)
		}
		if d.stopAfter == "write" {
			return
		}
		at.turnVersion = d.dispatch(at, d.rid(requestID, "dispatched"), token, operationKey, dispatchDispatched)
		if d.stopAfter == "dispatched" {
			return
		}
	}

	// The work session's own words at the seq this instruction went in at.
	seq, accepted := d.worker.seqOfInstruction(instruction)
	if !accepted {
		t.Fatalf("recorded as dispatched but the work session never took %q", instruction)
	}
	final, closed := d.worker.finalAt(seq)
	if !closed {
		return // still open; the next run continues from the same cursor
	}
	d.must("turn_patch", map[string]any{
		"task_id": d.taskID, "turn_id": at.turnID, "request_id": d.rid(requestID, "response"),
		"expected_version": d.taskVersion(), "expected_turn_version": at.turnVersion,
		"turn": map[string]any{
			"response": final, "ended_at": "2026-11-23T21:05:00Z", "session_seq": seq,
		},
	})
}

func monitorRows(v any) []map[string]any {
	raw, _ := v.([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, row := range raw {
		if fields, ok := row.(map[string]any); ok {
			out = append(out, fields)
		}
	}
	return out
}

func monitorInt(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			t.Fatalf("%s is not a whole number in %+v", key, m)
		}
		return n
	}
	t.Fatalf("%s missing in %+v", key, m)
	return 0
}

// monitorFlowFixture opens a real slot database for a monitor-role session.
// Calling it again over the same directory is a restart: a new handle, the same
// records, no memory of the run before it.
func monitorFlowFixture(t *testing.T, dir string, worker workerClient) *monitorDriver {
	t.Helper()
	db, err := newSessionDB(dir, false, "monitor", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &monitorDriver{t: t, db: db, worker: worker, taskID: "task-e2e"}
}

func TestMonitorFlowDelegatesOnceAndStoresTheWorkSessionsOwnWords(t *testing.T) {
	const workersOwnWords = "리뷰 코멘트 네 건 반영하고 CI 통과시켰습니다. (고유응답-7f3a)"
	worker := &fakeWorker{final: workersOwnWords}
	srv := httptest.NewServer(worker.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	d := monitorFlowFixture(t, dir, workerClient{base: srv.URL, t: t})
	d.must("task_put", map[string]any{
		"id": d.taskID, "request_id": "put-1", "expected_version": 0,
		"task": map[string]any{
			"session_id": "worker-session", "title": "리뷰 반영",
			"description": "왜: 사용자가 코멘트 반영을 요청했다", "completion_criteria": "CI 통과",
			"status": "planned",
		},
	})

	// 1. The instruction the person sent, drained once.
	const first = "코멘트 전부 반영해 줘"
	d.run("req-app-7", first, "op-req-app-7")
	if got := worker.writeCount(); got != 1 {
		t.Fatalf("writes after the first drain = %d, want 1", got)
	}

	// 2. The same instruction under the same stable key, sent again. The
	//    records say it was dispatched and answered, so nothing goes out.
	d.run("req-app-7", first, "op-req-app-7")
	if got := worker.writeCount(); got != 1 {
		t.Fatalf("a duplicate stable-key send delegated again: writes = %d", got)
	}

	// 3. A lost acceptance reply: the call left but `dispatched` was never
	//    recorded, and the process died there. A restart must neither call
	//    again nor lose the result.
	const second = "통합 테스트도 하나 추가해 줘"
	d.stopAfter = "write"
	d.run("req-app-8", second, "op-req-app-8")
	if got := worker.writeCount(); got != 2 {
		t.Fatalf("writes before the lost reply = %d, want 2", got)
	}
	restarted := monitorFlowFixture(t, dir, d.worker)
	restarted.run("req-app-8", second, "op-req-app-8")
	if got := worker.writeCount(); got != 2 {
		t.Fatalf("a restart after a lost acceptance reply delegated again: writes = %d", got)
	}

	// 4. A crash after `dispatched` and before the result was recorded: the
	//    next run collects the result and does not call again.
	const third = "릴리스 노트 초안도 써 줘"
	restarted.stopAfter = "dispatched"
	restarted.run("req-app-9", third, "op-req-app-9")
	again := monitorFlowFixture(t, dir, d.worker)
	again.run("req-app-9", third, "op-req-app-9")
	if got := worker.writeCount(); got != 3 {
		t.Fatalf("writes for three instructions = %d, want one each", got)
	}

	// What the records hold is the work session's own words, verbatim, at the
	// seq they came back on, with the dispatch that produced them.
	listed := mustWork(t, again.db, "turn_list", map[string]any{"task_id": again.taskID})
	turns := monitorRows(listed["turns"])
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want one per instruction", len(turns))
	}
	keys, states := map[int64]string{}, map[int64]string{}
	for _, row := range monitorRows(listed["dispatches"]) {
		id := monitorInt(t, row, "turn_id")
		keys[id], _ = row["operation_key"].(string)
		states[id], _ = row["dispatch_state"].(string)
	}
	wantKey := map[string]string{first: "op-req-app-7", second: "op-req-app-8", third: "op-req-app-9"}
	for i, row := range turns {
		request, _ := row["request"].(string)
		if row["response"] != workersOwnWords {
			t.Fatalf("turn %d stored %v, want the work session's own text %q", i, row["response"], workersOwnWords)
		}
		if got := monitorInt(t, row, "session_seq"); got != int64(i+1) {
			t.Fatalf("turn %d stored session_seq %d, want %d", i, got, i+1)
		}
		if row["started_at"] == nil || row["ended_at"] == nil {
			t.Fatalf("turn %d kept no times: %+v", i, row)
		}
		id := monitorInt(t, row, "id")
		if states[id] != dispatchDispatched {
			t.Fatalf("turn %d dispatch_state = %q, want dispatched", i, states[id])
		}
		if keys[id] != wantKey[request] {
			t.Fatalf("turn %d operation_key = %q, want %q", i, keys[id], wantKey[request])
		}
	}
	if len(worker.writes) != 3 {
		t.Fatalf("the work session was written %d times: %q", len(worker.writes), worker.writes)
	}
}

// A paired parent cannot run any of it. The calls the monitor just made are
// refused for the parent role before the transaction opens, so the
// single-writer boundary is the product's and not this driver's discipline.
func TestParentRoleCannotRunTheMonitorFlow(t *testing.T) {
	db, err := newSessionDB(t.TempDir(), false, pairRoleParent, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	calls := []struct {
		name string
		args map[string]any
	}{
		{"task_put", map[string]any{"id": "task-e2e", "request_id": "x1", "expected_version": 0,
			"task": map[string]any{"session_id": nil, "title": "t", "description": "d",
				"completion_criteria": "c", "status": "planned"}}},
		{"turn_append", map[string]any{"task_id": "task-e2e", "request_id": "x2",
			"expected_version": 0, "expected_turn_version": 1,
			"turn": map[string]any{"request": "do it"}}},
		{"turn_patch", map[string]any{"task_id": "task-e2e", "turn_id": 1, "request_id": "x3",
			"expected_version": 0, "expected_turn_version": 1,
			"turn": map[string]any{"started_at": "2026-11-23T21:00:00Z"}}},
		{"turn_dispatch", map[string]any{"task_id": "task-e2e", "turn_id": 1, "request_id": "x4",
			"expected_version": 0, "expected_turn_version": 1, "claim_token": "t",
			"operation_key": "k", "operation_target": "worker-session",
			"dispatch_state": dispatchIntended}},
	}
	for _, call := range calls {
		if _, code := workCall(t, db, call.name, call.args); code != "permission_denied" {
			t.Fatalf("%s for a paired parent answered %q, want permission_denied", call.name, code)
		}
	}
}
