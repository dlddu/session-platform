package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The scheduler's destination on a pod that runs both lanes (AC-P4), on the
// normal path with nothing mocked between the pieces: two real lane workloads
// behind one agent port, the real sidecar serving the real scheduler MCP and
// the real slot database at the real lane addresses, and the real loopback hop
// the sidecar makes to put a round on a lane's own queue.
//
// What these tests are for is the thing a count of writes cannot show: that a
// monitor's watching reaches the monitor *and nothing else* — no turn in the
// person's chat, and not by way of the parent being asked to pass it on.

// schedulerPairFixture is one such pod.
type schedulerPairFixture struct {
	sched   *scheduler
	clock   *testClock
	db      *sessionDB
	parent  *claudeWorkload
	monitor *claudeWorkload
	srv     *httptest.Server
	// parentMCP and monitorMCP are the scheduler addresses the two lanes have
	// in their managed config — derived the way the platform derives them.
	parentMCP, monitorMCP string
	// monitorTurns carries each prompt the monitor's conversation was given.
	monitorTurns chan string
	// parentTurns carries the same for the person's chat, which these tests
	// expect to stay empty.
	parentTurns chan string
}

func newSchedulerPairFixture(t *testing.T, onMonitorTurn func(prompt string)) *schedulerPairFixture {
	t.Helper()
	fixture := &schedulerPairFixture{
		monitorTurns: make(chan string, 4),
		parentTurns:  make(chan string, 4),
		clock:        &testClock{t: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)},
	}
	db, err := newSessionDB(t.TempDir(), false, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fixture.db = db

	runner := &lanePairRunner{
		// The parent's turns are recorded rather than refused, so a round that
		// landed in the person's chat is reported as what it is. What it was
		// given is read from that lane's own buffer.
		onParent: func(runnerOptions) { fixture.parentTurns <- "a turn ran in the person's chat" },
		onMonitor: func(prompt string, _ runnerOptions) {
			if onMonitorTurn != nil {
				onMonitorTurn(prompt)
			}
			fixture.monitorTurns <- prompt
		},
	}
	parent, monitor, srv := newLanePairTestServer(t, runner)
	runner.monitorDir = monitor.stateDir
	fixture.parent, fixture.monitor, fixture.srv = parent, monitor, srv

	agent, err := newHTTPSchedulerAgent(srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	fixture.sched = newScheduler(agent, testLogger())
	fixture.sched.now = fixture.clock.now

	// The sidecar as it really is: the scheduler in front of the slot database,
	// with the handover wired to the monitor lane. The handover being available
	// is the point — these tests assert it was never used.
	sidecar := httptest.NewServer(withScheduler(withSessionDBMCP(http.NotFoundHandler(), db), fixture.sched))
	t.Cleanup(sidecar.Close)
	db.monitorLane = newMonitorLaneClient("http://127.0.0.1:1" + managerLanePathPrefix + "/write")
	db.monitorLane.writeURL = srv.URL + managerLanePathPrefix + "/write"
	db.monitorLane.turnURL = srv.URL + managerLanePathPrefix + agentTurnPath

	laneScheduler := func(lane, incarnation string) string {
		address, err := schedulerLaneMCPURL(sidecar.URL+schedulerMCPPath, lane, incarnation)
		if err != nil {
			t.Fatal(err)
		}
		return address
	}
	fixture.parentMCP = laneScheduler(laneParent, "parent-run-1")
	fixture.monitorMCP = laneScheduler(laneMonitor, "monitor-run-1")
	return fixture
}

// handoversUsed is how many requests the parent handed to the monitor through
// the sidecar (session_work_lane.go). A scheduled round must not be one.
func (f *schedulerPairFixture) handoversUsed() int {
	f.db.monitorLane.mu.Lock()
	defer f.db.monitorLane.mu.Unlock()
	return len(f.db.monitorLane.asked)
}

// TestAMonitorsScheduleRunsInTheMonitorAndNowhereElse is the whole destination
// rule end to end: a thirty-minute schedule registered through the monitor
// lane's own address fires on the clock, arrives as a turn of the monitor's
// conversation with the prompt as registered, and leaves the person's chat
// untouched — no input, no turn, and no handover asked of the parent (AC-P4).
func TestAMonitorsScheduleRunsInTheMonitorAndNowhereElse(t *testing.T) {
	fixture := newSchedulerPairFixture(t, nil)
	ctx := context.Background()
	const theRound = "Check the staged rollout and record where it got to."

	registered := mcpCall(t, fixture.monitorMCP, "schedule_once", map[string]any{
		"prompt": theRound, "delay": "PT30M",
	})
	id, _ := registered["id"].(string)
	if id == "" {
		t.Fatalf("registration returned no id: %v", registered)
	}
	if got, _ := registered["nextRunAt"].(string); got != "2026-11-09T00:30:00Z" {
		t.Fatalf("next run = %q, want the half hour the registration asked for", got)
	}

	// Nothing is due yet, and a tick while nothing is due delivers nothing.
	fixture.sched.tick(ctx)
	if got := fixture.monitor.queueState(); got.Finished != 0 || !got.Idle {
		t.Fatalf("the monitor ran something before the schedule was due: %+v", got)
	}

	fixture.clock.advance(30 * time.Minute)
	fixture.sched.tick(ctx)

	var delivered string
	select {
	case delivered = <-fixture.monitorTurns:
	case <-time.After(20 * time.Second):
		t.Fatal("the monitor lane never got the round")
	}
	if delivered != theRound {
		t.Fatalf("the monitor's turn was given %q, want the prompt as registered", delivered)
	}

	// Exactly one round reached the monitor, and the schedule is spent.
	eventually(t, "the round settles", func() bool {
		fixture.sched.tick(ctx)
		return len(fixture.sched.list(laneCaller(laneMonitor, "monitor-run-1"))) == 0
	})
	for i := 0; i < 3; i++ {
		fixture.sched.tick(ctx)
	}
	select {
	case second := <-fixture.monitorTurns:
		t.Fatalf("the round was delivered twice; the second was %q", second)
	default:
	}
	// The receipt settles the round the moment the turn ends, which is just
	// before the lane's own counter moves; both are asserted, and the count
	// stays at the one round however many ticks follow.
	eventually(t, "the monitor records the round's turn", func() bool {
		return fixture.monitor.queueState().Finished == 1
	})
	for i := 0; i < 3; i++ {
		fixture.sched.tick(ctx)
	}
	if got := fixture.monitor.queueState(); got.Finished != 1 {
		t.Fatalf("the monitor ran %d turns, want exactly the one round", got.Finished)
	}
	// The round is in the monitor's own conversation, which is where its answer
	// belongs — and the only place it is.
	if payload, _ := laneRead(t, fixture.srv, managerLanePathPrefix, 0); !strings.Contains(payload, theRound) {
		t.Fatalf("the monitor's conversation does not hold the round: %q", payload)
	}

	// The person's chat: no input, no turn, nothing in its buffer.
	select {
	case what := <-fixture.parentTurns:
		t.Fatalf("%s, which was never given one", what)
	default:
	}
	if got := fixture.parent.queueState(); got.Finished != 0 || !got.Idle {
		t.Fatalf("the person's chat = %+v, want an untouched conversation", got)
	}
	if payload, _ := laneRead(t, fixture.srv, "", 0); payload != "" {
		t.Fatalf("the person's chat holds %q", payload)
	}
	// And the round did not arrive by asking the parent to pass it on: the
	// handover was available and untouched.
	if got := fixture.handoversUsed(); got != 0 {
		t.Fatalf("%d handovers were used for a scheduled round; the scheduler reaches the lane itself", got)
	}
}

// TestTheMonitorsRoundSetsItsOwnNextRoundAndEndsIt is the self-paced loop from
// inside the round, through the real addresses: the round that is running paces
// its own schedule by the id it was registered under, a leftover of an earlier
// run of that lane cannot, and the same id is how the lane ends the loop
// (AC-P2, AC-P4).
func TestTheMonitorsRoundSetsItsOwnNextRoundAndEndsIt(t *testing.T) {
	var fixture *schedulerPairFixture
	loopID := make(chan string, 1)
	paced := make(chan string, 1)
	refusedStale := make(chan string, 1)

	fixture = newSchedulerPairFixture(t, func(string) {
		id := <-loopID
		loopID <- id
		// A run of the lane that is gone must not move a live loop: the round
		// belongs to the run that holds it.
		stale, err := schedulerLaneMCPURL(strings.TrimSuffix(fixture.monitorMCP,
			laneMCPPath(schedulerLanePathPrefix, laneMonitor, "monitor-run-1"))+schedulerMCPPath,
			laneMonitor, "monitor-run-0")
		if err != nil {
			t.Error(err)
			return
		}
		refusedStale <- mcpCallRefusal(t, stale, "loop_next", map[string]any{"id": id, "delay": "PT2M"})
		answer := mcpCall(t, fixture.monitorMCP, "loop_next", map[string]any{"id": id, "delay": "PT2M"})
		next, _ := answer["nextRunAt"].(string)
		paced <- next
	})
	ctx := context.Background()

	registered := mcpCall(t, fixture.monitorMCP, "loop_create", map[string]any{
		"prompt": "Look at the rollout and decide when to look again.", "fallback_interval": "PT15M",
	})
	id, _ := registered["id"].(string)
	if id == "" {
		t.Fatalf("registration returned no id: %v", registered)
	}
	loopID <- id

	fixture.sched.tick(ctx) // a loop's first round is due at once
	select {
	case <-fixture.monitorTurns:
	case <-time.After(20 * time.Second):
		t.Fatal("the loop's first round never reached the monitor")
	}
	if got := <-refusedStale; !strings.Contains(got, "earlier run") {
		t.Fatalf("a stale run's loop_next = %q, want a refusal naming the round's owner", got)
	}
	if got := <-paced; got != "2026-11-09T00:02:00Z" {
		t.Fatalf("the round set its next run to %q", got)
	}

	// The round's own time stands: the fallback does not overwrite it when the
	// turn ends, and the loop keeps the id it was registered under.
	eventually(t, "the round settles", func() bool {
		fixture.sched.tick(ctx)
		schedules := fixture.sched.list(laneCaller(laneMonitor, "monitor-run-1"))
		return len(schedules) == 1 && !schedules[0].Running
	})
	listed := fixture.sched.list(laneCaller(laneMonitor, "monitor-run-1"))
	if listed[0].ID != id || listed[0].NextRunAt != "2026-11-09T00:02:00Z" {
		t.Fatalf("the loop after its round = %+v", listed[0])
	}

	// The parent cannot end the monitor's loop; the monitor can, by that id.
	if refusal := mcpCallRefusal(t, fixture.parentMCP, "schedule_cancel", map[string]any{"id": id}); !strings.Contains(refusal, laneMonitor) {
		t.Fatalf("the parent's cancel = %q, want a refusal naming the lane that owns it", refusal)
	}
	cancelled := mcpCall(t, fixture.monitorMCP, "schedule_cancel", map[string]any{"id": id})
	if cancelled["cancelled"] == nil {
		t.Fatalf("cancel = %v", cancelled)
	}
	if got := fixture.sched.list(laneCaller(laneMonitor, "monitor-run-1")); len(got) != 0 {
		t.Fatalf("the loop outlived its cancel: %+v", got)
	}
	if got := fixture.parent.queueState(); got.Finished != 0 {
		t.Fatalf("the person's chat ran %d turns during a monitor's loop", got.Finished)
	}
	if got := fixture.handoversUsed(); got != 0 {
		t.Fatalf("%d handovers were used for a loop's round", got)
	}
}

// mcpCallRefusal is one tools/call whose answer is expected to be the tool
// refusing: it returns the reason the model would read.
func mcpCallRefusal(t *testing.T, url, name string, args map[string]any) string {
	t.Helper()
	payload := mcpCall(t, url, name, args)
	refusal, _ := payload["error"].(string)
	if refusal == "" {
		t.Fatalf("%s was accepted: %v", name, payload)
	}
	return refusal
}
