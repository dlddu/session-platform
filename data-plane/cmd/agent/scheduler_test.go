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

type fakeSchedulerAgent struct {
	mu       sync.Mutex
	state    agentQueueState
	writes   []string
	writeErr error
}

func (f *fakeSchedulerAgent) queueState(context.Context) (agentQueueState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeSchedulerAgent) write(_ context.Context, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, prompt)
	f.state.Idle = false
	return nil
}

// finishTurn marks the queued run done, as the agent would after its turn.
func (f *fakeSchedulerAgent) finishTurn() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Idle = true
	f.state.Finished++
}

func (f *fakeSchedulerAgent) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *testClock) set(t time.Time)         { c.t = t }

func newTestScheduler(t *testing.T) (*scheduler, *fakeSchedulerAgent, *testClock) {
	t.Helper()
	agent := &fakeSchedulerAgent{state: agentQueueState{Idle: true, Epoch: "e1"}}
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
			_, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "x", Delay: "in half an hour"})
			return err
		},
		func() error { _, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "x", Delay: "PT30S"}); return err },
		func() error { _, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "x", Delay: "P8D"}); return err },
		func() error {
			_, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "x", At: "2026-11-09T09:00:00"})
			return err
		},
		func() error {
			_, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "x", Delay: "PT5M", At: "2026-11-09T09:00:00Z"})
			return err
		},
		func() error { _, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "  ", Delay: "PT5M"}); return err },
		func() error {
			_, err := s.scheduleOnce(scheduleOnceArgs{Prompt: strings.Repeat("a", schedulerMaxPromptBytes+1), Delay: "PT5M"})
			return err
		},
		func() error { _, err := s.loopCreate(loopCreateArgs{Prompt: "x", Interval: "PT30S"}); return err },
		func() error { _, err := s.loopCreate(loopCreateArgs{Prompt: "x", Interval: "PT25H"}); return err },
		func() error {
			_, err := s.loopCreate(loopCreateArgs{Prompt: "x", Interval: "PT5M", FallbackInterval: "PT5M"})
			return err
		},
		func() error { _, err := s.loopCreate(loopCreateArgs{Prompt: "x", ExpiresIn: "P8D"}); return err },
	}
	for i, call := range refused {
		if err := call(); err == nil {
			t.Errorf("call %d accepted", i)
		}
	}
	if len(s.list()) != 0 {
		t.Fatalf("refused calls left schedules: %v", s.list())
	}

	view, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "check the PR", At: "2026-11-09T09:30:00+09:00"})
	if err != nil {
		t.Fatal(err)
	}
	if view.NextRunAt != "2026-11-09T00:30:00Z" || view.Kind != "once" {
		t.Fatalf("once view = %+v", view)
	}
	fixed, err := s.loopCreate(loopCreateArgs{Prompt: "poll", Interval: "PT5M"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.loopNext(loopNextArgs{ID: fixed.ID, Delay: "PT2M"}); err == nil {
		t.Fatal("loop_next on a fixed loop accepted")
	}
	if _, err := s.loopNext(loopNextArgs{ID: view.ID, Delay: "PT2M"}); err == nil {
		t.Fatal("loop_next on a once schedule accepted")
	}
	paced, err := s.loopCreate(loopCreateArgs{Prompt: "watch", ExpiresIn: "PT1H"})
	if err != nil {
		t.Fatal(err)
	}
	if paced.Kind != "loop_self_paced" || paced.Fallback != "PT10M" || paced.ExpiresAt != "2026-11-09T01:00:00Z" {
		t.Fatalf("self-paced view = %+v", paced)
	}
	if _, err := s.loopNext(loopNextArgs{ID: paced.ID, Delay: "PT2H"}); err == nil {
		t.Fatal("loop_next past the loop's expiry accepted")
	}

	for len(s.list()) < schedulerMaxSchedules {
		if _, err := s.loopCreate(loopCreateArgs{Prompt: "fill", Interval: "PT1H"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.loopCreate(loopCreateArgs{Prompt: "one too many", Interval: "PT1H"}); err == nil {
		t.Fatal("schedule past the per-session limit accepted")
	}
}

func TestSchedulerOnceRunsWhenIdleAndOnlyOnce(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "check the PR", Delay: "PT30M"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if agent.writeCount() != 0 {
		t.Fatal("ran before its time")
	}
	clock.advance(30 * time.Minute)
	agent.state.Idle = false // a person's turn is running
	s.tick(ctx)
	if agent.writeCount() != 0 {
		t.Fatal("ran while the agent was busy")
	}
	agent.state.Idle = true
	s.tick(ctx)
	if agent.writeCount() != 1 {
		t.Fatalf("writes = %d, want 1", agent.writeCount())
	}
	if !strings.HasPrefix(agent.writes[0], "[session-platform scheduler] one-time schedule sch_00000001") ||
		!strings.HasSuffix(agent.writes[0], "\ncheck the PR") {
		t.Fatalf("run prompt = %q", agent.writes[0])
	}
	if views := s.list(); len(views) != 1 || !views[0].Running {
		t.Fatalf("running schedule not listed: %+v", views)
	}
	s.tick(ctx) // turn still running
	agent.finishTurn()
	s.tick(ctx)
	if len(s.list()) != 0 {
		t.Fatalf("once schedule left after its run: %+v", s.list())
	}
	if active, _ := s.state(); active != 0 {
		t.Fatalf("active = %d", active)
	}
}

func TestSchedulerFixedLoopSkipsOverrunSlots(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	view, err := s.loopCreate(loopCreateArgs{Prompt: "poll", Interval: "PT5M"})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx) // first run right away
	if agent.writeCount() != 1 {
		t.Fatalf("writes = %d", agent.writeCount())
	}
	if !strings.Contains(agent.writes[0], "loop "+view.ID+" run 1 — runs every PT5M") {
		t.Fatalf("header = %q", agent.writes[0])
	}
	clock.advance(17 * time.Minute) // the turn overran three slots
	agent.finishTurn()
	s.tick(ctx)
	if got := s.list()[0].NextRunAt; got != "2026-11-09T00:20:00Z" {
		t.Fatalf("next run = %s, want the first slot after now", got)
	}
	s.tick(ctx)
	if agent.writeCount() != 1 {
		t.Fatal("overrun slots were run")
	}
	clock.set(time.Date(2026, 11, 9, 0, 20, 0, 0, time.UTC))
	s.tick(ctx)
	if agent.writeCount() != 2 || !strings.Contains(agent.writes[1], " run 2 ") {
		t.Fatalf("second run missing: %v", agent.writes)
	}
}

func TestSchedulerSelfPacedLoop(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	view, err := s.loopCreate(loopCreateArgs{Prompt: "watch the rollout", FallbackInterval: "PT15M"})
	if err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	if !strings.Contains(agent.writes[0], "call loop_next with id "+view.ID) {
		t.Fatalf("self-paced header = %q", agent.writes[0])
	}
	// The run asks for two minutes, then finishes.
	clock.advance(time.Minute)
	if _, err := s.loopNext(loopNextArgs{ID: view.ID, Delay: "PT2M"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	agent.finishTurn()
	s.tick(ctx)
	if got := s.list()[0].NextRunAt; got != "2026-11-09T00:03:00Z" {
		t.Fatalf("next run = %s, want loop_next's time", got)
	}
	clock.set(time.Date(2026, 11, 9, 0, 3, 0, 0, time.UTC))
	s.tick(ctx)
	if agent.writeCount() != 2 {
		t.Fatalf("writes = %d", agent.writeCount())
	}
	// This run calls nothing: the fallback interval applies from its end.
	clock.advance(4 * time.Minute)
	agent.finishTurn()
	s.tick(ctx)
	if got := s.list()[0].NextRunAt; got != "2026-11-09T00:22:00Z" {
		t.Fatalf("next run = %s, want end + fallback", got)
	}
	if _, err := s.cancel(scheduleIDArgs{ID: view.ID}); err != nil {
		t.Fatal(err)
	}
	if len(s.list()) != 0 {
		t.Fatal("cancel left the loop")
	}
}

func TestSchedulerRunEndsOnAgentRestartAndRetriesFailedWrite(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.loopCreate(loopCreateArgs{Prompt: "poll", Interval: "PT5M"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	agent.state = agentQueueState{Idle: true, Finished: 0, Epoch: "e2"} // restarted, queue lost
	s.tick(ctx)
	if s.running != nil {
		t.Fatal("run outlived the agent restart")
	}

	clock.advance(5 * time.Minute)
	agent.writeErr = errors.New("agent write: 409 checkpoint in progress")
	s.tick(ctx)
	if s.running != nil || s.list()[0].Runs != 1 {
		t.Fatalf("failed write left state: running=%v runs=%d", s.running, s.list()[0].Runs)
	}
	agent.writeErr = nil
	s.tick(ctx)
	if agent.writeCount() != 1 {
		t.Fatal("retried before the retry delay")
	}
	clock.advance(schedulerRetryDelay)
	s.tick(ctx)
	if agent.writeCount() != 2 {
		t.Fatalf("writes = %d after the retry delay", agent.writeCount())
	}
}

func TestSchedulerLoopExpires(t *testing.T) {
	s, agent, clock := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.loopCreate(loopCreateArgs{Prompt: "poll", Interval: "PT40M", ExpiresIn: "PT1H"}); err != nil {
		t.Fatal(err)
	}
	s.tick(ctx)
	agent.finishTurn()
	clock.advance(time.Minute)
	s.tick(ctx) // next slot 00:40 is before expiry 01:00
	if len(s.list()) != 1 {
		t.Fatal("loop ended early")
	}
	clock.set(time.Date(2026, 11, 9, 0, 40, 0, 0, time.UTC))
	s.tick(ctx)
	agent.finishTurn()
	clock.advance(time.Minute)
	s.tick(ctx) // next slot 01:20 is past expiry
	if len(s.list()) != 0 {
		t.Fatalf("loop outlived its expiry: %+v", s.list())
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
	routes := claudeRoutes(testLogger(), c)

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
		if _, err := newHTTPSchedulerAgent(bad); err == nil {
			t.Errorf("agent URL %q accepted", bad)
		}
	}
}

func TestRenderCodexConfigRegistersScheduler(t *testing.T) {
	for _, task := range []bool{false, true} {
		doc := string(renderCodexConfig(codexConfig{task: task, agentBinary: "/agent", skillsDir: "/s",
			schedulerURL: "http://127.0.0.1:8093/scheduler/mcp"}))
		if !strings.Contains(doc, "[mcp_servers.scheduler]\nurl = \"http://127.0.0.1:8093/scheduler/mcp\"") ||
			!strings.Contains(doc, "use the `scheduler` MCP server") {
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

// AC-P2 end to end on a real queue: the sidecar's run goes through POST
// /write as a turn, and the run is settled from GET /queue once it finished.
func TestSchedulerRunsThroughTheAgentQueue(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, "m", false, nil)
	agent, err := newHTTPSchedulerAgent(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := newScheduler(agent, testLogger())
	clock := &testClock{t: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)}
	s.now = clock.now
	if _, err := s.scheduleOnce(scheduleOnceArgs{Prompt: "check the PR", Delay: "PT1M"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	ctx := context.Background()
	s.tick(ctx)
	run := <-runner.started
	if prompt := run.argv[len(run.argv)-1]; !strings.HasPrefix(prompt, "[session-platform scheduler] one-time schedule") ||
		!strings.HasSuffix(prompt, "\ncheck the PR") {
		t.Fatalf("turn prompt = %q", prompt)
	}
	eventually(t, "the run settles", func() bool {
		s.tick(ctx)
		return len(s.list()) == 0
	})
	if got := c.queueState(); !got.Idle || got.Finished != 1 {
		t.Fatalf("queue state = %+v", got)
	}
}
