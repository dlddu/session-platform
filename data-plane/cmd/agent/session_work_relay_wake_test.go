package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubLane is a monitor lane as the sidecar's hop sees it: it takes prompts and
// answers about the receipts it issued. Both halves are real HTTP on the real
// paths, so what is stubbed is the conversation behind them and nothing else.
type stubLane struct {
	mu      sync.Mutex
	prompts []string
	// refusal, when set, is the status the write answers with instead of
	// taking the prompt.
	refusal int
	// ticketState is what /turn says about any receipt. The default is this
	// run still holding it; turnTicketUnknownRun is the lane having restarted.
	ticketState string
	issued      int
}

func (l *stubLane) took() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.prompts...)
}

func (l *stubLane) set(mutate func(*stubLane)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	mutate(l)
}

// newWakeTestSlot is one slot's database behind the real sidecar routes, with a
// stub monitor lane wired to the real hop. The returned server is the sidecar:
// every request in these tests goes through it rather than calling the store
// path directly, because the route is what the control plane actually talks to.
func newWakeTestSlot(t *testing.T) (*sessionDB, *httptest.Server, *stubLane) {
	t.Helper()
	db, err := newSessionDB(t.TempDir(), false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sidecar := httptest.NewServer(withSessionDBMCP(http.NotFoundHandler(), db))
	t.Cleanup(sidecar.Close)

	lane := &stubLane{ticketState: turnTicketQueued}
	mux := http.NewServeMux()
	mux.HandleFunc(managerLanePathPrefix+"/write", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lane.mu.Lock()
		defer lane.mu.Unlock()
		if lane.refusal != 0 {
			http.Error(w, "the lane is not taking prompts", lane.refusal)
			return
		}
		lane.prompts = append(lane.prompts, string(body))
		lane.issued++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "queued", "ticket": fmt.Sprintf("run1-%d", lane.issued),
		})
	})
	mux.HandleFunc(managerLanePathPrefix+agentTurnPath, func(w http.ResponseWriter, r *http.Request) {
		lane.mu.Lock()
		state := lane.ticketState
		lane.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ticket": r.URL.Query().Get("ticket"), "state": state,
		})
	})
	laneSrv := httptest.NewServer(mux)
	t.Cleanup(laneSrv.Close)

	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1" + managerLanePathPrefix + "/write")
	db.monitorLane.writeURL = laneSrv.URL + managerLanePathPrefix + "/write"
	db.monitorLane.turnURL = laneSrv.URL + managerLanePathPrefix + agentTurnPath
	return db, sidecar, lane
}

// wakeTestTask puts one task in one status, as the monitor lane would.
func wakeTestTask(t *testing.T, db *sessionDB, id, status string) {
	t.Helper()
	payload, refusal := laneTool(t, db, laneCaller(laneMonitor, "monitor-run-1"), "task_put", map[string]any{
		"id": id, "request_id": "put-" + id, "expected_version": 0,
		"task": map[string]any{"session_id": nil, "title": "decide " + id,
			"description": "a task " + id, "completion_criteria": "decided", "status": status},
	})
	if refusal != "" {
		t.Fatalf("task_put %s: %s", id, refusal)
	}
	if failure, _ := payload["error"].(string); failure != "" {
		t.Fatalf("task_put %s: %s", id, failure)
	}
}

// storeRequestOverHTTP is one app request as the control plane sends it.
func storeRequestOverHTTP(
	t *testing.T, sidecar *httptest.Server, body map[string]any,
) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(sidecar.URL+sessionDBWorkRequestsPath, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var answer map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &answer)
	}
	return resp.StatusCode, answer
}

func wakeRelayOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	relay, ok := answer["relay"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carried no handover: %v", answer)
	}
	return relay
}

func wakeStoredTurnID(t *testing.T, answer map[string]any) int64 {
	t.Helper()
	turn, ok := answer["turn"].(map[string]any)
	if !ok {
		t.Fatalf("the answer carried no turn: %v", answer)
	}
	id, ok := turn["id"].(float64)
	if !ok {
		t.Fatalf("the stored turn has no id: %v", turn)
	}
	return int64(id)
}

// wakeIntentRow reads one handover row straight out of the slot, which is how
// these tests assert on durability rather than on what a call happened to
// return.
func wakeIntentRow(t *testing.T, db *sessionDB, turnID int64) (workWakeIntent, bool) {
	t.Helper()
	handle, err := db.handle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var intent workWakeIntent
	found := false
	if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
		var readErr error
		intent, found, readErr = readWakeIntent(context.Background(), tx, turnID)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	return intent, found
}

