package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSchedulerAgent is this pod's conversations in memory. One entry per
// lane, so a test can hold the person's chat busy while the monitor is idle,
// and each lane issues receipts for what it queues the way a real one does
// (turn_ticket.go).
type fakeSchedulerAgent struct {
	mu      sync.Mutex
	lanes   map[string]*fakeSchedulerLane
	monitor bool
}

// fakeSchedulerLane is one conversation: its queue state, what was written to
// it and the receipts it has issued.
type fakeSchedulerLane struct {
	state  agentQueueState
	writes []string
	issued []*fakeSchedulerTicket
	// writeErr is this lane refusing what it is given, as a lane does when its
	// queue is full or it is checkpointing. It is per lane: one conversation
	// refusing a round says nothing about the other.
	writeErr error
	// noTicket is a lane too old to issue receipts, whose rounds are settled
	// from the queue state alone.
	noTicket bool
}

type fakeSchedulerTicket struct{ id, epoch, state string }

func newFakeSchedulerAgent(lanes ...string) *fakeSchedulerAgent {
	f := &fakeSchedulerAgent{lanes: map[string]*fakeSchedulerLane{}}
	for _, lane := range lanes {
		f.at(lane)
		if lane == laneMonitor {
			f.monitor = true
		}
	}
	return f
}

// at is one lane's record, created on demand so a test names only the lanes it
// uses.
func (f *fakeSchedulerAgent) at(lane string) *fakeSchedulerLane {
	if l, ok := f.lanes[lane]; ok {
		return l
	}
	l := &fakeSchedulerLane{state: agentQueueState{Idle: true, Epoch: "e1"}}
	f.lanes[lane] = l
	return l
}

func (f *fakeSchedulerAgent) reaches(lane string) bool {
	switch lane {
	case "", laneParent:
		return true
	case laneMonitor:
		return f.monitor
	default:
		return false
	}
}

func (f *fakeSchedulerAgent) queueState(_ context.Context, lane string) (agentQueueState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at(lane).state, nil
}

func (f *fakeSchedulerAgent) write(_ context.Context, lane, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.at(lane)
	if l.writeErr != nil {
		return "", l.writeErr
	}
	l.writes = append(l.writes, prompt)
	l.state.Idle = false
	if l.noTicket {
		return "", nil
	}
	issued := &fakeSchedulerTicket{
		id:    fmt.Sprintf("%s-%d", l.state.Epoch, len(l.issued)+1),
		epoch: l.state.Epoch, state: turnTicketQueued,
	}
	l.issued = append(l.issued, issued)
	return issued.id, nil
}

func (f *fakeSchedulerAgent) turnState(_ context.Context, lane, ticket string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.at(lane)
	for _, issued := range l.issued {
		if issued.id != ticket {
			continue
		}
		if issued.epoch != l.state.Epoch {
			return turnTicketUnknownRun, nil
		}
		return issued.state, nil
	}
	return turnTicketAbsent, nil
}

// finishTurn ends the turn one lane was given, as that lane would: idle again,
// one more turn finished, and that round's receipt done.
func (f *fakeSchedulerAgent) finishTurn(lane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.at(lane)
	l.state.Idle = true
	l.state.Finished++
	for i := len(l.issued) - 1; i >= 0; i-- {
		if l.issued[i].epoch == l.state.Epoch {
			l.issued[i].state = turnTicketDone
			break
		}
	}
}

// restart is one lane coming back as a new run: a new epoch, an empty queue,
// and receipts that no longer name a turn of the run it is in.
func (f *fakeSchedulerAgent) restart(lane, epoch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at(lane).state = agentQueueState{Idle: true, Epoch: epoch}
}

func (f *fakeSchedulerAgent) writeCount(lane string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.at(lane).writes)
}

func (f *fakeSchedulerAgent) wrote(lane string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.at(lane).writes...)
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *testClock) set(t time.Time)         { c.t = t }

func newTestScheduler(t *testing.T) (*scheduler, *fakeSchedulerAgent, *testClock) {
	t.Helper()
	agent := newFakeSchedulerAgent("")
	clock := &testClock{t: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}
	s := newScheduler(agent, nil)
	s.now = clock.now
	n := 0
	s.newID = func() string { n++; return fmt.Sprintf("sch_%08d", n) }
	return s, agent, clock
}

