package service_test

import (
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-G6: minimax-code sits behind the same gate as approval-gated, so a pending
// human decision holds its idle count exactly as AC-F3 does.
func TestMiniMaxCodeApprovalWaitHoldsTheIdleCount(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	ag := newApprovalReportingAgent()
	stub := criu.NewStubCheckpointer(true)
	svc := service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		stub,
		ag,
		service.WithWorkloadCheckpointer(session.WorkloadTypeMiniMaxCode, stub),
		service.WithClock(clock.Now),
	)

	sess := mustCreate(t, svc, session.WorkloadTypeMiniMaxCode)
	ag.setAwaiting(sess.Pod, true)

	clock.advance(session.MaxIdle + time.Minute)
	if n := scan(t, svc, clock); n != 0 {
		t.Fatalf("snapshotted = %d while an approval was pending, want 0", n)
	}
	held := mustGet(t, svc, sess.ID)
	if held.State != session.StateActive || !held.LastAccess.Equal(clock.Now()) {
		t.Fatalf("state = %q, lastAccess = %s, want active and refreshed to %s", held.State, held.LastAccess, clock.Now())
	}
	if ag.timesAsked(sess.Pod) == 0 {
		t.Fatal("the reaper never asked the minimax-code agent whether it was waiting")
	}

	ag.setAwaiting(sess.Pod, false)
	decidedAt := clock.Now()
	clock.advance(time.Minute)
	if n := scan(t, svc, clock); n != 0 {
		t.Fatalf("snapshotted = %d a minute after the decision, want 0", n)
	}
	if got := mustGet(t, svc, sess.ID); !got.LastAccess.Equal(decidedAt) {
		t.Fatalf("lastAccess = %s after the decision, want the refresh to have stopped at %s", got.LastAccess, decidedAt)
	}
}