// wakeForceRow rewrites one handover row, standing in for the states a crash
// leaves behind. It writes the row directly on purpose: the point of each of
// these tests is what happens to a row the normal path did not finish writing.
func wakeForceRow(t *testing.T, db *sessionDB, turnID int64, state, ticket, claimedAt string) {
	t.Helper()
	handle, err := db.handle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
		_, execErr := tx.ExecContext(context.Background(),
			"UPDATE sp_work_request_wake SET state=?,ticket=?,claimed_at=? WHERE turn_id=?",
			state, ticket, claimedAt, turnID)
		return execErr
	}); err != nil {
		t.Fatal(err)
	}
}

// wakeTakeTurn is the monitor taking one turn, the way its standing round does:
// read the counters the task carries, then write started_at against them. It
// asserts the write landed, because a silently refused one would make a test
// about "the monitor already has this" prove nothing.
func wakeTakeTurn(t *testing.T, db *sessionDB, taskID string, turnID int64, requestID string) {
	t.Helper()
	monitor := laneCaller(laneMonitor, "monitor-run-1")
	task, refusal := laneTool(t, db, monitor, "task_get", map[string]any{"id": taskID})
	if refusal != "" {
		t.Fatalf("task_get %s: %s", taskID, refusal)
	}
	record, _ := task["task"].(map[string]any)
	version, _ := record["version"].(float64)
	turnVersion, _ := task["turn_version"].(float64)
	patched, refusal := laneTool(t, db, monitor, "turn_patch", map[string]any{
		"task_id": taskID, "turn_id": turnID, "request_id": requestID,
		"expected_version": int64(version), "expected_turn_version": int64(turnVersion),
		"turn": map[string]any{"started_at": "2026-11-28T00:00:00Z"},
	})
	if refusal != "" {
		t.Fatalf("turn_patch %s/%d: %s", taskID, turnID, refusal)
	}
	if failure, _ := patched["error"].(string); failure != "" {
		t.Fatalf("turn_patch %s/%d: %s", taskID, turnID, failure)
	}
}

// TestARequestOnATaskWaitingForAPersonReachesTheMonitorLaneAtOnce is the normal
// path end to end with nothing mocked between the pieces: a real lane pair
// behind one agent port, the real session database behind the real sidecar, and
// the real app request route.
//
// The monitor lane had stopped looking at this task because it was waiting on
// the person. The request tells it, in the same moment the words land, and what
// it is told is the two ids — which is enough for it to read the words and take
// the turn.
func TestARequestOnATaskWaitingForAPersonReachesTheMonitorLaneAtOnce(t *testing.T) {
	db, err := newSessionDB(t.TempDir(), false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sidecar := httptest.NewServer(withSessionDBMCP(http.NotFoundHandler(), db))
	t.Cleanup(sidecar.Close)

	monitorMCP, err := sessionDBLaneMCPURL(sidecar.URL+sessionDBMCPPath, laneMonitor, "monitor-run-1")
	if err != nil {
		t.Fatal(err)
	}
	wakeTestTask(t, db, "permission", "review")

	const theWords = "읽기 권한을 더해 주세요 — contents:read 한 레포만."
	woken := make(chan string, 4)
	runner := &lanePairRunner{
		onMonitor: func(prompt string, opts runnerOptions) {
			woken <- prompt
			// What the monitor does with it: read the two ids out of the
			// records and take the turn. This is the consumption side of the
			// handover, and it is the proof that the ids are usable.
			page := mcpCall(t, monitorMCP, "turn_list", map[string]any{"task_id": "permission", "open": true})
			turns, _ := page["turns"].([]any)
			if len(turns) != 1 {
				t.Errorf("the woken monitor found %d open turns, want 1", len(turns))
				return
			}
			turn, _ := turns[0].(map[string]any)
			if request, _ := turn["request"].(string); request != theWords {
				t.Errorf("the open turn's words = %q, want the person's own", request)
			}
			task := mcpCall(t, monitorMCP, "task_get", map[string]any{"id": "permission"})
			record, _ := task["task"].(map[string]any)
			version, _ := record["version"].(float64)
			turnVersion, _ := task["turn_version"].(float64)
			taken := mcpCall(t, monitorMCP, "turn_patch", map[string]any{
				"task_id": "permission", "turn_id": turn["id"], "request_id": "monitor-take-1",
				"expected_version": int64(version), "expected_turn_version": int64(turnVersion),
				"turn": map[string]any{"started_at": "2026-11-28T00:00:00Z"},
			})
			if failure, _ := taken["error"].(string); failure != "" {
				t.Errorf("the woken monitor could not take the turn: %s", failure)
			}
			_, _ = io.WriteString(opts.Stdout, "took the person's answer on task permission\n")
		},
	}
	_, monitor, srv := newLanePairTestServer(t, runner)
	runner.monitorDir = monitor.stateDir
	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1" + managerLanePathPrefix + "/write")
	db.monitorLane.writeURL = srv.URL + managerLanePathPrefix + "/write"
	db.monitorLane.turnURL = srv.URL + managerLanePathPrefix + agentTurnPath

	status, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": theWords, "author": "user-1",
	})
	if status != http.StatusCreated {
		t.Fatalf("storing the request = %d: %v", status, answer)
	}
	turnID := wakeStoredTurnID(t, answer)
	if relay := wakeRelayOf(t, answer); relay["state"] != workRelayAccepted {
		t.Fatalf("the handover = %v, want %s", relay, workRelayAccepted)
	}

	var prompt string
	select {
	case prompt = <-woken:
	case <-time.After(20 * time.Second):
		t.Fatal("the monitor lane was never told a person had answered")
	}
	for _, want := range []string{
		"task_id=permission",
		fmt.Sprintf("turn_id=%d", turnID),
		"delivery=" + wakeDeliveryID(turnID),
		"attempt=1",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the delivery does not name %s:\n%s", want, prompt)
		}
	}
	// The words are not in the delivery. The record is the one copy, and a
	// second one in a prompt can disagree with it.
	if strings.Contains(prompt, theWords) {
		t.Errorf("the delivery quoted the person's words:\n%s", prompt)
	}
	// Nothing was sent into the person's own conversation: the only prompt
	// either lane was given in this test is the one the monitor got.
	runner.mu.Lock()
	seen := append([]string(nil), runner.seen...)
	runner.mu.Unlock()
	if len(seen) != 1 {
		t.Errorf("the lane pair was given %d prompts, want only the monitor's: %v", len(seen), seen)
	}
}

