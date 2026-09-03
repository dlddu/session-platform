// Package store defines the StateStore port: the backend-neutral source of
// truth for session metadata and state.
package store

import (
	"context"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// StateStore is the source of truth for session metadata and state. Every
// state transition and occupancy claim must be atomic.
type StateStore interface {
	Put(ctx context.Context, s *session.Session) error
	Get(ctx context.Context, id string) (*session.Session, error)
	List(ctx context.Context) ([]*session.Session, error)
	// Delete removes session metadata only when token owns the lifecycle fence.
	Delete(ctx context.Context, id, token string) error

	// Touch advances LastAccess without replacing lifecycle or recovery metadata.
	Touch(ctx context.Context, id string, at time.Time) error
	// CompareAndSwapState atomically moves a session from->to, returning
	// session.ErrConflict if the current state is not `from`.
	CompareAndSwapState(ctx context.Context, id string, from, to session.State) error
	// CompareAndSwapSession atomically replaces the whole aggregate only when its
	// state and snapshot transaction still match the caller's expected values.
	CompareAndSwapSession(ctx context.Context, id, token string, expectedState session.State,
		expectedTxn *session.SnapshotTransaction, next *session.Session) error

	// Lock acquires an exclusive, per-session advisory lock fenced by token.
	// Returns session.ErrConflict if the lock is already held.
	Lock(ctx context.Context, id, token string) error
	// Renew extends a held lock only when token is still its owner.
	Renew(ctx context.Context, id, token string) error
	Unlock(ctx context.Context, id, token string) error
}