func TestParseISODuration(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"PT30M":   30 * time.Minute,
		"PT2H":    2 * time.Hour,
		"P1D":     24 * time.Hour,
		"P1DT2H":  26 * time.Hour,
		"PT1M30S": 90 * time.Second,
	} {
		got, err := parseISODuration(raw)
		if err != nil || got != want {
			t.Errorf("parseISODuration(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "P", "PT", "30m", "tomorrow morning", "PT1.5H", "P1W", "P1Y", "-PT1M", "PT9999999M"} {
		if _, err := parseISODuration(raw); err == nil {
			t.Errorf("parseISODuration(%q) accepted", raw)
		}
	}
	if got := isoDuration(26*time.Hour + 90*time.Second); got != "P1DT2H1M30S" {
		t.Errorf("isoDuration = %q", got)
	}
}

func TestSchedulerToolLimits(t *testing.T) {
	s, _, _ := newTestScheduler(t)
	refused := []func() error{
		func() error {
			_, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "x", Delay: "in half an hour"})
			return err
		},
		func() error {
			_, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "x", Delay: "PT30S"})
			return err
		},
		func() error {
			_, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "x", Delay: "P8D"})
			return err
		},
		func() error {
			_, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "x", At: "2026-11-09T09:00:00"})
			return err
		},
		func() error {
			_, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "x", Delay: "PT5M", At: "2026-11-09T09:00:00Z"})
			return err
		},
		func() error {
			_, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "  ", Delay: "PT5M"})
			return err
		},
		func() error {
			_, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: strings.Repeat("a", schedulerMaxPromptBytes+1), Delay: "PT5M"})
			return err
		},
		func() error {
			_, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "x", Interval: "PT30S"})
			return err
		},
		func() error {
			_, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "x", Interval: "PT25H"})
			return err
		},
		func() error {
			_, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "x", Interval: "PT5M", FallbackInterval: "PT5M"})
			return err
		},
		func() error {
			_, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "x", ExpiresIn: "P8D"})
			return err
		},
	}
	for i, call := range refused {
		if err := call(); err == nil {
			t.Errorf("call %d accepted", i)
		}
	}
	if len(s.list(workCaller{})) != 0 {
		t.Fatalf("refused calls left schedules: %v", s.list(workCaller{}))
	}

	view, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "check the PR", At: "2026-11-09T09:30:00+09:00"})
	if err != nil {
		t.Fatal(err)
	}
	if view.NextRunAt != "2026-11-09T00:30:00Z" || view.Kind != "once" {
		t.Fatalf("once view = %+v", view)
	}
	fixed, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "poll", Interval: "PT5M"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.loopNext(workCaller{}, loopNextArgs{ID: fixed.ID, Delay: "PT2M"}); err == nil {
		t.Fatal("loop_next on a fixed loop accepted")
	}
	if _, err := s.loopNext(workCaller{}, loopNextArgs{ID: view.ID, Delay: "PT2M"}); err == nil {
		t.Fatal("loop_next on a once schedule accepted")
	}
	paced, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "watch", ExpiresIn: "PT1H"})
	if err != nil {
		t.Fatal(err)
	}
	if paced.Kind != "loop_self_paced" || paced.Fallback != "PT10M" || paced.ExpiresAt != "2026-11-09T01:00:00Z" {
		t.Fatalf("self-paced view = %+v", paced)
	}
	if _, err := s.loopNext(workCaller{}, loopNextArgs{ID: paced.ID, Delay: "PT2H"}); err == nil {
		t.Fatal("loop_next past the loop's expiry accepted")
	}

	for len(s.list(workCaller{})) < schedulerMaxSchedules {
		if _, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "fill", Interval: "PT1H"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "one too many", Interval: "PT1H"}); err == nil {
		t.Fatal("schedule past the per-session limit accepted")
	}
}

func TestSchedulerOnceRunsWhenIdleAndOnlyOnce(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "check the PR", Delay: "PT30M"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if agent.writeCount("") != 0 {
		t.Fatal("ran before its time")
	}
	clock.advance(30 * time.Minute)
	agent.at("").state.Idle = false // a person's turn is running
	s.tick(ctx)
	if agent.writeCount("") != 0 {
		t.Fatal("ran while the agent was busy")
	}
	agent.at("").state.Idle = true
	s.tick(ctx)
	if agent.writeCount("") != 1 {
		t.Fatalf("writes = %d, want 1", agent.writeCount(""))
	}
	if agent.wrote("")[0] != "check the PR" {
		t.Fatalf("run prompt = %q", agent.wrote("")[0])
	}
	if views := s.list(workCaller{}); len(views) != 1 || !views[0].Running {
		t.Fatalf("running schedule not listed: %+v", views)
	}
	s.tick(ctx) // turn still running
	agent.finishTurn("")
	s.tick(ctx)
	if len(s.list(workCaller{})) != 0 {
		t.Fatalf("once schedule left after its run: %+v", s.list(workCaller{}))
	}
	if active, _ := s.state(); active != 0 {
		t.Fatalf("active = %d", active)
	}
}