// TestNothingIsHandedOverForARequestNobodyWasWaitingOn: a request on a task that
// is simply running is an ordinary exchange. Waking the monitor for one would be
// the nagging the monitor's own policy exists to avoid, and there is nothing to
// unblock.
func TestNothingIsHandedOverForARequestNobodyWasWaitingOn(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	for _, status := range []string{"planned", "running", "completed", "unknown"} {
		wakeTestTask(t, db, "task-"+status, status)
		code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
			"id": "req-" + status, "task_id": "task-" + status, "text": "한 마디 더", "author": "user-1",
		})
		if code != http.StatusCreated {
			t.Fatalf("%s: storing = %d: %v", status, code, answer)
		}
		if _, carried := answer["relay"]; carried {
			t.Errorf("%s: the answer claimed a handover: %v", status, answer)
		}
		if _, found := wakeIntentRow(t, db, wakeStoredTurnID(t, answer)); found {
			t.Errorf("%s: a handover row was recorded for a task nobody was waiting on", status)
		}
	}
	if took := lane.took(); len(took) != 0 {
		t.Errorf("the monitor lane was given %d prompts, want 0: %v", len(took), took)
	}
}

// TestNothingIsHandedOverByASingleLaneManager: a manager with one lane has
// nobody to tell. The request is not handed to the parent instead — that lane is
// the person's conversation, and putting the platform's words into it would be
// speaking to somebody who had not asked.
func TestNothingIsHandedOverByASingleLaneManager(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	db.monitorLane = nil
	wakeTestTask(t, db, "permission", "review")
	code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "req-1", "task_id": "permission", "text": "네, 그렇게 해 주세요", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, answer)
	}
	if _, carried := answer["relay"]; carried {
		t.Errorf("an unpaired manager claimed a handover: %v", answer)
	}
	if _, found := wakeIntentRow(t, db, wakeStoredTurnID(t, answer)); found {
		t.Error("an unpaired manager recorded a handover it could never make")
	}
	if took := lane.took(); len(took) != 0 {
		t.Errorf("a lane that is not this pod's was given %v", took)
	}
}

