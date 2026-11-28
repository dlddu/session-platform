package main

import (
	"context"
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

func laneCaller(lane, incarnation string) workCaller {
	return workCaller{lane: lane, incarnation: incarnation}
}

// newLaneTestDB is a slot's database with both lanes' callers available.
func newLaneTestDB(t *testing.T) (*sessionDB, workCaller, workCaller) {
	t.Helper()
	db, err := newSessionDB(t.TempDir(), false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, laneCaller(laneParent, "parent-run-1"), laneCaller(laneMonitor, "monitor-run-1")
}

func laneNames(t *testing.T, db *sessionDB, caller workCaller) []string {
	t.Helper()
	return sessionDBToolNames(t, sessionDBLaneHandler(db, caller))
}

// laneTool calls one tool as one lane and returns the tool's own payload, plus
// the JSON-RPC refusal if the call never reached the tool.
func laneTool(t *testing.T, db *sessionDB, caller workCaller, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	resp := sessionDBCall(t, sessionDBLaneHandler(db, caller), "tools/call",
		map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		return nil, resp.Error.Message
	}
	content := resp.Result.(map[string]any)["content"].([]any)[0].(map[string]any)
	var payload map[string]any
	if err := json.Unmarshal([]byte(content["text"].(string)), &payload); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return payload, ""
}

// TestLaneSurfaceSplitsReadsFromWrites is the permission separation as a lane
// meets it: the monitor writes the work records, the parent reads them, and only
// the parent is offered the two calls that hand work over.
func TestLaneSurfaceSplitsReadsFromWrites(t *testing.T) {
	db, parent, monitor := newLaneTestDB(t)
	parentTools := laneNames(t, db, parent)
	monitorTools := laneNames(t, db, monitor)

	// The three review moves are writes too — they write a task's status and the
	// question on its turn, which is exactly what a person reads — so they
	// belong to the lane that owns those rows (session_work_review.go, AC-N13).
	writes := []string{"task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch",
		taskBookPutTool, monitorPutTool, monitorDeleteTool,
		taskReviewOpenTool, taskReviewResumeTool, taskReviewCompleteTool}
	for _, name := range writes {
		if containsString(parentTools, name) {
			t.Errorf("the parent lane is offered %s; the monitor lane owns writing the records", name)
		}
		if !containsString(monitorTools, name) {
			t.Errorf("the monitor lane is not offered %s; it is the lane that writes the records", name)
		}
	}
	reads := []string{"task_get", "task_list", "turn_list", taskBookGetTool, monitorGetTool, monitorListTool}
	for _, name := range reads {
		if !containsString(parentTools, name) {
			t.Errorf("the parent lane cannot read %s; it reads the records directly", name)
		}
		if !containsString(monitorTools, name) {
			t.Errorf("the monitor lane cannot read %s", name)
		}
	}
	// The handover is the parent's alone. The monitor's answer is the turn it is
	// already in, and a tool that started a parent turn would put words in
	// front of a person who had not spoken.
	for _, name := range laneHandoverTools {
		if !containsString(parentTools, name) {
			t.Errorf("the parent lane has no %s; it would have no way to get work done", name)
		}
		if containsString(monitorTools, name) {
			t.Errorf("the monitor lane is offered %s; it does not start the parent's turns", name)
		}
	}

	// A manager with one lane keeps the whole record surface and is offered no
	// handover: there is no other lane to hand anything to.
	single := laneNames(t, db, workCaller{})
	if strings.Join(single, ",") != workToolNames {
		t.Fatalf("an unpaired manager's surface = %v, want the whole one", single)
	}
	for _, name := range laneHandoverTools {
		if containsString(single, name) {
			t.Errorf("an unpaired manager is offered %s", name)
		}
	}
}

// TestParentLaneCannotWriteTheRecords is the guard rather than the menu: a
// client holding a list from before the pair still cannot write, and it learns
// which lane owns the records and how to get the change made.
func TestParentLaneCannotWriteTheRecords(t *testing.T) {
	db, parent, _ := newLaneTestDB(t)
	_, refusal := laneTool(t, db, parent, "task_put", map[string]any{
		"id": "t1", "request_id": "r1", "expected_version": 0,
		"task": map[string]any{"session_id": nil, "title": "t", "description": "d",
			"completion_criteria": "c", "status": "planned"},
	})
	for _, want := range []string{"monitor", laneAskTool, laneAnswerTool} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal does not name %q: %s", want, refusal)
		}
	}
	// Nothing was written.
	if _, r := laneTool(t, db, laneCaller(laneMonitor, "m1"), "task_get", map[string]any{"id": "t1"}); r == "" {
		if payload, _ := laneTool(t, db, laneCaller(laneMonitor, "m1"), "task_get", map[string]any{"id": "t1"}); payload["error"] == nil {
			t.Error("the parent's refused task_put left a task behind")
		}
	}
}