func TestSchedulerFixedLoopSkipsOverrunSlots(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "poll", Interval: "PT5M"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx) // first run right away
	if agent.writeCount("") != 1 {
		t.Fatalf("writes = %d", agent.writeCount(""))
	}
	if agent.wrote("")[0] != "poll" {
		t.Fatalf("run prompt = %q, want the schedule's prompt alone", agent.wrote("")[0])
	}
	clock.advance(17 * time.Minute) // the turn overran three slots
	agent.finishTurn("")
	s.tick(ctx)
	if got := s.list(workCaller{})[0].NextRunAt; got != "2026-11-09T00:20:00Z" {
		t.Fatalf("next run = %s, want the first slot after now", got)
	}
	s.tick(ctx)
	if agent.writeCount("") != 1 {
		t.Fatal("overrun slots were run")
	}
	clock.set(time.Date(2026, 11, 9, 0, 20, 0, 0, time.UTC))
	s.tick(ctx)
	if agent.writeCount("") != 2 || s.list(workCaller{})[0].Runs != 2 {
		t.Fatalf("second run missing: %v", agent.wrote(""))
	}
}

func TestSchedulerSelfPacedLoop(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	view, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "watch the rollout", FallbackInterval: "PT15M"})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if agent.wrote("")[0] != "watch the rollout" {
		t.Fatalf("run prompt = %q, want the schedule's prompt alone", agent.wrote("")[0])
	}
	// The run asks for two minutes, then finishes.
	clock.advance(time.Minute)
	if _, err := s.loopNext(workCaller{}, loopNextArgs{ID: view.ID, Delay: "PT2M"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	agent.finishTurn("")
	s.tick(ctx)
	if got := s.list(workCaller{})[0].NextRunAt; got != "2026-11-09T00:03:00Z" {
		t.Fatalf("next run = %s, want loop_next's time", got)
	}
	clock.set(time.Date(2026, 11, 9, 0, 3, 0, 0, time.UTC))
	s.tick(ctx)
	if agent.writeCount("") != 2 {
		t.Fatalf("writes = %d", agent.writeCount(""))
	}
	// This run calls nothing: the fallback interval applies from its end.
	clock.advance(4 * time.Minute)
	agent.finishTurn("")
	s.tick(ctx)
	if got := s.list(workCaller{})[0].NextRunAt; got != "2026-11-09T00:22:00Z" {
		t.Fatalf("next run = %s, want end + fallback", got)
	}
	if _, err := s.cancel(workCaller{}, scheduleIDArgs{ID: view.ID}); err != nil {
		t.Fatal(err)
	}
	if len(s.list(workCaller{})) != 0 {
		t.Fatal("cancel left the loop")
	}
}

func TestSchedulerRunEndsOnAgentRestartAndRetriesFailedWrite(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "poll", Interval: "PT5M"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	agent.restart("", "e2") // restarted, queue lost
	s.tick(ctx)
	if len(s.running) != 0 {
		t.Fatal("run outlived the agent restart")
	}

	clock.advance(5 * time.Minute)
	agent.at("").writeErr = errors.New("agent write: 409 checkpoint in progress")
	s.tick(ctx)
	if len(s.running) != 0 || s.list(workCaller{})[0].Runs != 1 {
		t.Fatalf("failed write left state: running=%d runs=%d", len(s.running), s.list(workCaller{})[0].Runs)
	}
	agent.at("").writeErr = nil
	s.tick(ctx)
	if agent.writeCount("") != 1 {
		t.Fatal("retried before the retry delay")
	}
	clock.advance(schedulerRetryDelay)
	s.tick(ctx)
	if agent.writeCount("") != 2 {
		t.Fatalf("writes = %d after the retry delay", agent.writeCount(""))
	}
}

func TestSchedulerLoopExpires(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.loopCreate(workCaller{}, loopCreateArgs{Prompt: "poll", Interval: "PT40M", ExpiresIn: "PT1H"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	agent.finishTurn("")
	clock.advance(time.Minute)
	s.tick(ctx) // next slot 00:40 is before expiry 01:00
	if len(s.list(workCaller{})) != 1 {
		t.Fatal("loop ended early")
	}
	clock.set(time.Date(2026, 11, 9, 0, 40, 0, 0, time.UTC))
	s.tick(ctx)
	agent.finishTurn("")
	clock.advance(time.Minute)
	s.tick(ctx) // next slot 01:20 is past expiry
	if len(s.list(workCaller{})) != 0 {
		t.Fatalf("loop outlived its expiry: %+v", s.list(workCaller{}))
	}
}

func rpc(t *testing.T, h http.Handler, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, schedulerMCPPath, strings.NewReader(string(body))))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", method, err, rec.Body.String())
	}
	return out
}