// TestAStoreThatFailedHandsNothingOver covers the three refusals that must cost
// zero prompts: a task this slot does not have, a request that is not a
// person's words, and a pick from a turn the task has moved past. In each the
// ledger is left exactly as it was.
func TestAStoreThatFailedHandsNothingOver(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")

	// A turn with choices, so a pick can be made stale by a later turn.
	first, refusal := laneTool(t, db, laneCaller(laneMonitor, "m1"), "turn_append", map[string]any{
		"task_id": "permission", "request_id": "ask-1", "expected_version": 1, "expected_turn_version": 1,
		"turn": map[string]any{"question": "어느 쪽으로 할까요?", "choices": []any{
			map[string]any{"label": "읽기만", "request_text": "읽기 권한만 주세요"},
		}},
	})
	if refusal != "" {
		t.Fatal(refusal)
	}
	choiceTurn, _ := first["turn"].(map[string]any)["id"].(float64)
	if _, r := laneTool(t, db, laneCaller(laneMonitor, "m1"), "turn_append", map[string]any{
		"task_id": "permission", "request_id": "ask-2", "expected_version": 1, "expected_turn_version": 2,
		"turn": map[string]any{"question": "생각이 바뀌었습니다, 다시 묻습니다"},
	}); r != "" {
		t.Fatal(r)
	}

	for _, c := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"a task this slot does not have", map[string]any{
			"id": "r-missing", "task_id": "nosuchtask", "text": "네", "author": "user-1"},
			http.StatusNotFound},
		{"words that are not words", map[string]any{
			"id": "r-empty", "task_id": "permission", "text": "   ", "author": "user-1"},
			http.StatusBadRequest},
		{"a pick the task has moved past", map[string]any{
			"id": "r-stale", "task_id": "permission", "text": "읽기 권한만 주세요",
			"author": "user-1", "choice_turn_id": int64(choiceTurn)},
			http.StatusConflict},
	} {
		code, answer := storeRequestOverHTTP(t, sidecar, c.body)
		if code != c.want {
			t.Errorf("%s = %d, want %d: %v", c.name, code, c.want, answer)
		}
		if _, carried := answer["relay"]; carried {
			t.Errorf("%s claimed a handover: %v", c.name, answer)
		}
	}
	if took := lane.took(); len(took) != 0 {
		t.Errorf("failed stores gave the monitor lane %d prompts, want 0: %v", len(took), took)
	}
}

// TestAResentRequestIsHandedOverOnceRatherThanTwice: the app retrying an answer
// it never saw must not start a second turn on the monitor. The store already
// replays; the handover row is keyed by the turn, so it replays with it.
func TestAResentRequestIsHandedOverOnceRatherThanTwice(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	body := map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네, 읽기만 주세요", "author": "user-1",
	}
	code, first := storeRequestOverHTTP(t, sidecar, body)
	if code != http.StatusCreated {
		t.Fatalf("the first store = %d: %v", code, first)
	}
	turnID := wakeStoredTurnID(t, first)

	code, again := storeRequestOverHTTP(t, sidecar, body)
	if code != http.StatusOK {
		t.Fatalf("the resend = %d, want a replay: %v", code, again)
	}
	if replayed, _ := again["replayed"].(bool); !replayed {
		t.Errorf("the resend was not reported as a replay: %v", again)
	}
	if wakeStoredTurnID(t, again) != turnID {
		t.Errorf("the resend became a second turn: %v", again)
	}
	if relay := wakeRelayOf(t, again); relay["state"] != workRelayAccepted {
		t.Errorf("the resend's handover = %v", relay)
	}
	if took := lane.took(); len(took) != 1 {
		t.Fatalf("the monitor lane was given %d prompts, want 1: %v", len(took), took)
	}
	intent, found := wakeIntentRow(t, db, turnID)
	if !found || intent.State != workWakeDelivered || intent.Attempts != 1 {
		t.Fatalf("the handover row after a resend = %+v (found %v)", intent, found)
	}
}

// TestAHandoverTheLaneWouldNotTakeIsRecordedRatherThanLost: the lane can be
// full, stopping, or not yet restored. The request is stored either way, the
// answer says the handover is pending rather than accepted, and the intent is on
// disk — so the next request carries it, and the monitor's own standing round
// finds the turn regardless.
func TestAHandoverTheLaneWouldNotTakeIsRecordedRatherThanLost(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	lane.set(func(l *stubLane) { l.refusal = http.StatusServiceUnavailable })

	code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, answer)
	}
	turnID := wakeStoredTurnID(t, answer)
	if relay := wakeRelayOf(t, answer); relay["state"] != workRelayPending {
		t.Fatalf("a refused handover = %v, want %s", relay, workRelayPending)
	}
	intent, found := wakeIntentRow(t, db, turnID)
	if !found || intent.State != workWakePending || intent.Attempts != 1 {
		t.Fatalf("the row after a refusal = %+v (found %v)", intent, found)
	}

	// The lane comes back, and the next request carries the waiting one too.
	lane.set(func(l *stubLane) { l.refusal = 0 })
	wakeTestTask(t, db, "second", "review")
	code, next := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-2", "task_id": "second", "text": "이것도요", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("the second store = %d: %v", code, next)
	}
	if took := lane.took(); len(took) != 2 {
		t.Fatalf("the lane was given %d prompts, want both: %v", len(took), took)
	}
	recovered, _ := wakeIntentRow(t, db, turnID)
	if recovered.State != workWakeDelivered {
		t.Fatalf("the waiting handover was not carried: %+v", recovered)
	}
	if recovered.Attempts != 2 {
		t.Errorf("the recovered handover reported attempt %d, want 2 — a re-delivery says so",
			recovered.Attempts)
	}
}

