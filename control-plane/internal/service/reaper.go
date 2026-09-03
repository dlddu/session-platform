package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// IdleReaper is the operational idle->snapshot trigger. On a fixed interval it
// scans every session and snapshots any that has had no client read/write for at
// least maxIdle, using its workload snapshot strategy and reclaiming its pod. It
// is the automatic counterpart to the product /snapshot endpoint.
//
// Service.SnapshotIfIdle acquires the session Lease and reloads LastAccess,
// closing the stale List-to-Snapshot gap. Generic managers retain Snapshot.
type IdleReaper struct {
	mgr      session.Manager
	maxIdle  time.Duration
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger
}

type idleSnapshotManager interface {
	SnapshotIfIdle(context.Context, string, time.Time) (*session.Session, bool, error)
}

// NewIdleReaper builds a reaper over the session manager. now defaults to
// time.Now().UTC() and log to slog.Default() when nil.
func NewIdleReaper(mgr session.Manager, maxIdle, interval time.Duration, now func() time.Time, log *slog.Logger) *IdleReaper {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if log == nil {
		log = slog.Default()
	}
	return &IdleReaper{mgr: mgr, maxIdle: maxIdle, interval: interval, now: now, log: log}
}

// Run scans once per interval until ctx is cancelled.
func (r *IdleReaper) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.ScanOnce(ctx)
		}
	}
}

// ScanOnce snapshots every session whose idle time has reached maxIdle and
// returns how many it snapshotted. Per-session errors are logged and skipped so
// one bad session does not block the rest; only a failure to list sessions aborts
// the scan.
func (r *IdleReaper) ScanOnce(ctx context.Context) (int, error) {
	sessions, err := r.mgr.List(ctx)
	if err != nil {
		r.log.Warn("idle reaper: list sessions failed", "err", err)
		return 0, err
	}
	now := r.now()
	cutoff := now.Add(-r.maxIdle)
	snapshotted := 0
	for _, sess := range sessions {
		if sess.State == session.StateSnapshot {
			continue // already frozen
		}
		idle := sess.IdleFor(now)
		if idle < r.maxIdle {
			continue // still within the idle budget (e.g. the 59-minute boundary)
		}
		didSnapshot := true
		var snapshotErr error
		if guarded, ok := r.mgr.(idleSnapshotManager); ok {
			_, didSnapshot, snapshotErr = guarded.SnapshotIfIdle(ctx, sess.ID, cutoff)
		} else {
			_, snapshotErr = r.mgr.Snapshot(ctx, sess.ID)
		}
		if snapshotErr != nil {
			if errors.Is(snapshotErr, session.ErrCheckpointDisabled) {
				r.log.Debug("idle reaper: workload snapshot strategy disabled", "session", sess.ID)
				continue
			}
			r.log.Warn("idle reaper: snapshot failed", "session", sess.ID, "idle", idle.String(), "err", snapshotErr)
			continue
		}
		if !didSnapshot {
			continue
		}
		snapshotted++
		r.log.Info("idle reaper: snapshotted idle session (AC-B1)", "session", sess.ID, "idle", idle.String())
	}
	return snapshotted, nil
}
