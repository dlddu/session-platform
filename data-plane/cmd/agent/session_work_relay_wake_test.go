package main

import (
	"context"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLane is the monitor lane as the waker uses it: free or not, and a record
// of the prompts that were put on its queue.
type fakeLane struct {
	mu      sync.Mutex
	free    bool
	prompts []string
	idleErr error
	wrErr   error
}

func (l *fakeLane) idle(context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.free, l.idleErr
}

func (l *fakeLane) write(_ context.Context, prompt string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wrErr != nil {
		return l.wrErr
	}
	l.prompts = append(l.prompts, prompt)
	return nil
}

func (l *fakeLane) sent() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.prompts...)
}

func newTestWaker(t *testing.T, db *sessionDB, lane relayWakeTarget) *relayWaker {
	t.Helper()
	w := newRelayWaker(db.monitorRelayWakeSet, lane, testLogger())
	w.sweep = time.Millisecond
	return w
}

// TestRelaySendWakesTheMonitorLane is the gap the pair would otherwise have: the
// parent hands over a message and the monitor's turn starts, with nobody outside
// the pod asked to start it. The prompt is the wake, not the message — the
// message is the row the monitor claims for itself.
func TestRelaySendWakesTheMonitorLane(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	lane := &fakeLane{free: true}
	db.waker = newTestWaker(t, db, lane)

	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindInstruction, "ship it", "roll the release out"),
	})
	if got := db.waker.sweepOnce(context.Background()); got != db.waker.sweep {
		t.Fatalf("sweep returned %s, want the ordinary sweep %s", got, db.waker.sweep)
	}

	sent := lane.sent()
	if len(sent) != 1 {
		t.Fatalf("the monitor lane was given %d turns, want exactly one: %#v", len(sent), sent)
	}
	for _, tool := range []string{relayListTool, relayClaimTool, relayAckTool} {
		if !strings.Contains(sent[0], tool) {
			t.Fatalf("the wake prompt does not name %s, so the lane is not told how to take the work: %q", tool, sent[0])
		}
	}
	// The wake must not carry the handover, or a dropped prompt would be a lost
	// instruction rather than a late turn.
	for _, leaked := range []string{"ship it", "roll the release out", "m1"} {
		if strings.Contains(sent[0], leaked) {
			t.Fatalf("the wake prompt carries the message itself (%q); the database is the handover", leaked)
		}
	}
}

// A nudge that nobody is listening for is not an error: an unpaired manager has
// no waker at all, and relay_send still commits.
func TestRelaySendWithoutAWakerStillCommits(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	if db.waker != nil {
		t.Fatal("a fresh database has a waker")
	}
	sent := mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindInstruction, "ship it", "roll the release out"),
	})
	if sent["state"] != relayStatePending {
		t.Fatalf("relay_send = %v, want a pending message", sent["state"])
	}
	db.waker.notify() // a nil waker is nudged by the same call path
}

// The sweep is level-triggered: it asks the rows, not a queue of events. So a
// settled inbox wakes nothing, and a message that is merely held by a lane that
// has gone idle is still a reason to wake — that is the restart case, where the
// claim's own incarnation is gone and nobody else will ever settle it.
func TestRelayWakeFollowsTheRowsNotTheEvents(t *testing.T) {
	db, parent, monitor := newRelayTestDB(t)
	lane := &fakeLane{free: true}
	waker := newTestWaker(t, db, lane)

	// Nothing sent yet: no table, nothing to wake, and the sweep must not create
	// the schema just to count it as empty.
	waker.sweepOnce(context.Background())
	if len(lane.sent()) != 0 {
		t.Fatalf("an empty slot woke the lane: %#v", lane.sent())
	}

	sent := mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindInstruction, "ship it", "roll the release out"),
	})
	waker.sweepOnce(context.Background())
	if len(lane.sent()) != 1 {
		t.Fatalf("a pending message woke the lane %d times, want 1", len(lane.sent()))
	}

	// Claimed and then abandoned: the monitor's incarnation is gone, the lane is
	// idle again, and the message is still unsettled.
	claim := mustLaneCall(t, db, monitor, relayClaimTool, map[string]any{
		"id": "m1", "request_id": "c1", "expected_version": sent["version"],
	})
	waker.sweepOnce(context.Background())
	if len(lane.sent()) != 2 {
		t.Fatalf("an unsettled claim on an idle lane woke it %d times, want 2 — nobody else settles it", len(lane.sent()))
	}

	taken := claim["message"].(map[string]any)
	mustLaneCall(t, db, monitor, relayAckTool, map[string]any{
		"id": "m1", "request_id": "a1", "claim_token": claim["claim_token"],
		"outcome": relayStateProcessed, "result": "released",
	})
	_ = taken
	waker.sweepOnce(context.Background())
	if len(lane.sent()) != 2 {
		t.Fatalf("a settled inbox woke the lane again (%d turns); settled is the end of it", len(lane.sent()))
	}
}