// TestAHandoverWhoseAcknowledgementWasLostIsCarriedAgain is the seam between the
// hop and writing down that it happened: the lane has the prompt and the sidecar
// died before recording it. The claim is leased, so the row comes back, and the
// re-delivery says which attempt it is — the monitor's own contract is to check
// before acting on an attempt above one.
func TestAHandoverWhoseAcknowledgementWasLostIsCarriedAgain(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, answer)
	}
	turnID := wakeStoredTurnID(t, answer)
	// What a crash between the hop and the acknowledgement leaves: a claim
	// nobody holds any more, older than the lease.
	wakeForceRow(t, db, turnID, workWakeDelivering, "",
		workNow(time.Now().Add(-2*workWakeClaimLease)))

	wakeTestTask(t, db, "second", "review")
	if code, next := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-2", "task_id": "second", "text": "또 하나", "author": "user-1",
	}); code != http.StatusCreated {
		t.Fatalf("the second store = %d: %v", code, next)
	}
	took := lane.took()
	if len(took) != 3 {
		t.Fatalf("the lane was given %d prompts, want 3 (one, one again, and the new one): %v",
			len(took), took)
	}
	redelivered := ""
	for _, prompt := range took {
		if strings.Contains(prompt, "delivery="+wakeDeliveryID(turnID)) &&
			strings.Contains(prompt, "attempt=2") {
			redelivered = prompt
		}
	}
	if redelivered == "" {
		t.Fatalf("the lost handover was not carried again as a second attempt: %v", took)
	}
	if !strings.Contains(redelivered, "attempt above 1 means this was delivered before") {
		t.Errorf("the re-delivery does not tell the monitor it may have seen it:\n%s", redelivered)
	}
}

// TestADeliveredHandoverIsNotRepeatedWhileTheLaneStillHasIt: age alone must
// never re-deliver. Every later request would otherwise queue another turn about
// an answer the monitor is already acting on.
func TestADeliveredHandoverIsNotRepeatedWhileTheLaneStillHasIt(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, answer)
	}
	turnID := wakeStoredTurnID(t, answer)
	// Delivered long ago, and the lane still has the receipt.
	intent, _ := wakeIntentRow(t, db, turnID)
	wakeForceRow(t, db, turnID, workWakeDelivered, intent.Ticket,
		workNow(time.Now().Add(-10*workWakeClaimLease)))
	lane.set(func(l *stubLane) { l.ticketState = turnTicketRunning })

	wakeTestTask(t, db, "second", "review")
	if code, next := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-2", "task_id": "second", "text": "또 하나", "author": "user-1",
	}); code != http.StatusCreated {
		t.Fatalf("the second store = %d: %v", code, next)
	}
	took := lane.took()
	if len(took) != 2 {
		t.Fatalf("the lane was given %d prompts, want 2 — the old one must not be repeated: %v",
			len(took), took)
	}
	for _, prompt := range took {
		if strings.Contains(prompt, "attempt=2") {
			t.Errorf("a handover the lane still holds was delivered again:\n%s", prompt)
		}
	}
}

// TestAHandoverIsCarriedAgainWhenTheLaneThatTookItIsGone: the monitor lane's
// conversation does not survive a restart, so a prompt queued on the run that is
// gone went with it. The lane's own answer for a receipt from a previous run of
// itself — unknown_run — is the evidence this acts on, and it is the only one.
func TestAHandoverIsCarriedAgainWhenTheLaneThatTookItIsGone(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, answer)
	}
	turnID := wakeStoredTurnID(t, answer)
	lane.set(func(l *stubLane) { l.ticketState = turnTicketUnknownRun })

	wakeTestTask(t, db, "second", "review")
	if code, next := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-2", "task_id": "second", "text": "또 하나", "author": "user-1",
	}); code != http.StatusCreated {
		t.Fatalf("the second store = %d: %v", code, next)
	}
	carried := 0
	for _, prompt := range lane.took() {
		if strings.Contains(prompt, "delivery="+wakeDeliveryID(turnID)) {
			carried++
		}
	}
	if carried != 2 {
		t.Fatalf("the handover was carried %d times, want 2 — the lane that took it is gone", carried)
	}
	intent, found := wakeIntentRow(t, db, turnID)
	if !found || intent.Attempts != 2 {
		t.Fatalf("the row after the lane restarted = %+v (found %v)", intent, found)
	}
}