// TestTheMonitorIsToldItsAnswerIsItsTurn: the monitor calling the parent's
// handover learns why it has none, rather than that its arguments were wrong.
func TestTheMonitorIsToldItsAnswerIsItsTurn(t *testing.T) {
	db, _, monitor := newLaneTestDB(t)
	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1/monitor/write")
	for _, name := range laneHandoverTools {
		_, refusal := laneTool(t, db, monitor, name, map[string]any{"request": "x", "request_id": "r", "ticket": "e-1"})
		if !strings.Contains(refusal, "parent lane's") {
			t.Errorf("%s as the monitor was refused with %q", name, refusal)
		}
	}
}

func TestTheHandoverIsRefusedToASingleLaneManager(t *testing.T) {
	db, _, _ := newLaneTestDB(t)
	for _, name := range laneHandoverTools {
		_, refusal := laneTool(t, db, workCaller{}, name, map[string]any{"request": "x", "request_id": "r", "ticket": "e-1"})
		if !strings.Contains(refusal, "single lane") {
			t.Errorf("%s on an unpaired manager was refused with %q", name, refusal)
		}
	}
}

// TestLaneAskReplaysARetryRatherThanStartingASecondTurn: a parent whose answer
// was lost resends the same call and learns the receipt it already has.
func TestLaneAskReplaysARetryRatherThanStartingASecondTurn(t *testing.T) {
	db, parent, _ := newLaneTestDB(t)
	var queued int
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queued++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued", "ticket": fmt.Sprintf("run-%d", queued)})
	}))
	t.Cleanup(lane.Close)
	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1/monitor/write")
	db.monitorLane.writeURL = lane.URL

	first, refusal := laneTool(t, db, parent, laneAskTool, map[string]any{
		"request": "roll the release out in stages", "request_id": "ask-1",
	})
	if refusal != "" {
		t.Fatal(refusal)
	}
	if first["ticket"] != "run-1" || first["replayed"] != false {
		t.Fatalf("the first ask = %v", first)
	}
	again, _ := laneTool(t, db, parent, laneAskTool, map[string]any{
		"request": "roll the release out in stages", "request_id": "ask-1",
	})
	if again["ticket"] != "run-1" || again["replayed"] != true {
		t.Fatalf("the retry = %v, want the first receipt replayed", again)
	}
	if queued != 1 {
		t.Fatalf("the monitor lane was given %d prompts, want 1 — a retry is not a second turn", queued)
	}
	// The same id for different words is a conflict rather than a second turn
	// quietly sharing the first one's receipt.
	conflict, _ := laneTool(t, db, parent, laneAskTool, map[string]any{
		"request": "something else entirely", "request_id": "ask-1",
	})
	if failure, _ := conflict["error"].(string); !strings.Contains(failure, "conflict") {
		t.Fatalf("a reused id with different words = %v", conflict)
	}
	if queued != 1 {
		t.Fatalf("the conflicting ask still queued a turn (%d)", queued)
	}
}