// A lane that is already working is not woken: the turn it is running will see
// whatever landed, and a second prompt would only queue a turn behind it.
func TestRelayWakeLeavesABusyLaneAlone(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	lane := &fakeLane{free: false}
	waker := newTestWaker(t, db, lane)

	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindInstruction, "ship it", "roll the release out"),
	})
	for i := 0; i < relayWakeMaxTries+3; i++ {
		if got := waker.sweepOnce(context.Background()); got != waker.sweep {
			t.Fatalf("a busy lane backed the sweep off to %s; being busy is not a failed wake", got)
		}
	}
	if len(lane.sent()) != 0 {
		t.Fatalf("a busy lane was given %d turns", len(lane.sent()))
	}

	lane.mu.Lock()
	lane.free = true
	lane.mu.Unlock()
	waker.sweepOnce(context.Background())
	if len(lane.sent()) != 1 {
		t.Fatalf("the lane went idle with the message still open and was woken %d times, want 1", len(lane.sent()))
	}
}

// An inbox that never changes stops being asked about every few seconds. The
// message stays open rather than being dropped, which is what relay_list and
// relay_status keep showing.
func TestRelayWakeBacksOffOnALaneThatNeverSettles(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	lane := &fakeLane{free: true}
	waker := newTestWaker(t, db, lane)

	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindInstruction, "ship it", "roll the release out"),
	})
	for i := 0; i < relayWakeMaxTries; i++ {
		if got := waker.sweepOnce(context.Background()); got != waker.sweep {
			t.Fatalf("sweep %d backed off early (%s)", i+1, got)
		}
	}
	if got := waker.sweepOnce(context.Background()); got != relayWakeBackoff {
		t.Fatalf("sweep after %d wakes = %s, want the backoff %s", relayWakeMaxTries, got, relayWakeBackoff)
	}
	if len(lane.sent()) != relayWakeMaxTries {
		t.Fatalf("the lane was woken %d times, want at most %d before backing off", len(lane.sent()), relayWakeMaxTries)
	}

	// Progress resets it: a second message is a different inbox, and the lane is
	// asked again at once.
	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m2", "request_id": "r2",
		"message": relayMessageArgs(relayKindDecision, "the person chose B", "go with B"),
	})
	if got := waker.sweepOnce(context.Background()); got != waker.sweep {
		t.Fatalf("a changed inbox stayed backed off (%s)", got)
	}
	if len(lane.sent()) != relayWakeMaxTries+1 {
		t.Fatalf("a new message did not wake the lane: %d turns", len(lane.sent()))
	}
}

// A message for the parent is never a wake. A parent turn is a turn a person
// started, so starting one here would put text in front of somebody who did not
// speak; the parent reads its inbox when it next answers.
func TestRelayWakeNeverStartsAParentTurn(t *testing.T) {
	db, _, monitor := newRelayTestDB(t)
	lane := &fakeLane{free: true}
	db.waker = newTestWaker(t, db, lane)

	mustLaneCall(t, db, monitor, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindResult, "released", "the rollout finished"),
	})
	db.waker.sweepOnce(context.Background())
	if len(lane.sent()) != 0 {
		t.Fatalf("a message addressed to the parent woke the monitor lane: %#v", lane.sent())
	}
}

// A lane that cannot be reached is a retry, not a crash and not a lost message.
func TestRelayWakeRetriesAnUnreachableLane(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	lane := &fakeLane{free: true, wrErr: http.ErrServerClosed}
	waker := newTestWaker(t, db, lane)

	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindInstruction, "ship it", "roll the release out"),
	})
	if got := waker.sweepOnce(context.Background()); got != waker.sweep {
		t.Fatalf("a failed write = %s, want another sweep %s", got, waker.sweep)
	}
	lane.mu.Lock()
	lane.wrErr = nil
	lane.mu.Unlock()
	waker.sweepOnce(context.Background())
	if len(lane.sent()) != 1 {
		t.Fatalf("the lane was woken %d times once reachable, want 1", len(lane.sent()))
	}
}