// TestAHandoverLeavesOnceTheMonitorHasTheTurn: the monitor's own standing round
// rediscovers the open turn from the ledger regardless of this path, so both can
// arrive at the same request. What stops the work happening twice is the turn:
// one the monitor has started is never handed over, and its row is dropped
// rather than kept as a record nothing will read again.
func TestAHandoverLeavesOnceTheMonitorHasTheTurn(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	lane.set(func(l *stubLane) { l.refusal = http.StatusServiceUnavailable })
	code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, answer)
	}
	turnID := wakeStoredTurnID(t, answer)
	lane.set(func(l *stubLane) { l.refusal = 0 })

	// The standing round got there first and took the turn.
	wakeTakeTurn(t, db, "permission", turnID, "round-take-1")

	wakeTestTask(t, db, "second", "review")
	if code, next := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-2", "task_id": "second", "text": "또 하나", "author": "user-1",
	}); code != http.StatusCreated {
		t.Fatalf("the second store = %d: %v", code, next)
	}
	for _, prompt := range lane.took() {
		if strings.Contains(prompt, "delivery="+wakeDeliveryID(turnID)) {
			t.Errorf("a turn the monitor already holds was handed over:\n%s", prompt)
		}
	}
	if intent, found := wakeIntentRow(t, db, turnID); found {
		t.Errorf("the row for a taken turn was kept: %+v", intent)
	}
}

// TestAReplayAfterTheMonitorTookTheTurnStartsNothing: the app's own resend can
// arrive after the work has been picked up. It must answer about the turn that
// exists and hand nothing over — the monitor is already on it.
func TestAReplayAfterTheMonitorTookTheTurnStartsNothing(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	body := map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네", "author": "user-1",
	}
	code, first := storeRequestOverHTTP(t, sidecar, body)
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, first)
	}
	turnID := wakeStoredTurnID(t, first)
	wakeTakeTurn(t, db, "permission", turnID, "take-1")
	before := len(lane.took())

	code, again := storeRequestOverHTTP(t, sidecar, body)
	if code != http.StatusOK {
		t.Fatalf("the resend = %d: %v", code, again)
	}
	if wakeStoredTurnID(t, again) != turnID {
		t.Errorf("the resend answered about a different turn: %v", again)
	}
	if _, carried := again["relay"]; carried {
		t.Errorf("the resend claimed a handover for a turn the monitor holds: %v", again)
	}
	if after := len(lane.took()); after != before {
		t.Errorf("the resend gave the lane %d more prompts, want 0", after-before)
	}
}

// TestTheHandoverCanOnlyReachThisPodsOwnMonitorLane is the target check: these
// tools exist to reach the conversation next to this sidecar, so an address
// pointing anywhere else is refused at start rather than used. A sidecar that
// could be pointed at another pod would be one that starts turns in somebody
// else's session.
func TestTheHandoverCanOnlyReachThisPodsOwnMonitorLane(t *testing.T) {
	for _, c := range []struct{ name, raw string }{
		{"another pod by name", "http://sess-other.session-platform.svc:8093" + managerLanePathPrefix + "/write"},
		{"another host on loopback's port", "http://10.42.0.7:8093" + managerLanePathPrefix + "/write"},
		{"the parent lane's own write", "http://127.0.0.1:8093/write"},
		{"a path that is not a lane's write", "http://127.0.0.1:8093" + managerLanePathPrefix + "/read"},
	} {
		if _, err := monitorWriteURLFromEnv(func(string) string { return c.raw }); err == nil {
			t.Errorf("%s was accepted as this pod's monitor lane: %s", c.name, c.raw)
		}
	}
	// And the one address that is one.
	ok := "http://127.0.0.1:8093" + managerLanePathPrefix + "/write"
	if got, err := monitorWriteURLFromEnv(func(string) string { return ok }); err != nil || got != ok {
		t.Errorf("this pod's own lane address = %q, %v", got, err)
	}
}

// TestOnlyTheStatusesThatWaitOnAPersonAreHandedOver pins the predicate over the
// whole status domain, so a status added later cannot start waking the monitor
// by accident.
func TestOnlyTheStatusesThatWaitOnAPersonAreHandedOver(t *testing.T) {
	waiting := map[string]bool{"review": true, "blocked": true}
	for _, status := range workStatuses {
		if got := taskAwaitsPersonsJudgement(status); got != waiting[status] {
			t.Errorf("taskAwaitsPersonsJudgement(%q) = %v, want %v", status, got, waiting[status])
		}
	}
}

