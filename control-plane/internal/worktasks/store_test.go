package worktasks

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// baseTime keeps the clock in tests explicit: an observation must land after
// the write it observes, and real time is too coarse to assert that.
var baseTime = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBindingIsOneToOneAndRetrySafe(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.Bind(ctx, "parent1", "monitor1", "pocket-aide"); err != nil {
		t.Fatal(err)
	}
	// A lost response must be retryable.
	if _, err := s.Bind(ctx, "parent1", "monitor1", "pocket-aide"); err != nil {
		t.Fatalf("rebinding the same pair must be a no-op: %v", err)
	}
	for _, tc := range []struct{ parent, monitor string }{
		{"parent1", "monitor2"}, // parent already has a monitor
		{"parent2", "monitor1"}, // monitor already serves a parent
		{"parent3", "parent3"},  // a session cannot monitor itself
	} {
		if _, err := s.Bind(ctx, tc.parent, tc.monitor, "pocket-aide"); !errors.Is(err, ErrBound) {
			t.Fatalf("Bind(%s, %s) = %v, want ErrBound", tc.parent, tc.monitor, err)
		}
	}
	b, err := s.BindingForParent(ctx, "parent1")
	if err != nil || b.MonitorSessionID != "monitor1" || b.Tenant != "pocket-aide" {
		t.Fatalf("BindingForParent = %+v, %v", b, err)
	}
	if b, err := s.BindingForMonitor(ctx, "monitor1"); err != nil || b.ParentSessionID != "parent1" {
		t.Fatalf("BindingForMonitor = %+v, %v", b, err)
	}
	if _, err := s.BindingForParent(ctx, "stranger"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unbound session must not resolve: %v", err)
	}
}

func TestPutTaskIsCASAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	first, err := s.PutTask(ctx, "monitor1", "task1", "req1", 0, `{"status":"running"}`)
	if err != nil || first.Version != 1 {
		t.Fatalf("first put = %+v, %v", first, err)
	}
	// Same request_id, same body: the first commit comes back, the version
	// does not move.
	again, err := s.PutTask(ctx, "monitor1", "task1", "req1", 0, `{"status":"running"}`)
	if err != nil || again.Version != 1 {
		t.Fatalf("retry = %+v, %v; want version 1", again, err)
	}
	// Same request_id, different body: refused rather than overwriting.
	if _, err := s.PutTask(ctx, "monitor1", "task1", "req1", 1, `{"status":"done"}`); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("reused request id = %v, want ErrRequestReused", err)
	}
	// Stale expected version is refused.
	if _, err := s.PutTask(ctx, "monitor1", "task1", "req2", 0, `{"status":"done"}`); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale version = %v, want ErrVersionMismatch", err)
	}
	second, err := s.PutTask(ctx, "monitor1", "task1", "req2", 1, `{"status":"done"}`)
	if err != nil || second.Version != 2 {
		t.Fatalf("second put = %+v, %v", second, err)
	}
	hist, err := s.History(ctx, "monitor1", "task1")
	if err != nil || len(hist) != 2 || hist[0].Version != 2 || hist[0].RequestID != "req2" {
		t.Fatalf("history = %+v, %v", hist, err)
	}
}

func TestOneMonitorsLedgerIsInvisibleToAnother(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.PutTask(ctx, "monitor1", "task1", "req1", 0, "body"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Task(ctx, "monitor2", "task1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another monitor must not read the task: %v", err)
	}
	tasks, err := s.Tasks(ctx, "monitor2")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("Tasks(monitor2) = %+v, %v; want empty", tasks, err)
	}
	// The same task id under a different monitor is a different row, so one
	// monitor's write can never bump another's version.
	other, err := s.PutTask(ctx, "monitor2", "task1", "req2", 0, "other")
	if err != nil || other.Version != 1 {
		t.Fatalf("monitor2 put = %+v, %v", other, err)
	}
}