// run is what main starts: a nudge is answered without waiting for the tick, and
// the loop stops with its context.
func TestRelayWakeRunAnswersANudge(t *testing.T) {
	db, parent, _ := newRelayTestDB(t)
	lane := &fakeLane{free: true}
	waker := newRelayWaker(db.monitorRelayWakeSet, lane, testLogger())
	waker.sweep = time.Hour // only the nudge can produce a wake in this test
	db.waker = waker

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { waker.run(ctx); close(done) }()

	mustLaneCall(t, db, parent, relaySendTool, map[string]any{
		"id": "m1", "request_id": "r1",
		"message": relayMessageArgs(relayKindInstruction, "ship it", "roll the release out"),
	})
	deadline := time.Now().Add(5 * time.Second)
	for len(lane.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(lane.sent()) == 0 {
		t.Fatal("relay_send did not wake the monitor lane within 5s, with a sweep an hour away")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the waker did not stop with its context")
	}
}

// The wake goes to the monitor lane's own conversation over loopback, and reads
// that lane's own queue to decide. Both are the lane's paths, not the pod's.
func TestHTTPRelayWakeTargetUsesTheLanesOwnPaths(t *testing.T) {
	var gotQueue, gotWrite string
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gotQueue = r.URL.Path
			_ = json.NewEncoder(w).Encode(agentQueueState{Idle: true})
		case http.MethodPost:
			gotWrite = r.URL.Path
			raw := make([]byte, 4096)
			n, _ := r.Body.Read(raw)
			body = string(raw[:n])
			_, _ = w.Write([]byte(`{"status":"queued"}`))
		}
	}))
	defer server.Close()

	target := newHTTPRelayWakeTarget(server.URL + managerLanePathPrefix + "/write")
	idle, err := target.idle(context.Background())
	if err != nil || !idle {
		t.Fatalf("idle = %v, %v, want true", idle, err)
	}
	if err := target.write(context.Background(), relayWakePrompt); err != nil {
		t.Fatal(err)
	}
	if gotQueue != managerLanePathPrefix+agentQueuePath {
		t.Fatalf("queue read at %q, want %q", gotQueue, managerLanePathPrefix+agentQueuePath)
	}
	if gotWrite != managerLanePathPrefix+"/write" {
		t.Fatalf("wake written to %q, want the monitor lane's own write", gotWrite)
	}
	if body != relayWakePrompt {
		t.Fatalf("the lane was given %q", body)
	}
}

// A lane whose queue cannot be read is not woken blindly: a wake queued behind a
// running turn is a turn nobody asked for.
func TestHTTPRelayWakeTargetRefusesANonLoopbackAddress(t *testing.T) {
	for _, raw := range []string{
		"http://10.0.0.1:8080/monitor/write",
		"https://127.0.0.1:8080/monitor/write",
		"http://127.0.0.1:8080/write",
		"http://127.0.0.1:8080/monitor/read",
	} {
		if _, err := monitorWakeURLFromEnv(func(string) string { return raw }); err == nil {
			t.Fatalf("%s accepted %q", managerMonitorWakeURLEnv, raw)
		}
	}
	url := "http://127.0.0.1:8080" + managerLanePathPrefix + "/write"
	got, err := monitorWakeURLFromEnv(func(string) string { return url })
	if err != nil || got != url {
		t.Fatalf("%s = %q, %v, want %q", managerMonitorWakeURLEnv, got, err, url)
	}
	// Unset is a sidecar with no lane to wake, which is every pod that is not a
	// paired manager.
	if got, err := monitorWakeURLFromEnv(func(string) string { return "  " }); err != nil || got != "" {
		t.Fatalf("unset %s = %q, %v, want empty", managerMonitorWakeURLEnv, got, err)
	}
}

// laneScriptRunner is the two lanes' models, scripted. Which lane a run belongs
// to comes from the state directory it runs in, which is the one thing that
// actually differs between them in the pod.
type laneScriptRunner struct {
	monitorDir string
	onParent   func(opts runnerOptions)
	onMonitor  func(prompt string, opts runnerOptions)

	mu      sync.Mutex
	prompts []string
}

