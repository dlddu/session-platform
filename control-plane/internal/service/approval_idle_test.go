package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// docs/test/approval-gated-workload.md scenario 5, as far as it can be pinned
// without a cluster: an approval-gated session waiting on a human does not have
// its idle count advance, the count resumes once the wait ends, and shell and
// claude-code sessions in the same condition freeze exactly as before.

// approvalReportingAgent is the workload I/O stub plus the one extra answer
// the idle path asks for. It is a separate type rather than a widened
// agent.Client so that every other fake in this package stays untouched.
type approvalReportingAgent struct {
	*agent.StubClient
	mu       sync.Mutex
	awaiting map[string]bool
	err      error
	asked    map[string]int
}

func newApprovalReportingAgent() *approvalReportingAgent {
	return &approvalReportingAgent{
		StubClient: agent.NewStubClient(),
		awaiting:   map[string]bool{},
		asked:      map[string]int{},
	}
}

func (a *approvalReportingAgent) AwaitingApproval(_ context.Context, pod string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked[pod]++
	if a.err != nil {
		return false, a.err
	}
	return a.awaiting[pod], nil
}

func (a *approvalReportingAgent) setAwaiting(pod string, waiting bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.awaiting[pod] = waiting
}

func (a *approvalReportingAgent) timesAsked(pod string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked[pod]
}

// testClock is the "시간을 제어할 수 있는 테스트 하네스" the scenario asks for.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newApprovalIdleService builds a service whose clock the test owns and which
// can freeze every workload type, so a type that stays active is doing so
// because of AC-F3 and not because it had no snapshot strategy.
func newApprovalIdleService(clock *testClock, ag agent.Client) *service.Service {
	stub := criu.NewStubCheckpointer(true)
	return service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		stub,
		ag,
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, stub),
		service.WithWorkloadCheckpointer(session.WorkloadTypeApprovalGated, stub),
		service.WithClock(clock.Now),
	)
}

func mustCreate(t *testing.T, svc *service.Service, workload session.WorkloadType) *session.Session {
	t.Helper()
	sess, err := svc.Create(context.Background(), session.CreateRequest{
		Name:         "gated-" + string(workload),
		WorkloadType: workload,
	})
	if err != nil {
		t.Fatalf("create %s session: %v", workload, err)
	}
	return sess
}

func scan(t *testing.T, svc *service.Service, clock *testClock) int {
	t.Helper()
	reaper := service.NewIdleReaper(svc, session.MaxIdle, time.Hour, clock.Now, nil)
	n, err := reaper.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("reaper scan: %v", err)
	}
	return n
}

func mustGet(t *testing.T, svc *service.Service, id string) *session.Session {
	t.Helper()
	got, err := svc.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}

// The requirement itself: while a human is being waited on, lastAccess is
// refreshed with no client I/O at all and the session does not freeze; once
// the decision lands the refresh stops and the ordinary count resumes from
// there, freezing the session when it next reaches the limit.
func TestApprovalWaitHoldsTheIdleCountAndReleasesIt(t *testing.T) {
	start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := &testClock{now: start}
	ag := newApprovalReportingAgent()
	svc := newApprovalIdleService(clock, ag)

	sess := mustCreate(t, svc, session.WorkloadTypeApprovalGated)
	ag.setAwaiting(sess.Pod, true)

	// Past the limit with nobody touching the session: without AC-F3 this is
	// exactly the shape that freezes.
	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 0 {
		t.Fatalf("snapshotted = %d while an approval was pending, want 0 (AC-F3)", n)
	}
	held := mustGet(t, svc, sess.ID)
	if held.State != session.StateActive {
		t.Fatalf("state = %q while awaiting approval, want active", held.State)
	}
	if !held.LastAccess.Equal(clock.Now()) {
		t.Fatalf("lastAccess = %s, want it refreshed to %s while waiting", held.LastAccess, clock.Now())
	}
	if held.IdleFor(clock.Now()) != 0 {
		t.Fatalf("idle = %s while waiting, want the count held at zero", held.IdleFor(clock.Now()))
	}

	// Still waiting an hour later: the count keeps being held, not merely
	// deferred once.
	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 0 {
		t.Fatalf("snapshotted = %d on the second scan while still waiting, want 0", n)
	}

	// The human decides. Nothing else changes — no client read or write.
	ag.setAwaiting(sess.Pod, false)
	decidedAt := clock.Now()
	if n := scan(t, svc, clock); n != 0 {
		t.Fatalf("snapshotted = %d immediately after the decision, want 0: the count restarts from the last refresh, it does not backdate", n)
	}
	if got := mustGet(t, svc, sess.ID); !got.LastAccess.Equal(decidedAt) {
		t.Fatalf("lastAccess = %s after the decision, want the refresh to have stopped at %s", got.LastAccess, decidedAt)
	}

	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 1 {
		t.Fatalf("snapshotted = %d after the wait ended and the limit was reached, want 1 (AC-B1)", n)
	}
	frozen := mustGet(t, svc, sess.ID)
	if frozen.State != session.StateSnapshot {
		t.Fatalf("state = %q, want snapshot once the ordinary count reached the limit", frozen.State)
	}
	if frozen.Pod != "" {
		t.Error("the pod should be reclaimed like any other idle session (AC-A3)")
	}
}

