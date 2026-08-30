// Package store defines the StateStore port: the source of truth for session
// metadata and state. It is backend-neutral — the concrete adapter lives under
// internal/adapter (the ConfigMap + Lease implementation backs every operation
// with the Kubernetes API so transitions and occupancy are atomic across
// control plane replicas, AC-C1). The domain errors the contract returns
// (session.ErrConflict / session.ErrNotFound) stay in the session package.
package store

import (
	"context"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// StateStore is the source of truth for session metadata and state. Every
// state transition and occupancy claim must be atomic (AC-C1) so concurrent
// restore/snapshot/switch requests for the same session converge to a single
// valid state.
//
// AC mapping:
//   - Put/Get/List/Touch→ AC-C2/C3 (read/write dispatch needs current state),
//     V5 (single source of truth for session metadata).
//   - CompareAndSwapState→ AC-C1 (atomic transitions, no torn state).
//   - Lock/Renew/Unlock → AC-C1 (single in-flight mutation per session).
type StateStore interface {
	Put(ctx context.Context, s *session.Session) error
	Get(ctx context.Context, id string) (*session.Session, error)
	List(ctx context.Context) ([]*session.Session, error)
	// Delete removes session metadata only when token owns the lifecycle fence;
	// the caller keeps that lock held and releases it separately through Unlock.
	Delete(ctx context.Context, id, token string) error

	// Touch advances LastAccess on the latest stored object without replacing
	// lifecycle or recovery metadata from a stale read.
	Touch(ctx context.Context, id string, at time.Time) error
	// CompareAndSwapState atomically moves a session from->to, returning
	// session.ErrConflict if the current state is not `from`.
	CompareAndSwapState(ctx context.Context, id string, from, to session.State) error
	// CompareAndSwapSession atomically replaces the whole aggregate only when
	// both its lifecycle state and durable snapshot transaction still match the
	// caller's expected values.
	CompareAndSwapSession(ctx context.Context, id, token string, expectedState session.State,
		expectedTxn *session.SnapshotTransaction, next *session.Session) error

	// Lock acquires an exclusive, per-session advisory lock. token uniquely
	// identifies this acquisition so aggregate CAS/Delete and Unlock are fenced.
	// Returns session.ErrConflict if the lock is already held.
	Lock(ctx context.Context, id, token string) error
	// Renew extends a held lock only when token is still its owner. Long archive
	// transfers use it to prevent another replica recovering a live transaction.
	Renew(ctx context.Context, id, token string) error
	Unlock(ctx context.Context, id, token string) error
}