// TestLaneAskIssuesNoReceiptWhenTheLaneRefuses is the dangling-receipt case one
// level up: the sidecar must not invent a receipt the lane never gave.
func TestLaneAskIssuesNoReceiptWhenTheLaneRefuses(t *testing.T) {
	db, parent, _ := newLaneTestDB(t)
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "claude prompt queue is full", http.StatusTooManyRequests)
	}))
	t.Cleanup(lane.Close)
	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1/monitor/write")
	db.monitorLane.writeURL = lane.URL

	payload, _ := laneTool(t, db, parent, laneAskTool, map[string]any{
		"request": "do the thing", "request_id": "ask-1",
	})
	if payload["ticket"] != nil {
		t.Fatalf("a refused handover returned a receipt: %v", payload)
	}
	if failure, _ := payload["error"].(string); !strings.Contains(failure, "did not take the request") {
		t.Fatalf("the refusal = %v", payload)
	}
	// And the id is not remembered, so asking again really asks again.
	if _, replayed := db.monitorLane.replay("ask-1"); replayed {
		t.Error("a request that was never queued was remembered as asked")
	}
}

func TestLaneAskChecksItsArguments(t *testing.T) {
	db, parent, _ := newLaneTestDB(t)
	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1/monitor/write")
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty request", map[string]any{"request": "   ", "request_id": "r1"}, "must not be empty"},
		{"long request", map[string]any{"request": strings.Repeat("x", laneAskRequestMax+1), "request_id": "r1"}, "at most"},
		{"bad id", map[string]any{"request": "x", "request_id": "no spaces here"}, "request_id must be"},
	} {
		payload, _ := laneTool(t, db, parent, laneAskTool, c.args)
		if failure, _ := payload["error"].(string); !strings.Contains(failure, c.want) {
			t.Errorf("%s = %v, want %q", c.name, payload, c.want)
		}
	}
	payload, _ := laneTool(t, db, parent, laneAnswerTool, map[string]any{"ticket": " "})
	if failure, _ := payload["error"].(string); !strings.Contains(failure, "ticket required") {
		t.Errorf("an empty ticket = %v", payload)
	}
}

