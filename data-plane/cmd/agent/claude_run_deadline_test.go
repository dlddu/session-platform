package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// runDeadline is what one turn's context says about when the platform will cut
// the turn off. A wall-clock deadline is exactly what a fixed invocation cap is
// made of, so reading it is how these tests tell "this turn has an hour to
// live" apart from "this turn runs until its work is done" — without waiting
// out either span.
type runDeadline struct {
	set bool
	in  time.Duration
}

// deadlineRecordingRunner reports each turn's deadline and then blocks until
// the test releases it or the turn's context is cancelled.
type deadlineRecordingRunner struct {
	seen    chan runDeadline
	release chan struct{}
}

func newDeadlineRecordingRunner() *deadlineRecordingRunner {
	return &deadlineRecordingRunner{
		seen:    make(chan runDeadline, 4),
		release: make(chan struct{}),
	}
}

func (r *deadlineRecordingRunner) Run(ctx context.Context, argv []string, opts runnerOptions) error {
	seen := runDeadline{}
	if at, ok := ctx.Deadline(); ok {
		seen.set = true
		seen.in = time.Until(at)
	}
	r.seen <- seen
	select {
	case <-r.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *deadlineRecordingRunner) receive(t *testing.T) runDeadline {
	t.Helper()
	select {
	case seen := <-r.seen:
		return seen
	case <-time.After(5 * time.Second):
		t.Fatal("claude invocation did not start")
		return runDeadline{}
	}
}

// AC-E2: a turn the pod did not put a deadline on carries none. This is the
// deterministic form of "a healthy turn is still running after the half hour
// the platform used to kill it at": a context with no deadline has no such
// hour to reach, whatever the clock says.
func TestClaudeTurnRunsWithoutADeadlineByDefault(t *testing.T) {
	runner := newDeadlineRecordingRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, "work that outlives any fixed cap", http.StatusOK)
	if seen := runner.receive(t); seen.set {
		t.Fatalf("default turn was given a deadline %s away; a healthy turn must not be cut off on a platform-picked clock", seen.in)
	}

	// The turn still ends the moment its own work does, and it ends as a
	// success — a deadline-free turn is not an unfinishable one.
	close(runner.release)
	waitClaudeIdle(t, c)
	payload, _ := readViaHTTP(t, srv, 0)
	if want := turnClose(1, "ok"); !strings.Contains(payload, want) {
		t.Fatalf("turn did not end ok after its work finished; output = %q", payload)
	}
}

// AC-E2: the shared reader every turn-running workload (claude-code, the codex
// types and both manager lanes, minimax-code) uses for the optional deadline.
// Unset means none; a set value is honoured as given; a value that is not a
// positive duration fails the boot rather than quietly installing a cap.
func TestClaudeRunTimeoutFromEnvDefaultsToNoDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		set     bool
		want    time.Duration
		wantErr bool
	}{
		{name: "unset means no deadline", want: 0},
		{name: "empty means no deadline", value: "", set: true, want: 0},
		{name: "a set duration is honoured", value: "45m", set: true, want: 45 * time.Minute},
		{name: "zero is refused", value: "0s", set: true, wantErr: true},
		{name: "negative is refused", value: "-5m", set: true, wantErr: true},
		{name: "nonsense is refused", value: "half an hour", set: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(claudeRunTimeoutEnv, tc.value)
			}
			got, err := claudeRunTimeoutFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s = %s, want an error", claudeRunTimeoutEnv, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", claudeRunTimeoutEnv, err)
			}
			if got != tc.want {
				t.Fatalf("%s = %s, want %s", claudeRunTimeoutEnv, got, tc.want)
			}
		})
	}
}

// AC-E2: a deadline the pod did configure is still a deadline, and the turn it
// cuts off is still reported as a timeout. This is the policy the default used
// to carry, so a run under it is what a turn would have met at the old cap.
func TestClaudeConfiguredRunTimeoutStillEndsTheTurn(t *testing.T) {
	runner := newDeadlineRecordingRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)
	c.runTimeout = 50 * time.Millisecond

	writeViaHTTP(t, srv, "work the configured deadline cuts off", http.StatusOK)
	seen := runner.receive(t)
	if !seen.set {
		t.Fatal("configured run timeout did not reach the turn's context")
	}
	if seen.in <= 0 || seen.in > 50*time.Millisecond {
		t.Fatalf("turn deadline is %s away, want the configured 50ms", seen.in)
	}
	waitClaudeIdle(t, c)
	payload, _ := readViaHTTP(t, srv, 0)
	if want := turnClose(1, "timeout"); !strings.Contains(payload, want) {
		t.Fatalf("turn that passed its configured deadline did not end as a timeout; output = %q", payload)
	}
	c.mu.Lock()
	hasRun := c.hasRun
	c.mu.Unlock()
	if hasRun {
		t.Fatal("timed-out first invocation created resume state")
	}
}

// AC-E2/AC-E3: shutdown is what ends a deadline-free turn that is still
// working — at once, with the turn closed as cancelled rather than left open.
// Without a deadline there is no backstop behind this path, so it is the one
// that has to hold.
func TestClaudeShutdownEndsADeadlineFreeTurnAtOnce(t *testing.T) {
	runner := newDeadlineRecordingRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, "work interrupted by shutdown", http.StatusOK)
	if seen := runner.receive(t); seen.set {
		t.Fatalf("default turn was given a deadline %s away", seen.in)
	}

	closed := make(chan struct{})
	go func() {
		c.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not reclaim the running turn")
	}

	// Read the recorded transcript rather than /read: a shut-down workload
	// refuses reads (503), and what this test is about is what shutdown wrote
	// before it went.
	payload := string(c.out.snapshot())
	if want := turnClose(1, "canceled"); !strings.Contains(payload, want) {
		t.Fatalf("shutdown left the turn open or mislabelled; output = %q", payload)
	}
}
