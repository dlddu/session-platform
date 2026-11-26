package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A manager pod runs the parent's and the monitor's invocations concurrently
// on one execCommandRunner, so these tests run two lanes at once against the
// real runner and assert each one only ever reaches its own processes. The
// fixture below stands in for the CLI: no network, no cluster, nothing outside
// the test's own temp dir.
const (
	claudeLaneFixtureEnv = "SESSION_PLATFORM_CLAUDE_LANE_FIXTURE"
	claudeLaneNameEnv    = "SESSION_PLATFORM_CLAUDE_LANE_NAME"
	claudeLaneReleaseEnv = "SESSION_PLATFORM_CLAUDE_LANE_RELEASE"
	claudeLaneToolPIDEnv = "SESSION_PLATFORM_CLAUDE_LANE_TOOL_PID"
	claudeLaneExitEnv    = "SESSION_PLATFORM_CLAUDE_LANE_EXIT"

	claudeLaneStartMarker = ":start"
	claudeLaneMoreMarker  = ":more"
	claudeLaneWait        = 20 * time.Second
)

// TestClaudeLaneFixture is one lane's stand-in CLI, selected by env the way
// TestClaudeDetachedProcessHelper is: it starts a detached tool in its own
// session, answers once, waits for the test to release it, answers again and
// exits with the status the test gave it.
func TestClaudeLaneFixture(t *testing.T) {
	switch os.Getenv(claudeLaneFixtureEnv) {
	case "cli":
		// Its own session, so this tool is no longer in the invocation's
		// session and only the reaper it reparents onto can still find it.
		// That is the process the sibling lane's cleanup used to take.
		tool := exec.Command(os.Args[0], "-test.run=^TestClaudeLaneFixture$")
		tool.Env = append(os.Environ(), claudeLaneFixtureEnv+"=tool")
		tool.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := tool.Start(); err != nil {
			os.Exit(90)
		}
		pid := strconv.Itoa(tool.Process.Pid)
		if err := os.WriteFile(os.Getenv(claudeLaneToolPIDEnv), []byte(pid), 0o600); err != nil {
			os.Exit(91)
		}
		fmt.Println(os.Getenv(claudeLaneNameEnv) + claudeLaneStartMarker)
		release := os.Getenv(claudeLaneReleaseEnv)
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Println(os.Getenv(claudeLaneNameEnv) + claudeLaneMoreMarker)
		code, err := strconv.Atoi(os.Getenv(claudeLaneExitEnv))
		if err != nil {
			os.Exit(92)
		}
		os.Exit(code)
	case "tool":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// TestExecCommandRunnerConcurrentLanesOwnOnlyTheirOwnProcesses runs both lanes
// at once, lets one finish, and checks the other keeps answering and reports
// its own status. Before invocation-scoped ownership the lane that finished
// first killed the running lane's CLI and reaped its status, so the surviving
// lane's assertions here are the regression: more output after the sibling
// returned, its own exit code, and not a signalled one.
func TestExecCommandRunnerConcurrentLanesOwnOnlyTheirOwnProcesses(t *testing.T) {
	for _, order := range []struct {
		name  string
		first int
	}{
		{"parent lane finishes first", 0},
		{"monitor lane finishes first", 1},
	} {
		t.Run(order.name, func(t *testing.T) {
			lanes := startClaudeLanes(t, context.Background(), context.Background())
			first, second := lanes[order.first], lanes[1-order.first]

			first.release(t)
			firstErr := first.wait(t)
			first.requireOwnExit(t, firstErr)
			first.requireFullAnswer(t)
			first.requireToolGone(t)

			// The sibling's whole cleanup ran while this lane was mid-turn:
			// its CLI, its tool and its unclaimed exit status all have to be
			// untouched by it.
			second.requireStillRunning(t)
			second.requireToolAlive(t)

			second.release(t)
			secondErr := second.wait(t)
			second.requireOwnExit(t, secondErr)
			second.requireFullAnswer(t)
			second.requireToolGone(t)
		})
	}
}

// TestExecCommandRunnerCancelReachesOnlyItsOwnLane is the same split for the
// cancel and timeout path, which cancels the invocation's context: the lane
// still reports the signalled status it did before a reaper sat in between —
// what makes the turn read as cancelled rather than a plain non-zero exit —
// and the concurrent lane runs on to its own normal exit.
func TestExecCommandRunnerCancelReachesOnlyItsOwnLane(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	defer cancel()
	lanes := startClaudeLanes(t, canceled, context.Background())
	doomed, survivor := lanes[0], lanes[1]

	cancel()
	err := doomed.wait(t)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("cancelled lane did not report an exit status: %v (output %q)", err, doomed.output())
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("cancelled lane lost its signalled status: %v", exitErr)
	}
	doomed.requireToolGone(t)

	survivor.requireStillRunning(t)
	survivor.requireToolAlive(t)
	survivor.release(t)
	survivorErr := survivor.wait(t)
	survivor.requireOwnExit(t, survivorErr)
	survivor.requireFullAnswer(t)
	survivor.requireToolGone(t)
}

// claudeLane is one invocation under test: the runner call, its output and the
// detached tool whose fate says which invocation the cleanup reached.
type claudeLane struct {
	name     string
	exitCode int
	status   string
	release_ string
	toolPID  string
	out      *claudeLaneOutput
	done     chan error
	err      error
	waited   bool
}

// startClaudeLanes starts one lane per context on the real runner and returns
// once both have answered, so a later cleanup cannot be mistaken for a lane
// that never started.
func startClaudeLanes(t *testing.T, ctxs ...context.Context) []*claudeLane {
	t.Helper()
	// Lane 0 exits normally and lane 1 with a status of its own, so whichever
	// order the subtest picks asserts both an untouched normal exit and an
	// untouched non-zero one.
	codes := []int{0, 23}
	statuses := []string{"ok", "error"}
	lanes := make([]*claudeLane, 0, len(ctxs))
	for i, ctx := range ctxs {
		dir := t.TempDir()
		lane := &claudeLane{
			name:     fmt.Sprintf("lane%d", i),
			exitCode: codes[i],
			status:   statuses[i],
			release_: filepath.Join(dir, "release"),
			toolPID:  filepath.Join(dir, "tool.pid"),
			out:      &claudeLaneOutput{},
			done:     make(chan error, 1),
		}
		env := append(os.Environ(),
			claudeLaneFixtureEnv+"=cli",
			claudeLaneNameEnv+"="+lane.name,
			claudeLaneReleaseEnv+"="+lane.release_,
			claudeLaneToolPIDEnv+"="+lane.toolPID,
			claudeLaneExitEnv+"="+strconv.Itoa(lane.exitCode),
		)
		go func() {
			lane.done <- (execCommandRunner{}).Run(ctx, []string{
				os.Args[0], "-test.run=^TestClaudeLaneFixture$",
			}, runnerOptions{Dir: dir, Env: env, Stdout: lane.out, Stderr: lane.out})
		}()
		// A lane left running would hold a tool for a minute, so release every
		// lane the test did not already wait on and drain its goroutine
		// however the test ends.
		t.Cleanup(func() {
			if !lane.waited {
				_ = os.WriteFile(lane.release_, nil, 0o600)
				select {
				case <-lane.done:
				case <-time.After(claudeLaneWait):
				}
			}
			if pid, err := lane.readToolPID(); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		})
		lanes = append(lanes, lane)
	}
	for _, lane := range lanes {
		lane.awaitMarker(t, claudeLaneStartMarker)
		lane.requireToolAlive(t)
	}
	return lanes
}

func (l *claudeLane) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(l.release_, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (l *claudeLane) wait(t *testing.T) error {
	t.Helper()
	if l.waited {
		return l.err
	}
	select {
	case err := <-l.done:
		l.waited, l.err = true, err
		return err
	case <-time.After(claudeLaneWait):
		t.Fatalf("%s did not return (output %q)", l.name, l.output())
		return nil
	}
}

func (l *claudeLane) output() string { return l.out.String() }

// requireOwnExit checks the status the agent sees is the one this lane's own
// fixture exited with, and that nothing signalled it on the way.
func (l *claudeLane) requireOwnExit(t *testing.T, err error) {
	t.Helper()
	if got := claudeTurnStatus(err, false); got != l.status {
		t.Fatalf("%s turn status = %q, want %q (err %v)", l.name, got, l.status, err)
	}
	if l.exitCode == 0 {
		if err != nil {
			t.Fatalf("%s failed its normal exit: %v (output %q)", l.name, err, l.output())
		}
		return
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("%s did not report an exit status: %v (output %q)", l.name, err, l.output())
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		t.Fatalf("%s was signalled (%v) instead of exiting on its own", l.name, status.Signal())
	}
	if exitErr.ExitCode() != l.exitCode {
		t.Fatalf("%s exit code = %d, want %d", l.name, exitErr.ExitCode(), l.exitCode)
	}
}

// requireFullAnswer checks the lane produced the answer it had left after the
// sibling lane finished, not just the one from before.
func (l *claudeLane) requireFullAnswer(t *testing.T) {
	t.Helper()
	for _, marker := range []string{claudeLaneStartMarker, claudeLaneMoreMarker} {
		if !strings.Contains(l.output(), l.name+marker) {
			t.Fatalf("%s output is missing %q: %q", l.name, l.name+marker, l.output())
		}
	}
}

func (l *claudeLane) requireStillRunning(t *testing.T) {
	t.Helper()
	select {
	case err := <-l.done:
		l.waited, l.err = true, err
		t.Fatalf("%s ended with the sibling lane: %v (output %q)", l.name, err, l.output())
	case <-time.After(200 * time.Millisecond):
	}
}

func (l *claudeLane) requireToolAlive(t *testing.T) {
	t.Helper()
	pid, err := l.readToolPID()
	if err != nil {
		t.Fatalf("%s tool pid: %v", l.name, err)
	}
	state, live := claudeProcessState(t, pid)
	if !live || state == "Z" {
		t.Fatalf("%s detached tool %d is gone (state %q) while its lane runs", l.name, pid, state)
	}
}

// requireToolGone checks the tool was killed and waited on, not left behind as
// an orphan of a dead reaper or as a zombie nobody reaps.
func (l *claudeLane) requireToolGone(t *testing.T) {
	t.Helper()
	pid, err := l.readToolPID()
	if err != nil {
		t.Fatalf("%s tool pid: %v", l.name, err)
	}
	deadline := time.Now().Add(claudeLaneWait)
	for {
		state, live := claudeProcessState(t, pid)
		if !live {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s left its detached tool %d behind (state %q)", l.name, pid, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (l *claudeLane) readToolPID() (int, error) {
	data, err := os.ReadFile(l.toolPID)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func (l *claudeLane) awaitMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(claudeLaneWait)
	for !strings.Contains(l.output(), l.name+marker) {
		select {
		case err := <-l.done:
			l.waited, l.err = true, err
			t.Fatalf("%s ended before %q: %v (output %q)", l.name, marker, err, l.output())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never wrote %q (output %q)", l.name, marker, l.output())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The pid file is written before the first answer, so both are readable
	// once the marker is out.
	if _, err := l.readToolPID(); err != nil {
		t.Fatalf("%s tool pid: %v", l.name, err)
	}
}

// claudeProcessState reports the /proc state letter of pid, and whether it is
// still there at all. A reaped process has no entry; a zombie has state "Z",
// which is how an unwaited descendant differs from a cleaned-up one.
func claudeProcessState(t *testing.T, pid int) (string, bool) {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return "", false
		}
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	fields := strings.Fields(string(data[strings.LastIndex(string(data), ")")+1:]))
	if len(fields) == 0 {
		t.Fatalf("/proc/%d/stat has no state field", pid)
	}
	return fields[0], true
}

// claudeLaneOutput collects a lane's bytes while the test reads them from
// another goroutine.
type claudeLaneOutput struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *claudeLaneOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(p)
}

func (o *claudeLaneOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}
