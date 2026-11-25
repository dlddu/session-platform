package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newLanePairTestServer is a manager pod running both lanes behind one agent
// port, which is the shape the control plane addresses (AC-N11). Both lanes
// share one runner so a test can see what the two of them do at the same time.
func newLanePairTestServer(t *testing.T, runner commandRunner) (parent, monitor *claudeWorkload, srv *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	build := func(stateDir string) *claudeWorkload {
		c, err := newClaudeWorkload(claudeConfig{
			StateDir: stateDir,
			Model:    "gpt-6-sol",
			Binary:   defaultClaudeRunnerBinary,
			Runner:   runner,
			Logger:   testLogger(),
			Tools:    toolSurface{},
		})
		if err != nil {
			t.Fatalf("new lane workload at %s: %v", stateDir, err)
		}
		c.now = func() time.Time { return testTurnTime }
		return c
	}
	parent = build(filepath.Join(root, "state"))
	monitor = build(managerMonitorStateDir(filepath.Join(root, "state")))
	srv = httptest.NewServer(routes(testLogger(), &agent{claude: parent, monitor: monitor}))
	t.Cleanup(func() {
		srv.Close()
		parent.Close()
		monitor.Close()
	})
	return parent, monitor, srv
}

func laneWrite(t *testing.T, srv *httptest.Server, prefix, prompt string) {
	t.Helper()
	resp, err := http.Post(srv.URL+prefix+"/write", "text/plain", strings.NewReader(prompt))
	if err != nil {
		t.Fatalf("write %s: %v", prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("write %s = %d: %s", prefix, resp.StatusCode, body)
	}
}

func laneRead(t *testing.T, srv *httptest.Server, prefix string, offset int64) (string, int64) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s%s/read?offset=%d", srv.URL, prefix, offset))
	if err != nil {
		t.Fatalf("read %s: %v", prefix, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("read %s = %d: %s", prefix, resp.StatusCode, body)
	}
	var out struct {
		Payload    string `json:"payload"`
		NextOffset int64  `json:"nextOffset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode read %s: %v", prefix, err)
	}
	return out.Payload, out.NextOffset
}

// TestManagerLanesRunAtTheSameTime is the property the pair exists for: the
// parent writing a long answer must not hold the monitor's turn.
//
// The runner blocks every run until the test releases it, so both lanes are
// inside a run at once only if they really are two workers. maxActive is the
// measurement — a single serial worker could never reach 2.
func TestManagerLanesRunAtTheSameTime(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	_, _, srv := newLanePairTestServer(t, runner)

	laneWrite(t, srv, "", "the person asked something long")
	laneWrite(t, srv, managerLanePathPrefix, "move the work along")

	// Both runs must have started before either is released.
	for i := 0; i < 2; i++ {
		select {
		case <-runner.started:
		case <-time.After(5 * time.Second):
			_, active := runner.snapshot()
			t.Fatalf("only %d lane run(s) started; the lanes are not running at the same time", active)
		}
	}
	if _, maxActive := runner.snapshot(); maxActive != 2 {
		t.Fatalf("max concurrent lane runs = %d, want 2 — the lanes are taking turns, not running beside each other", maxActive)
	}
	close(runner.release)
}

// TestManagerLanesKeepSeparateConversations is the other half of separation:
// two workers would still be one context if they shared an output buffer. Each
// lane's cursor has to mean only its own conversation.
func TestManagerLanesKeepSeparateConversations(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.output = func(index int, opts runnerOptions) {
		_, _ = opts.Stdout.Write([]byte(fmt.Sprintf("answer-%d\n", index)))
	}
	parent, monitor, srv := newLanePairTestServer(t, runner)

	laneWrite(t, srv, "", "for the person")
	waitClaudeIdle(t, parent)
	laneWrite(t, srv, managerLanePathPrefix, "for the work")
	waitClaudeIdle(t, monitor)

	parentOut, parentNext := laneRead(t, srv, "", 0)
	monitorOut, monitorNext := laneRead(t, srv, managerLanePathPrefix, 0)

	if !strings.Contains(parentOut, "for the person") {
		t.Fatalf("the parent lane did not record its own prompt: %q", parentOut)
	}
	if strings.Contains(parentOut, "for the work") {
		t.Fatalf("the parent lane can see the monitor's conversation: %q", parentOut)
	}
	if !strings.Contains(monitorOut, "for the work") {
		t.Fatalf("the monitor lane did not record its own prompt: %q", monitorOut)
	}
	if strings.Contains(monitorOut, "for the person") {
		t.Fatalf("the monitor lane can see the person's chat: %q", monitorOut)
	}
	// Separate buffers mean separate cursors. Reading one lane at the other's
	// nextOffset must not be treated as "nothing new".
	if parentNext == 0 || monitorNext == 0 {
		t.Fatalf("both lanes must issue a cursor: parent=%d monitor=%d", parentNext, monitorNext)
	}
	if tail, _ := laneRead(t, srv, managerLanePathPrefix, 0); tail != monitorOut {
		t.Fatalf("the monitor lane's read is not stable: %q then %q", monitorOut, tail)
	}
}

// TestManagerLanePathsServeOnlyTheConversation pins the prefix down. Anything
// under it that is not the lane's own conversation is refused by name, because
// answering it from the parent's workload would be exactly the leak the pair
// exists to prevent.
func TestManagerLanePathsServeOnlyTheConversation(t *testing.T) {
	runner := newFakeClaudeRunner()
	_, _, srv := newLanePairTestServer(t, runner)

	for _, path := range []string{
		managerLanePathPrefix + authSlotPath,
		managerLanePathPrefix + agentWorkTasksPath,
		managerLanePathPrefix + agentLanesPath,
		managerLanePathPrefix + "/checkpoint",
		managerLanePathPrefix,
		managerLanePathPrefix + "/read/extra",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s = %d (%s), want 404: the lane prefix serves its conversation only",
				path, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	// The pod's own paths stay exactly where they were, which is what keeps
	// every client that predates the pair correct.
	for _, path := range []string{"/healthz", agentQueuePath, agentLanesPath} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("the pod's own %s = %d, want it still served beside the lane prefix", path, resp.StatusCode)
		}
	}
}

// TestAgentLanesInventoryComesFromThePod checks the one answer that says the
// pair is in one pod, and that an unpaired manager is distinguishable from a
// pod that could not be asked.
func TestAgentLanesInventoryComesFromThePod(t *testing.T) {
	runner := newFakeClaudeRunner()
	_, _, srv := newLanePairTestServer(t, runner)

	get := func(url string) laneInventory {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("get lanes: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET %s = %d: %s", agentLanesPath, resp.StatusCode, body)
		}
		var inventory laneInventory
		if err := json.NewDecoder(resp.Body).Decode(&inventory); err != nil {
			t.Fatalf("decode lanes: %v", err)
		}
		return inventory
	}

	paired := get(srv.URL + agentLanesPath)
	if !paired.Paired || len(paired.Lanes) != 2 {
		t.Fatalf("paired inventory = %+v, want two lanes and paired=true", paired)
	}
	if paired.Lanes[0].Role != laneParent || paired.Lanes[0].Path != "/read" {
		t.Fatalf("first lane = %+v, want the parent on the agent's own path", paired.Lanes[0])
	}
	if paired.Lanes[1].Role != laneMonitor || paired.Lanes[1].Path != managerLanePathPrefix+"/read" {
		t.Fatalf("second lane = %+v, want the monitor under %s", paired.Lanes[1], managerLanePathPrefix)
	}

	// A manager with one lane answers the route rather than 404ing it: a reader
	// has to be able to tell "one conversation" from "this pod is too old to
	// ask", and only the first has an answer here.
	single, lone := newClaudeTestServer(t, newFakeClaudeRunner(), "gpt-6-sol", false, nil)
	_ = single
	unpaired := get(lone.URL + agentLanesPath)
	if unpaired.Paired || len(unpaired.Lanes) != 1 || unpaired.Lanes[0].Role != laneParent {
		t.Fatalf("unpaired inventory = %+v, want one parent lane and paired=false", unpaired)
	}
}

// TestManagerPairEnabledRefusesAnythingButOnOrOff: a deployment that meant to
// turn the pair on and misspelled it must not run a single-lane manager that
// looks exactly like one that was never paired.
func TestManagerPairEnabledRefusesAnythingButOnOrOff(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		workload string
		want     bool
		wantErr  bool
	}{
		{raw: "", workload: workloadCodexManager, want: false},
		{raw: "0", workload: workloadCodexManager, want: false},
		{raw: "1", workload: workloadCodexManager, want: true},
		{raw: " 1 ", workload: workloadCodexManager, want: true},
		{raw: "true", workload: workloadCodexManager, wantErr: true},
		{raw: "yes", workload: workloadCodexManager, wantErr: true},
		{raw: "on", workload: workloadCodexManager, wantErr: true},
		{raw: "1", workload: legacyWorkloadCodexChat, want: true},
		// The pair is a manager's. Asking for it anywhere else is a
		// configuration mistake, not a quiet single lane.
		{raw: "1", workload: workloadCodexTask, wantErr: true},
		{raw: "1", workload: workloadClaudeCode, wantErr: true},
		{raw: "", workload: workloadCodexTask, want: false},
	} {
		t.Setenv(managerMonitorLaneEnv, tc.raw)
		got, err := managerPairEnabled(tc.workload)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s=%q on %s: want a refusal, got paired=%v", managerMonitorLaneEnv, tc.raw, tc.workload, got)
			}
			if got {
				t.Fatalf("%s=%q on %s: a refusal must not also report paired", managerMonitorLaneEnv, tc.raw, tc.workload)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s=%q on %s: %v", managerMonitorLaneEnv, tc.raw, tc.workload, err)
		}
		if got != tc.want {
			t.Fatalf("%s=%q on %s = %v, want %v", managerMonitorLaneEnv, tc.raw, tc.workload, got, tc.want)
		}
	}
}

// TestManagerMonitorStateDirSitsOutsideTheArchive: the monitor's conversation
// must not be inside the tree a checkpoint copies, or the person's archive
// would carry it and a checkpoint taken mid-turn would be inconsistent.
func TestManagerMonitorStateDirSitsOutsideTheArchive(t *testing.T) {
	t.Setenv(managerMonitorStateDirEnv, "")
	parent := "/session/state"
	got := managerMonitorStateDir(parent)
	if got == parent || strings.HasPrefix(got, parent+"/") {
		t.Fatalf("monitor state dir %q is inside the parent's archived tree %q", got, parent)
	}
	if filepath.Dir(got) != filepath.Dir(parent) {
		t.Fatalf("monitor state dir %q is not beside the parent's %q", got, parent)
	}
	t.Setenv(managerMonitorStateDirEnv, "/session/state-monitor-explicit")
	if got := managerMonitorStateDir(parent); got != "/session/state-monitor-explicit" {
		t.Fatalf("the platform's own %s must win: got %q", managerMonitorStateDirEnv, got)
	}
}

// TestSessionDBLaneMCPURLCarriesTheRole is what makes a lane unable to call as
// the other one: the role is in the address, and the address is in the lane's
// managed config rather than in any argument the model can write.
func TestSessionDBLaneMCPURLCarriesTheRole(t *testing.T) {
	base := "http://127.0.0.1:8099" + sessionDBMCPPath
	got, err := sessionDBLaneMCPURL(base, laneMonitor, "a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:8099" + sessionDBLaneMCPPath(laneMonitor, "a1b2c3")
	if got != want {
		t.Fatalf("lane URL = %q, want %q", got, want)
	}
	if _, err := sessionDBLaneMCPURL(base, "auditor", "a1b2c3"); err == nil {
		t.Fatal("a role that is not a lane must be refused rather than addressed")
	}
	if _, err := sessionDBLaneMCPURL(base, laneParent, "bad/incarnation"); err == nil {
		t.Fatal("an incarnation that is not an id must be refused")
	}
	if _, err := sessionDBLaneMCPURL("http://127.0.0.1:8099/elsewhere", laneParent, "a1"); err == nil {
		t.Fatal("a non-database address must not be guessed at")
	}
	// An unconfigured database is not an error: a slot without one has no lane
	// address to derive and the workload still starts.
	if got, err := sessionDBLaneMCPURL("", laneParent, "a1"); err != nil || got != "" {
		t.Fatalf("empty URL = %q, %v; want it left empty", got, err)
	}
}

// TestSessionDBLaneCallerComesFromTheAddress: the lane and the run are read
// from the path, so nothing in the request body can change who is calling.
func TestSessionDBLaneCallerComesFromTheAddress(t *testing.T) {
	for _, tc := range []struct {
		path        string
		underPrefix bool
		wantLane    string
		wantRun     string
		wantRefusal bool
	}{
		{path: sessionDBMCPPath, underPrefix: false},
		{path: "/session-db/other", underPrefix: false},
		{path: sessionDBLaneMCPPath(laneParent, "run1"), underPrefix: true, wantLane: laneParent, wantRun: "run1"},
		{path: sessionDBLaneMCPPath(laneMonitor, "run2"), underPrefix: true, wantLane: laneMonitor, wantRun: "run2"},
		{path: sessionDBLanePathPrefix, underPrefix: true, wantRefusal: true},
		{path: sessionDBLanePathPrefix + "/monitor", underPrefix: true, wantRefusal: true},
		{path: sessionDBLanePathPrefix + "/monitor/run/extra", underPrefix: true, wantRefusal: true},
		{path: sessionDBLanePathPrefix + "/auditor/run", underPrefix: true, wantRefusal: true},
		{path: sessionDBLanePathPrefix + "/monitor/" + strings.Repeat("x", 129), underPrefix: true, wantRefusal: true},
	} {
		caller, under, refusal := sessionDBLaneCaller(tc.path)
		if under != tc.underPrefix {
			t.Fatalf("%s: underPrefix = %v, want %v", tc.path, under, tc.underPrefix)
		}
		if tc.wantRefusal {
			if refusal == "" {
				t.Fatalf("%s: a malformed lane address must be refused by name, not served", tc.path)
			}
			if caller.lane != "" || caller.incarnation != "" {
				t.Fatalf("%s: a refusal must not also yield a caller: %+v", tc.path, caller)
			}
			continue
		}
		if refusal != "" {
			t.Fatalf("%s: unexpected refusal %q", tc.path, refusal)
		}
		if caller.lane != tc.wantLane || caller.incarnation != tc.wantRun {
			t.Fatalf("%s: caller = %+v, want lane %q run %q", tc.path, caller, tc.wantLane, tc.wantRun)
		}
	}
}

// TestNewLaneIncarnationIsDistinctPerRun: the database only has to tell two
// runs of a lane apart, and that is what makes a claim a dead run left behind
// recoverable exactly once.
func TestNewLaneIncarnationIsDistinctPerRun(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		got, err := newLaneIncarnation()
		if err != nil {
			t.Fatal(err)
		}
		if !workID.MatchString(got) {
			t.Fatalf("incarnation %q is not usable in a lane address", got)
		}
		if seen[got] {
			t.Fatalf("incarnation %q repeated; a restart would look like the same run", got)
		}
		seen[got] = true
	}
}