func TestSchedulerMCPEndpoint(t *testing.T) {
	s, _, _ := newTestScheduler(t)
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := withScheduler(fallback, s)

	list := rpc(t, h, "tools/list", nil)
	var names []string
	for _, tool := range list["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "schedule_once,loop_create,loop_next,schedule_list,schedule_cancel" {
		t.Fatalf("tools = %v", names)
	}

	ok := rpc(t, h, "tools/call", map[string]any{"name": "loop_create", "arguments": map[string]any{"prompt": "poll", "interval": "PT5M"}})
	result := ok["result"].(map[string]any)
	if result["isError"] != false || !strings.Contains(result["content"].([]any)[0].(map[string]any)["text"].(string), `"nextRunAt":"2026-11-09T00:00:00Z"`) {
		t.Fatalf("loop_create result = %v", result)
	}
	refused := rpc(t, h, "tools/call", map[string]any{"name": "schedule_once", "arguments": map[string]any{"prompt": "x", "delay": "half an hour"}})
	if refused["result"].(map[string]any)["isError"] != true {
		t.Fatalf("natural-language delay not refused: %v", refused)
	}
	malformed := rpc(t, h, "tools/call", map[string]any{"name": "schedule_once", "arguments": map[string]any{"prompt": "x", "when": "PT5M"}})
	if malformed["error"] == nil {
		t.Fatalf("unknown argument not a JSON-RPC error: %v", malformed)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, schedulerStatePath, nil))
	if strings.TrimSpace(rec.Body.String()) != `{"active":1,"nextRunAt":"2026-11-09T00:00:00Z"}` {
		t.Fatalf("state = %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatal("other paths did not reach the sidecar's handler")
	}
}