// TestTheParentKeepsTalkingWhileTheMonitorAnswers is the design's whole point,
// end to end on the normal path with nothing mocked between the pieces: two real
// lane workloads behind one agent port, the real session database behind the
// real sidecar MCP handler at the real lane addresses, and the real loopback hop
// the sidecar makes to hand the request over.
//
// The parent's turn never ends — it is the person's long answer. It hands the
// request over mid-turn, is given a receipt immediately, and the monitor's turn
// runs beside it. What the monitor then does is the thing the parent is not
// allowed to do, which is write the records.
func TestTheParentKeepsTalkingWhileTheMonitorAnswers(t *testing.T) {
	db, err := newSessionDB(t.TempDir(), false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sidecar := httptest.NewServer(withSessionDBMCP(http.NotFoundHandler(), db))
	t.Cleanup(sidecar.Close)

	laneURL := func(lane, incarnation string) string {
		url, err := sessionDBLaneMCPURL(sidecar.URL+sessionDBMCPPath, lane, incarnation)
		if err != nil {
			t.Fatal(err)
		}
		return url
	}
	parentMCP := laneURL(laneParent, "parent-run-1")
	monitorMCP := laneURL(laneMonitor, "monitor-run-1")

	const theRequest = "Record the staged rollout on the work and take the first step."
	answered := make(chan string, 1)
	holdParent := make(chan struct{})
	var release sync.Once
	runner := &lanePairRunner{
		// The parent takes the person's decision, hands it over, and keeps
		// writing. It is still inside this run when the assertions happen.
		onParent: func(opts runnerOptions) {
			ask := mcpCall(t, parentMCP, laneAskTool, map[string]any{
				"request": theRequest, "request_id": "parent-ask-1",
			})
			ticket, _ := ask["ticket"].(string)
			if ticket == "" {
				t.Errorf("lane_ask returned no receipt: %v", ask)
				return
			}
			answered <- ticket
			<-holdParent
		},
		// The monitor, given the request, does it: writes the records and says
		// what it did. Its answer is this turn's own output — there is no ack.
		onMonitor: func(prompt string, opts runnerOptions) {
			if !strings.Contains(prompt, theRequest) {
				t.Errorf("the monitor's turn was given %q, want the request verbatim", prompt)
			}
			mcpCall(t, monitorMCP, "task_put", map[string]any{
				"id": "rollout", "request_id": "monitor-task-1", "expected_version": 0,
				"task": map[string]any{"session_id": nil, "title": "staged rollout",
					"description":         "roll the release out in stages",
					"completion_criteria": "every stage is live", "status": "running"},
			})
			mcpCall(t, monitorMCP, "turn_append", map[string]any{
				"task_id": "rollout", "request_id": "monitor-turn-1",
				"expected_version": 1, "expected_turn_version": 1,
				"turn": map[string]any{"instruction": "stage 1"},
			})
			_, _ = io.WriteString(opts.Stdout, "recorded the choice on task rollout and started stage 1\n")
		},
	}

	parent, monitor, srv := newLanePairTestServer(t, runner)
	runner.monitorDir = monitor.stateDir
	t.Cleanup(func() { release.Do(func() { close(holdParent) }) })
	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1/monitor/write")
	db.monitorLane.writeURL = srv.URL + managerLanePathPrefix + "/write"
	db.monitorLane.turnURL = srv.URL + managerLanePathPrefix + agentTurnPath

	// The only write in this test: the person speaks to the parent.
	laneWrite(t, srv, "", "go with the staged rollout")

	var ticket string
	select {
	case ticket = <-answered:
	case <-time.After(20 * time.Second):
		t.Fatal("the parent never got a receipt")
	}

	// The parent is *still* inside its turn — it did not wait for the monitor.
	if parent.queueState().Idle {
		t.Fatal("the parent's turn had already ended; this did not prove it keeps talking")
	}

	// The receipt resolves to the monitor turn's own answer, and only once that
	// turn is done.
	var answer map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for {
		answer = mcpCall(t, parentMCP, laneAnswerTool, map[string]any{"ticket": ticket})
		if answer["state"] == turnTicketDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the receipt never resolved to a finished turn: %v", answer)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if response, _ := answer["response"].(string); !strings.Contains(response, "task rollout") {
		t.Fatalf("the receipt did not carry the monitor turn's answer: %v", answer)
	}
	if answer["lane"] != laneMonitor {
		t.Errorf("the answer does not say which lane it came from: %v", answer)
	}

	// And what lasts is in the records, which the monitor wrote and the parent
	// reads directly on its own read-only surface.
	record := mcpCall(t, parentMCP, "task_get", map[string]any{"id": "rollout"})
	task := record["task"].(map[string]any)
	if task["title"] != "staged rollout" || task["status"] != "running" {
		t.Fatalf("the parent does not see the record the monitor wrote: %v", record)
	}
	turns := mcpCall(t, parentMCP, "turn_list", map[string]any{"task_id": "rollout"})
	if len(turns["turns"].([]any)) != 1 {
		t.Fatalf("the monitor's turn is not on the task: %v", turns)
	}
}

// lanePairRunner is the two lanes' models, scripted. Which lane a run belongs to
// is its HOME, which is the only thing that differs between them.
type lanePairRunner struct {
	monitorDir string
	onParent   func(runnerOptions)
	onMonitor  func(string, runnerOptions)

	mu   sync.Mutex
	seen []string
}

func (r *lanePairRunner) Run(_ context.Context, argv []string, opts runnerOptions) error {
	prompt := argv[len(argv)-1]
	monitor := r.monitorDir != "" && strings.Contains(strings.Join(opts.Env, " "), r.monitorDir)
	r.mu.Lock()
	r.seen = append(r.seen, prompt)
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
	return nil
}

// mcpCall is one tools/call against a lane's MCP address over HTTP.
func mcpCall(t *testing.T, url, name string, args map[string]any) map[string]any {
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
	return payload
}
