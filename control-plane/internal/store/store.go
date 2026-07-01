// Package store defines the StateStore port: the source of truth for session
// metadata and state. It is backend-neutral — the concrete adapter lives under
// internal/adapter (the ConfigMap + Lease implementation backs every operation
// with the Kubernetes API so transitions and occupancy are atomic across
// control plane replicas, AC-C1). The domain errors the contract returns
// (session.ErrConflict / session.ErrNotFound) stay in the session package.
package store

import (
	"context"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// StateStore is the source of truth for session metadata and state. Every
// state transition and occupancy claim must be atomic (AC-C1) so concurrent
// restore/snapshot/switch requests for the same session converge to a single
// valid state.
//
// AC mapping:
//   - Put/Get/List      → AC-C2/C3 (read/write dispatch needs current state),
//     V5 (single source of truth for session metadata).
//   - CompareAndSwapState→ AC-C1 (atomic transitions, no torn state).
//   - Lock/Unlock       → AC-C1 (single in-flight mutation per session).
type StateStore interface {
	Put(ctx context.Context, s *session.Session) error
	Get(ctx context.Context, id string) (*session.Session, error)
	List(ctx context.Context) ([]*session.Session, error)
	Delete(ctx context.Context, id string) error

	// CompareAndSwapState atomically moves a session from->to, returning
	// session.ErrConflict if the current state is not `from`.
	CompareAndSwapState(ctx context.Context, id string, from, to session.State) error

	// Lock acquires an exclusive, per-session advisory lock. token identifies
	// the holder so Unlock can be made safe. Returns session.ErrConflict if the
	// lock is already held.
	Lock(ctx context.Context, id, token string) error
	Unlock(ctx context.Context, id, token string) error
}