func TestAgentQueueAndSchedulerRoutes(t *testing.T) {
	c := newTestClaudeWorkload(t)
	routes := claudeRoutes(testLogger(), c, nil)

	req := httptest.NewRequest(http.MethodGet, agentQueuePath, nil)
	req.RemoteAddr = "10.0.0.5:4000"
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("queue state from the pod network = %d", rec.Code)
	}
	req.RemoteAddr = "127.0.0.1:4000"
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	var state agentQueueState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil || !state.Idle || state.Epoch != agentEpoch {
		t.Fatalf("queue state = %s (%v)", rec.Body.String(), err)
	}

	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, agentSchedulerPath, nil))
	if strings.TrimSpace(rec.Body.String()) != `{"active":0}` {
		t.Fatalf("scheduler count without a scheduler = %s", rec.Body.String())
	}

	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != schedulerStatePath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"active":3}`))
	}))
	defer sidecar.Close()
	c.cli = codexCLI{schedulerURL: sidecar.URL + schedulerMCPPath}
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, agentSchedulerPath, nil))
	if strings.TrimSpace(rec.Body.String()) != `{"active":3}` {
		t.Fatalf("relayed scheduler count = %s", rec.Body.String())
	}
	sidecar.Close()
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, agentSchedulerPath, nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable sidecar = %d", rec.Code)
	}
}

func TestSchedulerURLsFromEnv(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	if got, err := schedulerMCPURLFromEnv(env("")); got != "" || err != nil {
		t.Fatalf("unset = %q, %v", got, err)
	}
	if got, err := schedulerMCPURLFromEnv(env("http://127.0.0.1:8093/scheduler/mcp")); err != nil || schedulerStateURLFor(got) != "http://127.0.0.1:8093/scheduler/state" {
		t.Fatalf("loopback = %q, %v", got, err)
	}
	for _, bad := range []string{"http://10.0.0.1:8093/scheduler/mcp", "http://127.0.0.1:8093/mcp"} {
		if _, err := schedulerMCPURLFromEnv(env(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, bad := range []string{"http://10.0.0.1:8090", "https://127.0.0.1:8090", ""} {
		if _, err := newHTTPSchedulerAgent(bad, false); err == nil {
			t.Errorf("agent URL %q accepted", bad)
		}
	}
}

func TestRenderCodexConfigRegistersScheduler(t *testing.T) {
	for _, task := range []bool{false, true} {
		doc := string(renderCodexConfig(codexConfig{task: task, agentBinary: "/agent", skillsDir: "/s",
			schedulerURL: "http://127.0.0.1:8093/scheduler/mcp"}))
		if !strings.Contains(doc, "[mcp_servers.scheduler]\nurl = \"http://127.0.0.1:8093/scheduler/mcp\"") {
			t.Fatalf("task=%v config lacks the scheduler:\n%s", task, doc)
		}
		if strings.Contains(string(renderCodexConfig(codexConfig{task: task, agentBinary: "/agent", skillsDir: "/s"})), "[mcp_servers.scheduler]") {
			t.Fatalf("task=%v config registers a scheduler it was not given", task)
		}
	}
}

func newTestClaudeWorkload(t *testing.T) *claudeWorkload {
	t.Helper()
	c, _ := newClaudeTestServer(t, newFakeClaudeRunner(), "m", false, nil)
	return c
}

// AC-P2·AC-P4 end to end on a real queue: the sidecar's round goes through
// POST /write as a turn of that conversation, and the round is settled by the
// receipt that write returned (turn_ticket.go) rather than by counting turns.
func TestSchedulerRunsThroughTheAgentQueue(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, "m", false, nil)
	agent, err := newHTTPSchedulerAgent(srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	s := newScheduler(agent, testLogger())
	clock := &testClock{t: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}
	s.now = clock.now
	if _, err := s.scheduleOnce(workCaller{}, scheduleOnceArgs{Prompt: "check the PR", Delay: "PT1M"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	ctx := context.Background()
	s.tick(ctx)
	run := <-runner.started
	if prompt := run.argv[len(run.argv)-1]; prompt != "check the PR" {
		t.Fatalf("turn prompt = %q", prompt)
	}
	eventually(t, "the round settles on its receipt", func() bool {
		s.tick(ctx)
		return len(s.list(workCaller{})) == 0
	})
	// The receipt is what settled it — a turn that ended, which the lane's own
	// counters then record. Both are asserted: the round was one turn of this
	// conversation, and it is over.
	eventually(t, "the lane records the finished turn", func() bool {
		got := c.queueState()
		return got.Idle && got.Finished == 1
	})
}

// newPairedTestScheduler is a scheduler on a pod that runs both lanes, with a
// caller for each — the identity the lane's own address carries (AC-P4).
func newPairedTestScheduler(t *testing.T) (*scheduler, *fakeSchedulerAgent, *testClock, workCaller, workCaller) {
	t.Helper()
	agent := newFakeSchedulerAgent(laneParent, laneMonitor)
	clock := &testClock{t: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}
	s := newScheduler(agent, testLogger())
	s.now = clock.now
	n := 0
	s.newID = func() string { n++; return fmt.Sprintf("sch_%08d", n) }
	return s, agent, clock, laneCaller(laneParent, "parent-run-1"), laneCaller(laneMonitor, "monitor-run-1")
}

// TestEachLanesSchedulesRunInThatLane is the destination rule and what follows
// from it: the registration's own address decides where a round lands, and the
// lanes wait on each other for nothing (AC-P4).
func TestEachLanesSchedulesRunInThatLane(t *testing.T) {
	s, agent, clock, parent, monitor := newPairedTestScheduler(t)
	ctx := context.Background()
	if _, err := s.scheduleOnce(monitor, scheduleOnceArgs{Prompt: "check the rollout", Delay: "PT30M"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.scheduleOnce(parent, scheduleOnceArgs{Prompt: "say the daily summary", Delay: "PT30M"}); err != nil {
		t.Fatal(err)
	}

	// The person is mid-conversation: the parent is busy, the monitor is not.
	agent.at(laneParent).state.Idle = false
	clock.advance(30 * time.Minute)
	s.tick(ctx)
	if got := agent.wrote(laneMonitor); len(got) != 1 || got[0] != "check the rollout" {
		t.Fatalf("the monitor's round = %v, want the one prompt it registered", got)
	}
	if got := agent.writeCount(laneParent); got != 0 {
		t.Fatalf("the parent took %d rounds while it was busy with a person", got)
	}

	// Ticking again delivers neither round twice: the monitor holds one round,
	// and the parent is still busy.
	s.tick(ctx)
	if got := agent.writeCount(laneMonitor); got != 1 {
		t.Fatalf("the monitor's round was delivered %d times", got)
	}

	// The person's turn ends; the parent's own round goes to the parent.
	agent.at(laneParent).state.Idle = true
	s.tick(ctx)
	if got := agent.wrote(laneParent); len(got) != 1 || got[0] != "say the daily summary" {
		t.Fatalf("the parent's round = %v", got)
	}
	// A round in flight on one lane does not hold the other: both are running.
	if len(s.running) != 2 {
		t.Fatalf("rounds in flight = %d, want one per lane", len(s.running))
	}
	agent.finishTurn(laneMonitor)
	s.tick(ctx)
	if len(s.list(monitor)) != 0 {
		t.Fatalf("the monitor's once schedule outlived its round: %+v", s.list(monitor))
	}
	if len(s.list(parent)) != 1 || !s.list(parent)[0].Running {
		t.Fatalf("the parent's round was settled by the monitor's: %+v", s.list(parent))
	}
}

// TestALaneRefusingARoundDoesNotPauseTheOther keeps the retry per lane: the
// monitor's queue being full is not a reason the person's chat waits (AC-P4).
func TestALaneRefusingARoundDoesNotPauseTheOther(t *testing.T) {
	s, agent, clock, parent, monitor := newPairedTestScheduler(t)
	ctx := context.Background()
	agent.at(laneMonitor).writeErr = errors.New("agent write: 429 queue full")
	if _, err := s.loopCreate(monitor, loopCreateArgs{Prompt: "watch", Interval: "PT5M"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loopCreate(parent, loopCreateArgs{Prompt: "greet", Interval: "PT5M"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if got := agent.writeCount(laneParent); got != 1 {
		t.Fatalf("the parent's round = %d, want one despite the monitor's refusal", got)
	}
	if len(s.running) != 1 || s.running[laneParent] == nil {
		t.Fatalf("a refused round was left in flight: %v", s.running)
	}
	if s.list(monitor)[0].Runs != 0 {
		t.Fatal("a refused round was counted as a run")
	}

	// The refusal holds that lane back for the retry delay and no longer.
	agent.at(laneMonitor).writeErr = nil
	s.tick(ctx)
	if got := agent.writeCount(laneMonitor); got != 0 {
		t.Fatal("retried before the retry delay")
	}
	clock.advance(schedulerRetryDelay)
	s.tick(ctx)
	if got := agent.writeCount(laneMonitor); got != 1 {
		t.Fatalf("the monitor's round never came back: writes = %d", got)
	}
}

// TestOnlyTheOwningLanePacesOrCancelsASchedule is the ownership the address
// gives: a lane lists, paces and cancels its own schedules and is refused the
// other lane's by name (AC-P4).
func TestOnlyTheOwningLanePacesOrCancelsASchedule(t *testing.T) {
	s, _, _, parent, monitor := newPairedTestScheduler(t)
	view, err := s.loopCreate(monitor, loopCreateArgs{Prompt: "watch the rollout", FallbackInterval: "PT15M"})
	if err != nil {
		t.Fatal(err)
	}
	// Each lane sees its own and only its own: a schedule is a prompt that
	// becomes a turn of that lane's conversation.
	if got := s.list(monitor); len(got) != 1 || got[0].ID != view.ID {
		t.Fatalf("the monitor's list = %+v", got)
	}
	if got := s.list(parent); len(got) != 0 {
		t.Fatalf("the parent can see the monitor's schedules: %+v", got)
	}
	if got := s.list(workCaller{}); len(got) != 0 {
		t.Fatalf("a single-lane caller can see a lane's schedules: %+v", got)
	}
	for _, call := range []struct {
		what string
		err  error
	}{
		{"loop_next", func() error { _, err := s.loopNext(parent, loopNextArgs{ID: view.ID, Delay: "PT2M"}); return err }()},
		{"schedule_cancel", func() error { _, err := s.cancel(parent, scheduleIDArgs{ID: view.ID}); return err }()},
	} {
		if call.err == nil {
			t.Fatalf("the parent was allowed to %s the monitor's schedule", call.what)
		}
		if !strings.Contains(call.err.Error(), laneMonitor) {
			t.Errorf("%s refusal does not name the lane that owns it: %v", call.what, call.err)
		}
	}
	if len(s.list(monitor)) != 1 {
		t.Fatal("the other lane's refused calls changed the schedule")
	}
}

// TestARoundIsPacedByTheRunThatOwnsIt is the compare-and-swap on the round: a
// leftover of a conversation that is gone cannot move a live loop's next run,
// and the run that holds the round can — with the same schedule id (AC-P4).
func TestARoundIsPacedByTheRunThatOwnsIt(t *testing.T) {
	s, agent, clock, _, monitor := newPairedTestScheduler(t)
	ctx := context.Background()
	view, err := s.loopCreate(monitor, loopCreateArgs{Prompt: "watch", FallbackInterval: "PT15M"})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if agent.writeCount(laneMonitor) != 1 {
		t.Fatal("the loop's first round never ran")
	}

	stale := laneCaller(laneMonitor, "monitor-run-0")
	if _, err := s.loopNext(stale, loopNextArgs{ID: view.ID, Delay: "PT2M"}); err == nil {
		t.Fatal("an earlier run of the lane paced the round it no longer holds")
	}
	// Refusing it changed nothing: the next run is still the round's to set.
	if _, err := s.loopNext(monitor, loopNextArgs{ID: view.ID, Delay: "PT2M"}); err != nil {
		t.Fatal(err)
	}
	agent.finishTurn(laneMonitor)
	clock.advance(time.Minute)
	s.tick(ctx)
	if got := s.list(monitor)[0].NextRunAt; got != "2026-11-09T00:02:00Z" {
		t.Fatalf("next run = %s, want the time the round itself set", got)
	}
	// With no round in flight, the lane that owns the schedule paces it even
	// from a later run of itself: a restarted monitor is still this monitor.
	later := laneCaller(laneMonitor, "monitor-run-2")
	if _, err := s.loopNext(later, loopNextArgs{ID: view.ID, Delay: "PT5M"}); err != nil {
		t.Fatalf("the lane could not pace its own loop after restarting: %v", err)
	}
	if _, err := s.cancel(later, scheduleIDArgs{ID: view.ID}); err != nil {
		t.Fatalf("the lane could not end its own loop after restarting: %v", err)
	}
	if len(s.list(monitor)) != 0 {
		t.Fatal("cancel left the loop")
	}
}

// TestARoundEndsWhenTheLaneItRanInRestarts is the recovery: the receipt no
// longer names a turn of the lane's current run, so the round is over rather
// than waited on forever, and the loop keeps its id and runs again (AC-P4).
func TestARoundEndsWhenTheLaneItRanInRestarts(t *testing.T) {
	s, agent, clock, _, monitor := newPairedTestScheduler(t)
	ctx := context.Background()
	view, err := s.loopCreate(monitor, loopCreateArgs{Prompt: "watch", Interval: "PT5M"})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if s.running[laneMonitor] == nil {
		t.Fatal("the first round never started")
	}
	s.tick(ctx)
	if s.running[laneMonitor] == nil {
		t.Fatal("a round still queued on the lane was settled")
	}

	agent.restart(laneMonitor, "e2") // the monitor came back as a new conversation
	s.tick(ctx)
	if s.running[laneMonitor] != nil {
		t.Fatal("the round outlived the lane it was queued on")
	}
	if got := s.list(monitor); len(got) != 1 || got[0].ID != view.ID {
		t.Fatalf("the loop did not survive its lane restarting: %+v", got)
	}
	clock.advance(5 * time.Minute)
	s.tick(ctx)
	if got := agent.writeCount(laneMonitor); got != 2 {
		t.Fatalf("rounds delivered = %d, want the next one after the restart", got)
	}
}

// TestARoundIsSettledFromTheQueueWhenALaneIssuesNoReceipt keeps the way this
// always worked for a lane too old to issue receipts: idle again with a turn
// more finished.
func TestARoundIsSettledFromTheQueueWhenALaneIssuesNoReceipt(t *testing.T) {
	s, agent, _, _, monitor := newPairedTestScheduler(t)
	ctx := context.Background()
	agent.at(laneMonitor).noTicket = true
	if _, err := s.scheduleOnce(monitor, scheduleOnceArgs{Prompt: "check", Delay: "PT1M"}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 11, 9, 0, 2, 0, 0, time.UTC) }
	s.tick(ctx)
	if s.running[laneMonitor] == nil || s.running[laneMonitor].ticket != "" {
		t.Fatalf("round = %+v, want one in flight without a receipt", s.running[laneMonitor])
	}
	s.tick(ctx)
	if s.running[laneMonitor] == nil {
		t.Fatal("the round was settled while the lane was still busy")
	}
	agent.finishTurn(laneMonitor)
	s.tick(ctx)
	if len(s.list(monitor)) != 0 {
		t.Fatalf("the round never settled: %+v", s.list(monitor))
	}
}

// TestAMonitorScheduleIsRefusedWithoutAMonitorLane refuses rather than falling
// back to the one conversation there is: a round that appeared in the person's
// chat because the monitor was not there is the mix-up the address prevents.
func TestAMonitorScheduleIsRefusedWithoutAMonitorLane(t *testing.T) {
	s, agent, _ := newTestScheduler(t) // a session with one conversation
	monitor := laneCaller(laneMonitor, "monitor-run-1")
	if _, err := s.scheduleOnce(monitor, scheduleOnceArgs{Prompt: "check", Delay: "PT30M"}); err == nil {
		t.Fatal("a monitor's schedule was accepted by a pod with no monitor lane")
	}
	if _, err := s.loopCreate(monitor, loopCreateArgs{Prompt: "watch", Interval: "PT5M"}); err == nil {
		t.Fatal("a monitor's loop was accepted by a pod with no monitor lane")
	}
	if len(s.list(monitor)) != 0 || len(s.list(workCaller{})) != 0 {
		t.Fatal("a refused registration left a schedule")
	}
	// The parent's address is the chat either way, so a manager that never
	// turned the pair on still takes its own schedules.
	if !agent.reaches(laneParent) {
		t.Fatal("the parent lane is not reachable")
	}
	if _, err := s.scheduleOnce(laneCaller(laneParent, "parent-run-1"),
		scheduleOnceArgs{Prompt: "greet", Delay: "PT30M"}); err != nil {
		t.Fatalf("a parent's schedule was refused: %v", err)
	}
}

// TestSchedulerLaneAddresses is the address itself: what one lane registers,
// what the state path beside it is, and that a malformed lane address is
// refused by name rather than falling through to the proxy behind it.
func TestSchedulerLaneAddresses(t *testing.T) {
	const base = "http://127.0.0.1:8093" + schedulerMCPPath
	laneURL, err := schedulerLaneMCPURL(base, laneMonitor, "monitor-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if laneURL != "http://127.0.0.1:8093/scheduler/mcp/lane/monitor/monitor-run-1" {
		t.Fatalf("lane address = %s", laneURL)
	}
	// AC-P3's count is the pod's, and the address it is read from has to be
	// derived from a lane address too — not by trimming a path that is no
	// longer at the end.
	if got := schedulerStateURLFor(laneURL); got != "http://127.0.0.1:8093"+schedulerStatePath {
		t.Fatalf("state URL from a lane address = %s", got)
	}
	if got := schedulerStateURLFor(base); got != "http://127.0.0.1:8093"+schedulerStatePath {
		t.Fatalf("state URL from the single address = %s", got)
	}
	if _, err := schedulerLaneMCPURL("", laneMonitor, "monitor-run-1"); err != nil {
		t.Fatalf("an unset address = %v", err)
	}
	for _, bad := range []struct{ raw, lane, incarnation string }{
		{"http://127.0.0.1:8093/mcp", laneMonitor, "monitor-run-1"},
		{base, "chat", "monitor-run-1"},
		{base, laneMonitor, "monitor run 1"},
		{base, laneMonitor, ""},
	} {
		if got, err := schedulerLaneMCPURL(bad.raw, bad.lane, bad.incarnation); err == nil {
			t.Errorf("%+v accepted as %s", bad, got)
		}
	}

	s, _, _, _, monitor := newPairedTestScheduler(t)
	handler := withScheduler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), s)
	call := func(path string, params any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
		return rec
	}
	rec := call(laneMCPPath(schedulerLanePathPrefix, laneMonitor, "monitor-run-1"),
		map[string]any{"name": "schedule_once", "arguments": map[string]any{"prompt": "check", "delay": "PT30M"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sch_") {
		t.Fatalf("a lane's registration = %d %s", rec.Code, rec.Body.String())
	}
	if got := s.list(monitor); len(got) != 1 {
		t.Fatalf("the lane's registration did not land as the lane's: %+v", got)
	}
	for _, bad := range []string{
		schedulerLanePathPrefix,
		schedulerLanePathPrefix + "/monitor",
		schedulerLanePathPrefix + "/chat/monitor-run-1",
		schedulerLanePathPrefix + "/monitor/monitor.run.1",
		schedulerLanePathPrefix + "/monitor/monitor-run-1/extra",
	} {
		if rec := call(bad, map[string]any{"name": "schedule_list"}); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want a refusal by name rather than the proxy behind it", bad, rec.Code)
		}
	}
}
