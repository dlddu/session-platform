package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// docs/test/session-scheduler.md scenario 3, as far as it can be pinned
// without a cluster.

type scheduleReportingAgent struct {
	*agent.StubClient
	mu     sync.Mutex
	active map[string]int
	err    error
	asked  map[string]int
}

func newScheduleReportingAgent() *scheduleReportingAgent {
	return &scheduleReportingAgent{StubClient: agent.NewStubClient(), active: map[string]int{}, asked: map[string]int{}}
}

func (a *scheduleReportingAgent) ActiveSchedules(_ context.Context, pod string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked[pod]++
	if a.err != nil {
		return 0, a.err
	}
	return a.active[pod], nil
}

func (a *scheduleReportingAgent) set(pod string, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active[pod] = n
}

func (a *scheduleReportingAgent) timesAsked(pod string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked[pod]
}

func newScheduleIdleService(clock *testClock, ag agent.Client) *service.Service {
	stub := archive.NewStubCheckpointer(true)
	return service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		stub,
		ag,
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, stub),
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexChat, stub),
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexTask, stub),
		service.WithAuthPool(session.WorkloadTypeCodexChat, k8s.NewMemoryAuthPool(2)),
		service.WithAuthPool(session.WorkloadTypeCodexTask, k8s.NewMemoryAuthPool(2)),
		service.WithClock(clock.Now),
	)
}

// AC-P3 itself: the hold while schedules remain, and its release.
func TestSchedulesHoldTheIdleCountAndReleaseIt(t *testing.T) {
	for _, workload := range []session.WorkloadType{session.WorkloadTypeCodexChat, session.WorkloadTypeCodexTask} {
		t.Run(string(workload), func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
			ag := newScheduleReportingAgent()
			svc := newScheduleIdleService(clock, ag)
			sess := mustCreate(t, svc, workload)
			ag.set(sess.Pod, 1)

			clock.advance(session.MaxIdle + time.Minute)
			if n := scan(t, svc, clock); n != 0 {
				t.Fatalf("snapshotted = %d while a schedule remained, want 0 (AC-P3)", n)
			}
			held := mustGet(t, svc, sess.ID)
			if held.State != session.StateActive || !held.LastAccess.Equal(clock.Now()) {
				t.Fatalf("state = %q lastAccess = %s, want active and refreshed to %s", held.State, held.LastAccess, clock.Now())
			}

			ag.set(sess.Pod, 0)
			clock.advance(session.MaxIdle + time.Minute)
			if n := scan(t, svc, clock); n != 1 {
				t.Fatalf("snapshotted = %d after the schedules ended, want 1 (AC-B1)", n)
			}
			if got := mustGet(t, svc, sess.ID); got.State != session.StateSnapshot {
				t.Fatalf("state = %q, want snapshot", got.State)
			}
		})
	}
}

// Other types are never asked, whatever an agent would claim.
func TestNonCodexTypesAreNotAskedAboutSchedules(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
	ag := newScheduleReportingAgent()
	svc := newScheduleIdleService(clock, ag)
	sess := mustCreate(t, svc, session.WorkloadTypeClaudeCode)
	ag.set(sess.Pod, 3)

	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 1 {
		t.Fatalf("snapshotted = %d, want 1: claude-code keeps AC-B1 unchanged", n)
	}
	if asked := ag.timesAsked(sess.Pod); asked != 0 {
		t.Errorf("asked a claude-code session about schedules %d times", asked)
	}
}

// The exception belongs to the idle trigger only, and an unreadable answer
// cannot pin a pod.
func TestScheduleHoldIsReaperOnlyAndFailsOpen(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
	ag := newScheduleReportingAgent()
	svc := newScheduleIdleService(clock, ag)

	manual := mustCreate(t, svc, session.WorkloadTypeCodexChat)
	ag.set(manual.Pod, 1)
	frozen, err := svc.Snapshot(context.Background(), manual.ID)
	if err != nil || frozen.State != session.StateSnapshot {
		t.Fatalf("manual snapshot = %v, %v; want snapshot despite schedules", frozen, err)
	}
	if asked := ag.timesAsked(manual.Pod); asked != 0 {
		t.Errorf("an explicit snapshot asked about schedules %d times", asked)
	}

	unreadable := mustCreate(t, svc, session.WorkloadTypeCodexTask)
	ag.err = errors.New("sidecar unreachable")
	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 1 {
		t.Fatalf("snapshotted = %d when the schedule count could not be read, want 1", n)
	}
	if got := mustGet(t, svc, unreadable.ID); got.State != session.StateSnapshot {
		t.Fatalf("state = %q, want snapshot", got.State)
	}
}