// TestConcurrentRequestsOnOneTaskAreNeverLostAndNeverHandedOverTwice is the
// overlap a person actually creates: two taps that land together, or an app
// retrying while the first store is still in flight.
//
// What must hold in every tree is that no request is lost and no turn is handed
// over twice. How many handovers there are is deliberately **not** asserted,
// because it is not this change's to decide: where storing a request also
// receives the task off `review` (session_work_views.go), the first of a race
// takes the task out of "waiting on a person" and the ones behind it are then
// ordinary requests on a received task — which the approved contract says are
// found by the monitor's own round, not woken for
// (TestNothingIsHandedOverForARequestNobodyWasWaitingOn). Where nothing moves
// the status, all of them are owed one. Both are correct, and an earlier version
// of this test asserted the second as though it were the only answer — which
// passed alone and failed the moment the two changes met.
//
// The claim is what rules out the duplicate half. It is a conditional update
// (`claimWakeIntent`), so of two passes reaching for the same row only one
// changes it; the loser sees no row to carry rather than sending a second prompt
// about one answer.
func TestConcurrentRequestsOnOneTaskAreNeverLostAndNeverHandedOverTwice(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")

	const senders = 4
	type outcome struct {
		code int
		body map[string]any
	}
	results := make([]outcome, senders)
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Posted by hand rather than through the helper: that one fails the
			// test from inside the goroutine, which is not allowed. Everything
			// here is collected and asserted on the test's own goroutine.
			encoded, err := json.Marshal(map[string]any{
				"id": fmt.Sprintf("app-req-%d", i), "task_id": "permission",
				"text": fmt.Sprintf("동시 요청 %d", i), "author": "user-1",
			})
			if err != nil {
				results[i] = outcome{code: -1}
				return
			}
			resp, err := http.Post(
				sidecar.URL+sessionDBWorkRequestsPath, "application/json", bytes.NewReader(encoded))
			if err != nil {
				results[i] = outcome{code: -1}
				return
			}
			defer resp.Body.Close()
			var answer map[string]any
			raw, _ := io.ReadAll(resp.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &answer)
			}
			results[i] = outcome{code: resp.StatusCode, body: answer}
		}(i)
	}
	wg.Wait()

	turns := map[int64]bool{}
	for i, got := range results {
		if got.code != http.StatusCreated {
			t.Fatalf("concurrent store %d = %d: %v", i, got.code, got.body)
		}
		turns[wakeStoredTurnID(t, got.body)] = true
	}
	if len(turns) != senders {
		t.Fatalf("%d concurrent requests became %d turns, want %d — one was lost or merged",
			senders, len(turns), senders)
	}

	// No turn handed over twice, counted by the delivery id so a re-delivery of
	// one request cannot pass for the delivery of another; and whatever was
	// recorded is settled rather than left half done.
	took := lane.took()
	handed := 0
	for turnID := range turns {
		seen := 0
		for _, prompt := range took {
			if strings.Contains(prompt, "delivery="+wakeDeliveryID(turnID)) {
				seen++
			}
		}
		if seen > 1 {
			t.Errorf("turn %d was handed over %d times, want at most 1:\n%v", turnID, seen, took)
		}
		row, found := wakeIntentRow(t, db, turnID)
		if seen == 1 {
			handed++
			if !found || row.State != workWakeDelivered {
				t.Errorf("turn %d was carried but its handover did not settle: %+v (found %v)",
					turnID, row, found)
			}
			continue
		}
		// Not carried, so there must be no row claiming otherwise — a turn is
		// either owed a handover and delivered one, or owed none at all.
		if found {
			t.Errorf("turn %d was never carried but holds a handover row: %+v", turnID, row)
		}
	}
	if handed == 0 {
		t.Errorf("a task that was waiting on a person took %d answers and told the monitor none: %v",
			senders, took)
	}
	if len(took) != handed {
		t.Errorf("the lane was given %d prompts for %d handed-over turns: %v", len(took), handed, took)
	}
}

