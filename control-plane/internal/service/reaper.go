package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// IdleReaper is the operational idle->snapshot trigger (AC-B1). On a fixed
// interval it scans every session and snapshots any that has had no client
// read/write (AC-D5) for at least maxIdle — checkpointing it via CRIU and
// reclaiming its pod (AC-A3). It is the operational counterpart to the
// test-only /snapshot endpoint: without it a session never freezes on its own
// after its idle limit; a client had to trigger the snapshot explicitly.
//
// It reuses session.Manager.Snapshot unchanged. Snapshot is lock-guarded and
// idempotent — it re-reads state under the per-session lock and no-ops a
// session already in snapshot — so a candidate that a concurrent access resumes
// between the List scan and the Snapshot call is handled safely.
//
// Scope: this implements only the plain "idle >= maxIdle -> snapshot" rule. The
// finer trigger *policy* — grace periods, per-session overrides, and whether to
// freeze a shell running a long foreground job that has merely gone
// client-idle (AC-D5, see docs/prd/shell-workload.md) — remains the deferred
// TODO(policy) in package session and is out of scope here.
type IdleReaper struct {
	mgr      session.Manager
	maxIdle  time.Duration
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// NewIdleReaper builds a reaper over the session manager. now defaults to
// time.Now().UTC() and log to slog.Default() when nil; tests inject a
// controllable clock to age sessions past maxIdle deterministically.
func NewIdleReaper(mgr session.Manager, maxIdle, interval time.Duration, now func() time.Time, log *slog.Logger) *IdleReaper {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if log == nil {
		log = slog.Default()
	}
	return &IdleReaper{mgr: mgr, maxIdle: maxIdle, interval: interval, now: now, log: log}
}

// Run scans once per interval until ctx is cancelled (the process shutdown
// context), so the reaper stops cleanly on SIGINT/SIGTERM.
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
// one bad session does not block the rest; only a failure to list sessions
// aborts the scan. It is exported so a single deterministic tick can be driven
// directly (tests, or a future one-shot mode).
func (r *IdleReaper) ScanOnce(ctx context.Context) (int, error) {
	sessions, err := r.mgr.List(ctx)
	if err != nil {
		r.log.Warn("idle reaper: list sessions failed", "err", err)
		return 0, err
	}
	now := r.now()
	snapshotted := 0
	for _, sess := range sessions {
		if sess.State == session.StateSnapshot {
			continue // already frozen
		}
		idle := sess.IdleFor(now)
		if idle < r.maxIdle {
			continue // still within the idle budget (e.g. the 59-minute boundary)
		}
		if _, err := r.mgr.Snapshot(ctx, sess.ID); err != nil {
			r.log.Warn("idle reaper: snapshot failed", "session", sess.ID, "idle", idle.String(), "err", err)
			continue
		}
		snapshotted++
		r.log.Info("idle reaper: snapshotted idle session (AC-B1)", "session", sess.ID, "idle", idle.String())
	}
	return snapshotted, nil
}