func (r *laneScriptRunner) Run(ctx context.Context, argv []string, opts runnerOptions) error {
	prompt := ""
	if len(argv) > 0 {
		prompt = argv[len(argv)-1]
	}
	// The lanes differ by the state tree they run in, and it reaches the run as
	// its HOME and its working directory (claude.go).
	monitor := strings.HasPrefix(opts.Dir, r.monitorDir) ||
		strings.Contains(strings.Join(opts.Env, " "), r.monitorDir)
	r.mu.Lock()
	label := "parent"
	if monitor {
		label = "monitor"
	}
	r.prompts = append(r.prompts, label+": "+prompt)
	r.mu.Unlock()
	if monitor {
		if r.onMonitor != nil {
			r.onMonitor(prompt, opts)
		}
		return nil
	}
	if r.onParent != nil {
		r.onParent(opts)
	}
	return ctx.Err()
}

func (r *laneScriptRunner) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

// mcpToolCall is one tools/call over HTTP at a lane's own MCP address — the way
// a lane's model reaches the sidecar, rather than through the handler directly.
func mcpToolCall(t *testing.T, url, name string, args map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer resp.Body.Close()
	var decoded struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if decoded.Error != nil {
		t.Fatalf("%s: JSON-RPC error %s", name, decoded.Error.Message)
	}
	content := decoded.Result["content"].([]any)[0].(map[string]any)
	var payload map[string]any
	if err := json.Unmarshal([]byte(content["text"].(string)), &payload); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if decoded.Result["isError"] == true {
		t.Fatalf("%s refused: %v", name, payload)
	}
	return payload
}