// wakeAfterReceiptTaskRow leaves one task's status row exactly as a stored
// receipt leaves it: moved off review, in place, with nothing else touched.
//
// This is the *input state* of the unit under test below, not a reconstruction
// of a contract after the fact. An earlier version of this file wrote this
// column after the storing transaction had already committed and called that a
// test of the receipt race; it was not, and it passed while the real
// combination lost every request. The real sequencing — the receipt moving the
// task inside the same transaction, before the handover is noted — belongs to
// the other change and is verified by integrating the two trees, not here.
func wakeAfterReceiptTaskRow(t *testing.T, db *sessionDB, taskID, status string) {
	t.Helper()
	handle, err := db.handle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
		result, execErr := tx.ExecContext(context.Background(),
			"UPDATE sp_work_tasks SET status=? WHERE id=?", status, taskID)
		if execErr != nil {
			return execErr
		}
		moved, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if moved != 1 {
			t.Fatalf("moving %s to %q touched %d rows, want 1", taskID, status, moved)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTheHandoverIsDecidedOnTheStatusTheRequestWasSentFrom pins the one thing
// that made the real combination lose every request: the decision is an
// argument, settled before the store, and noteRequestWake must not re-derive it
// from a task row the store was allowed to move.
//
// Storing a request on a task shown as `review` receives it as `planned` in the
// same transaction. So by the time the handover is noted, the status reads
// `planned` — and a function that asked again would drop exactly the request
// that proved somebody had been waiting.
func TestTheHandoverIsDecidedOnTheStatusTheRequestWasSentFrom(t *testing.T) {
	db, sidecar, lane := newWakeTestSlot(t)
	wakeTestTask(t, db, "permission", "review")
	code, answer := storeRequestOverHTTP(t, sidecar, map[string]any{
		"id": "app-req-1", "task_id": "permission", "text": "네", "author": "user-1",
	})
	if code != http.StatusCreated {
		t.Fatalf("storing = %d: %v", code, answer)
	}
	turnID := wakeStoredTurnID(t, answer)
	if took := lane.took(); len(took) != 1 {
		t.Fatalf("the first store gave the lane %d prompts, want 1: %v", len(took), took)
	}

	// The row now looks as a receipt leaves it. A second request on the same
	// task is still owed its own handover, decided from what the person saw.
	wakeAfterReceiptTaskRow(t, db, "permission", workRequestReceiptStatusForTest)
	handle, err := db.handle(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// awaited=true while the row says planned: the intent is still recorded,
	// because the answer came from before the store. This is the regression.
	var noted bool
	if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
		turn, loadErr := loadWorkTurn(context.Background(), tx, "permission", turnID)
		if loadErr != nil {
			return loadErr
		}
		// A turn of its own, so this is not the first store's row being found.
		second, insertErr := insertWorkTurn(context.Background(), tx, "permission",
			&workTurnInput{Request: strPtrForTest("두 번째")}, workClock())
		if insertErr != nil {
			return insertErr
		}
		_ = turn
		var noteErr error
		noted, noteErr = noteRequestWake(context.Background(), tx,
			"app-req-2", "permission", true, second, true, workClock())
		return noteErr
	}); err != nil {
		t.Fatal(err)
	}
	if !noted {
		t.Fatal("a handover decided before the store was dropped because the row " +
			"had since been received — this is the bug the combination showed")
	}

	// awaited=false with no row of its own records nothing: a genuinely planned
	// task nobody was waiting on must not be woken just because it is planned.
	var ordinary bool
	if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
		third, insertErr := insertWorkTurn(context.Background(), tx, "permission",
			&workTurnInput{Request: strPtrForTest("세 번째")}, workClock())
		if insertErr != nil {
			return insertErr
		}
		var noteErr error
		ordinary, noteErr = noteRequestWake(context.Background(), tx,
			"app-req-3", "permission", false, third, true, workClock())
		return noteErr
	}); err != nil {
		t.Fatal(err)
	}
	if ordinary {
		t.Error("a request nobody was waiting on was handed over")
	}

	// awaited=false on a turn that already has a row is a replay of a request
	// that *was* owed one, and must still be answered with it.
	var replayed bool
	if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
		turn, loadErr := loadWorkTurn(context.Background(), tx, "permission", turnID)
		if loadErr != nil {
			return loadErr
		}
		var noteErr error
		replayed, noteErr = noteRequestWake(context.Background(), tx,
			"app-req-1", "permission", false, turn, true, workClock())
		return noteErr
	}); err != nil {
		t.Fatal(err)
	}
	if !replayed {
		t.Error("a resend of the request that was handed over was answered as " +
			"though there had never been a handover")
	}
}

// TestTheAwaitedJudgementIsReadFromTheReadPathNotTheStoredByte pins where the
// answer comes from. The read path is what the person was looking at, so a tree
// that folds an older word into a current one is obeyed without this file
// restating that fold — and a tree with no fold answers from the stored word,
// which is then also what is shown.
func TestTheAwaitedJudgementIsReadFromTheReadPathNotTheStoredByte(t *testing.T) {
	db, _, _ := newWakeTestSlot(t)
	waiting := map[string]bool{"review": true, "blocked": true}
	handle, err := db.handle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range workStatuses {
		wakeTestTask(t, db, "t-"+status, "review")
		wakeAfterReceiptTaskRow(t, db, "t-"+status, status)
		var got bool
		if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
			var judgeErr error
			got, judgeErr = requestAwaitedPersonsJudgement(context.Background(), tx, "t-"+status)
			return judgeErr
		}); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if got != waiting[status] {
			t.Errorf("a task shown as %q awaited a person = %v, want %v", status, got, waiting[status])
		}
	}
	// A task that is not there is an error, never a silent "nobody was waiting".
	if err := db.workTx(context.Background(), handle, func(tx *sql.Tx) error {
		_, judgeErr := requestAwaitedPersonsJudgement(context.Background(), tx, "nosuchtask")
		if judgeErr == nil {
			t.Error("a missing task answered instead of failing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// workRequestReceiptStatusForTest is the status a receipt moves a task into. It
// is spelled here rather than imported from the receipt change so that this
// file builds on its own; the two are the same member of workStatuses.
const workRequestReceiptStatusForTest = "planned"

func strPtrForTest(s string) *string { return &s }