// The control group the scenario names explicitly: shell and claude-code have
// no approval concept, so nothing about AC-D5/AC-B1 changes for them — not
// even the question being asked.
func TestOtherWorkloadTypesKeepTheOrdinaryIdleRule(t *testing.T) {
	for _, workload := range []session.WorkloadType{
		session.WorkloadTypeShell,
		session.WorkloadTypeClaudeCode,
	} {
		t.Run(string(workload), func(t *testing.T) {
			start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			clock := &testClock{now: start}
			ag := newApprovalReportingAgent()
			svc := newApprovalIdleService(clock, ag)

			sess := mustCreate(t, svc, workload)
			// Even with an agent that would claim a wait, this type must freeze.
			ag.setAwaiting(sess.Pod, true)

			clock.advance(session.MaxIdle + time.Minute)
			if n := scan(t, svc, clock); n != 1 {
				t.Fatalf("snapshotted = %d, want 1: %s keeps AC-D5/AC-B1 unchanged", n, workload)
			}
			if got := mustGet(t, svc, sess.ID); got.State != session.StateSnapshot {
				t.Fatalf("state = %q, want snapshot", got.State)
			}
			if asked := ag.timesAsked(sess.Pod); asked != 0 {
				t.Errorf("the idle path asked a %s session about approvals %d times, want 0", workload, asked)
			}
		})
	}
}

// AC-F3's "동결·삭제와의 충돌": the exception belongs to the idle trigger only.
// A user asking to freeze a session gets it frozen, wait or no wait.
func TestManualSnapshotStillFreezesDuringAnApprovalWait(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	ag := newApprovalReportingAgent()
	svc := newApprovalIdleService(clock, ag)

	sess := mustCreate(t, svc, session.WorkloadTypeApprovalGated)
	ag.setAwaiting(sess.Pod, true)

	frozen, err := svc.Snapshot(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if frozen.State != session.StateSnapshot {
		t.Fatalf("state = %q, want snapshot: a user-driven freeze is not subject to the idle exception", frozen.State)
	}
	if asked := ag.timesAsked(sess.Pod); asked != 0 {
		t.Errorf("an explicit snapshot asked about approvals %d times, want 0", asked)
	}
}

// An agent that cannot be asked must not be able to pin a pod. The idle rule
// falls back to what it did before AC-F3 existed.
func TestUnreadableApprovalStateFallsBackToTheOrdinaryIdleRule(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	ag := newApprovalReportingAgent()
	ag.err = errors.New("agent unreachable")
	svc := newApprovalIdleService(clock, ag)

	sess := mustCreate(t, svc, session.WorkloadTypeApprovalGated)

	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 1 {
		t.Fatalf("snapshotted = %d when the approval state could not be read, want 1", n)
	}
	if got := mustGet(t, svc, sess.ID); got.State != session.StateSnapshot {
		t.Fatalf("state = %q, want snapshot", got.State)
	}
}

// An agent client that cannot report at all — the shape every existing fake in
// this package has — leaves the idle rule exactly as it was.
func TestAgentWithoutApprovalReportingKeepsTheOrdinaryIdleRule(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	svc := newApprovalIdleService(clock, agent.NewStubClient())

	sess := mustCreate(t, svc, session.WorkloadTypeApprovalGated)

	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 1 {
		t.Fatalf("snapshotted = %d with a client that cannot report approvals, want 1", n)
	}
	_ = sess
}