// TestMonitorMovesTheWorkWhileTheParentIsStillAnswering is the pair's whole
// point, end to end on the normal path and with nothing mocked between the
// pieces: two real lane workloads behind one agent port, the real session
// database behind the real sidecar MCP handler at the real lane addresses, and
// the real waker making its real loopback hop.
//
// The parent's turn never ends — it is the person's long answer. Nothing in this
// test writes to the monitor lane: if the monitor runs at all, the sidecar woke
// it. What it then does is the thing the parent is not allowed to do, which is
// write the records.
func TestMonitorMovesTheWorkWhileTheParentIsStillAnswering(t *testing.T) {
	db, err := newSessionDB(t.TempDir(), false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sidecar := httptest.NewServer(withSessionDBMCP(http.NotFoundHandler(), db))
	t.Cleanup(sidecar.Close)

	// Each lane's MCP address, derived the way the platform renders it into that
	// lane's managed config — the role is in the path, never in an argument.
	laneURL := func(lane, incarnation string) string {
		url, err := sessionDBLaneMCPURL(sidecar.URL+sessionDBMCPPath, lane, incarnation)
		if err != nil {
			t.Fatal(err)
		}
		return url
	}
	parentMCP := laneURL(relayLaneParent, "parent-run-1")
	monitorMCP := laneURL(relayLaneMonitor, "monitor-run-1")

	settled := make(chan map[string]any, 1)
	// The parent's turn is held open for the whole test — it is the person's long
	// answer.
	holdParent := make(chan struct{})
	var release sync.Once
	runner := &laneScriptRunner{
		// The parent takes the person's decision, hands it over and keeps
		// writing. It is still inside this run when the assertions happen.
		onParent: func(runnerOptions) {
			mcpToolCall(t, parentMCP, relaySendTool, map[string]any{
				"id": "handover-1", "request_id": "parent-send-1",
				"message": map[string]any{
					"kind": relayKindDecision, "subject": "the person chose the staged rollout",
					"body": "Record the choice on the work and take the first step.",
				},
			})
			<-holdParent
		},
		// The monitor, woken, works its inbox: claim, write the records, ack.
		onMonitor: func(_ string, _ runnerOptions) {
			open := mcpToolCall(t, monitorMCP, relayListTool, map[string]any{"box": "inbox", "open": true})
			messages := open["messages"].([]any)
			if len(messages) != 1 {
				t.Errorf("the woken monitor found %d open messages, want 1", len(messages))
				return
			}
			message := messages[0].(map[string]any)
			claim := mcpToolCall(t, monitorMCP, relayClaimTool, map[string]any{
				"id": message["id"], "request_id": "monitor-claim-1",
				"expected_version": message["version"],
			})
			mcpToolCall(t, monitorMCP, "task_put", map[string]any{
				"id": "rollout", "request_id": "monitor-task-1", "expected_version": 0,
				"task": map[string]any{
					"session_id": nil, "title": "staged rollout",
					"description":         "roll the release out in stages",
					"completion_criteria": "every stage is live",
					"status":              "running",
				},
			})
			mcpToolCall(t, monitorMCP, "turn_append", map[string]any{
				"task_id": "rollout", "request_id": "monitor-turn-1",
				"expected_version": 1, "expected_turn_version": 1,
				"turn": map[string]any{"instruction": "stage 1"},
			})
			done := mcpToolCall(t, monitorMCP, relayAckTool, map[string]any{
				"id": message["id"], "request_id": "monitor-ack-1",
				"claim_token": claim["claim_token"], "outcome": relayStateProcessed,
				"result": "recorded the choice on task rollout and started stage 1",
			})
			settled <- done
		},
	}

	_, monitorLane, srv := newLanePairTestServer(t, runner)
	runner.monitorDir = monitorLane.stateDir
	// Registered after the server so it runs before the server's own cleanup
	// (cleanups are LIFO): a failing assertion must still let the held turn go,
	// or closing the lane would wait on it for ever.
	t.Cleanup(func() { release.Do(func() { close(holdParent) }) })
	// The sidecar's real hop onto the monitor lane's own queue, over loopback.
	waker := newRelayWaker(db.monitorRelayWakeSet, newHTTPRelayWakeTarget(srv.URL+managerLanePathPrefix+"/write"), testLogger())
	waker.sweep = 20 * time.Millisecond
	db.waker = waker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waker.run(ctx)

	// The only write in this test: the person speaks to the parent.
	laneWrite(t, srv, "", "go with the staged rollout")

	select {
	case done := <-settled:
		if done["state"] != relayStateProcessed {
			t.Fatalf("the monitor settled the handover as %v", done["state"])
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the monitor lane never ran; nobody but the sidecar could start it. runs: %#v", runner.seen())
	}

	// The parent is *still* inside its turn — the monitor did all of that beside
	// it, not after it.
	if laneQueue(t, srv, "").Idle {
		t.Fatal("the parent's turn had already ended; this did not prove the lanes overlap")
	}

	// What the monitor wrote, read back the way the parent reads it: directly,
	// from the same database, on its own read-only surface.
	record := mcpToolCall(t, parentMCP, "task_get", map[string]any{"id": "rollout"})
	task := record["task"].(map[string]any)
	if task["title"] != "staged rollout" || task["status"] != "running" {
		t.Fatalf("the parent does not see the record the monitor wrote: %#v", record)
	}
	turns := mcpToolCall(t, parentMCP, "turn_list", map[string]any{"task_id": "rollout"})
	if len(turns["turns"].([]any)) != 1 {
		t.Fatalf("the monitor's turn is not on the task: %#v", turns)
	}
	status := mcpToolCall(t, parentMCP, relayStatusTool, map[string]any{"id": "handover-1"})
	if status["dealt_with"] != true || status["settled"] != true {
		t.Fatalf("relay_status does not answer that the handover was dealt with: %#v", status)
	}
	message := status["message"].(map[string]any)
	if !strings.Contains(message["result"].(string), "task rollout") {
		t.Fatalf("the parent does not read the monitor's result: %#v", message["result"])
	}

	// And the wake itself was a wake: the monitor's prompt carried no message.
	var monitorPrompt string
	for _, seen := range runner.seen() {
		if strings.HasPrefix(seen, "monitor: ") {
			monitorPrompt = strings.TrimPrefix(seen, "monitor: ")
		}
	}
	if !strings.Contains(monitorPrompt, relayListTool) {
		t.Fatalf("the monitor was started by something other than the waker: %q", monitorPrompt)
	}
	if strings.Contains(monitorPrompt, "staged rollout") {
		t.Fatalf("the wake carried the handover instead of pointing at it: %q", monitorPrompt)
	}
	release.Do(func() { close(holdParent) })
}

// laneQueue reads one lane's queue state over that lane's own path.
func laneQueue(t *testing.T, srv *httptest.Server, prefix string) agentQueueState {
	t.Helper()
	resp, err := http.Get(srv.URL + prefix + agentQueuePath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state agentQueueState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