func TestReadsDoNotTouchTheLedger(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.PutTask(ctx, "monitor1", "task1", "req1", 0, "body"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Task(ctx, "monitor1", "task1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Task(ctx, "monitor1", "task1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tasks(ctx, "monitor1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.History(ctx, "monitor1", "task1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.LastUpdated(ctx, "monitor1"); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.Task(ctx, "monitor1", "task1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || !after.UpdatedAt.Equal(before.UpdatedAt) || !after.ObservedAt.Equal(before.ObservedAt) {
		t.Fatalf("reads changed the row: before %+v after %+v", before, after)
	}
}

func TestObservationIsNotAChange(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	// The clock is injected before the write so the observation can be proved
	// to land after it; real time is too coarse for a one-second assertion.
	ticks := int64(0)
	s.now = func() time.Time { ticks++; return baseTime.Add(time.Duration(ticks) * time.Minute) }
	if _, err := s.PutTask(ctx, "monitor1", "task1", "req1", 0, "body"); err != nil {
		t.Fatal(err)
	}
	if err := s.Observe(ctx, "monitor1", "task1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(ctx, "monitor1", "task1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("observation moved the version to %d", got.Version)
	}
	if !got.ObservedAt.After(got.UpdatedAt) {
		t.Fatalf("observedAt %s must be newer than updatedAt %s", got.ObservedAt, got.UpdatedAt)
	}
	updated, observed, err := s.LastUpdated(ctx, "monitor1")
	if err != nil || !observed.After(updated) {
		t.Fatalf("LastUpdated = %s / %s, %v", updated, observed, err)
	}
	if err := s.Observe(ctx, "monitor1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("observing a missing task = %v", err)
	}
}

func TestAcceptSeparatesIntakeFromResultAndSurvivesRetries(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	b, err := s.Bind(ctx, "parent1", "monitor1", "pocket-aide")
	if err != nil {
		t.Fatal(err)
	}
	cmd, created, err := s.Accept(ctx, b, "req1", `{"instruction":"watch #82"}`)
	if err != nil || !created || cmd.State != CommandAccepted {
		t.Fatalf("accept = %+v, created=%v, %v", cmd, created, err)
	}
	// Reconnect: the same request_id must not register a second command.
	again, created, err := s.Accept(ctx, b, "req1", `{"instruction":"watch #82"}`)
	if err != nil || created || again.ID != cmd.ID {
		t.Fatalf("retry = %+v, created=%v, %v", again, created, err)
	}
	if _, _, err := s.Accept(ctx, b, "req1", `{"instruction":"something else"}`); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("same id with a new payload = %v, want ErrRequestReused", err)
	}
	cmds, err := s.Commands(ctx, b)
	if err != nil || len(cmds) != 1 {
		t.Fatalf("Commands = %+v, %v; want exactly one", cmds, err)
	}

	if _, err := s.Progress(ctx, "monitor1", "req1", CommandRunning, ""); err != nil {
		t.Fatal(err)
	}
	// A restarted monitor repeating the transition must not re-run it.
	if c, err := s.Progress(ctx, "monitor1", "req1", CommandRunning, ""); err != nil || c.State != CommandRunning {
		t.Fatalf("repeated transition = %+v, %v", c, err)
	}
	if c, err := s.Progress(ctx, "monitor1", "req1", CommandDone, "delegated to task1"); err != nil || c.Result != "delegated to task1" {
		t.Fatalf("finish = %+v, %v", c, err)
	}
	// Done is terminal: nothing walks it back to running.
	if _, err := s.Progress(ctx, "monitor1", "req1", CommandRunning, ""); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("backwards transition = %v, want refusal", err)
	}
	// Another monitor cannot move someone else's command.
	if _, err := s.Progress(ctx, "monitor2", "req1", CommandFailed, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign monitor = %v, want ErrNotFound", err)
	}
}

func TestLastUpdatedIsZeroBeforeTheMonitorWrites(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	updated, observed, err := s.LastUpdated(ctx, "monitor1")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.IsZero() || !observed.IsZero() {
		t.Fatalf("an empty ledger must report zero times, got %s / %s", updated, observed)
	}
}

func TestInvalidArgumentsAreRefused(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.PutTask(ctx, "monitor1", "bad id", "req1", 0, "body"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("task id with a space = %v", err)
	}
	if _, err := s.PutTask(ctx, "monitor1", "task1", "req1", 0, ""); !errors.Is(err, ErrInvalidBody) {
		t.Fatalf("empty body = %v", err)
	}
}
