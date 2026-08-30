package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

type accessBeforeSnapshotManager struct {
	*service.Service
	beforeSnapshot func()
}

func (m *accessBeforeSnapshotManager) SnapshotIfIdle(ctx context.Context, id string, cutoff time.Time) (*session.Session, bool, error) {
	m.beforeSnapshot()
	return m.Service.SnapshotIfIdle(ctx, id, cutoff)
}

// TestIdleReaperSnapshotsIdleSessions covers the operational idle->snapshot
// trigger (AC-B1). A session idle for at least MaxIdle is checkpointed and its
// pod reclaimed (AC-A3); a session just under the threshold — the 59-minute
// boundary from docs/test/lifecycle.md Scenario 1 — is left active. It drives a
// single deterministic ScanOnce with an injected clock rather than waiting on
// the ticker, and ages each session relative to its own LastAccess so the
// assertions are independent of the wall clock.
func TestIdleReaperSnapshotsIdleSessions(t *testing.T) {
	ctx := context.Background()

	t.Run("idle past MaxIdle is snapshotted and pod reclaimed", func(t *testing.T) {
		svc := newService()
		sess, err := svc.Create(ctx, session.CreateRequest{Name: "idle-job"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		clock := func() time.Time { return sess.LastAccess.Add(session.MaxIdle + time.Minute) }
		reaper := service.NewIdleReaper(svc, session.MaxIdle, time.Hour, clock, nil)

		n, err := reaper.ScanOnce(ctx)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 1 {
			t.Fatalf("snapshotted = %d, want 1", n)
		}
		got, err := svc.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.State != session.StateSnapshot {
			t.Errorf("state = %q, want snapshot (AC-B1)", got.State)
		}
		if got.Pod != "" {
			t.Error("snapshot should reclaim the pod (AC-A3)")
		}
	})

	t.Run("idle under MaxIdle stays active (59-minute boundary)", func(t *testing.T) {
		svc := newService()
		sess, err := svc.Create(ctx, session.CreateRequest{Name: "recently-used"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		clock := func() time.Time { return sess.LastAccess.Add(session.MaxIdle - time.Minute) }
		reaper := service.NewIdleReaper(svc, session.MaxIdle, time.Hour, clock, nil)

		n, err := reaper.ScanOnce(ctx)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 0 {
			t.Fatalf("snapshotted = %d, want 0", n)
		}
		got, err := svc.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.State != session.StateActive {
			t.Errorf("state = %q, want active (below idle threshold)", got.State)
		}
	})

	t.Run("access after List is rechecked under snapshot Lease", func(t *testing.T) {
		svc, store, _ := newServiceWithStore()
		sess, err := svc.Create(ctx, session.CreateRequest{Name: "list-race"})
		if err != nil {
			t.Fatal(err)
		}
		now := sess.LastAccess.Add(session.MaxIdle + time.Minute)
		mgr := &accessBeforeSnapshotManager{
			Service: svc,
			beforeSnapshot: func() {
				if err := store.Touch(ctx, sess.ID, now); err != nil {
					t.Errorf("touch between List and SnapshotIfIdle: %v", err)
				}
			},
		}
		reaper := service.NewIdleReaper(
			mgr, session.MaxIdle, time.Hour, func() time.Time { return now }, nil,
		)
		n, err := reaper.ScanOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("snapshotted=%d, want 0 after recent access", n)
		}
		stored, err := store.Get(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != session.StateActive || stored.Pod != sess.Pod || stored.Checkpoint != nil {
			t.Fatalf("recent session was frozen: %+v", stored)
		}
	})
}
