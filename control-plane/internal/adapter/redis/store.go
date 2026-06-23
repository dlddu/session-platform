// Package redis contains the StateStore port and an in-memory stub. The real
// implementation will back these operations with Redis so state transitions
// and session occupancy are atomic across control plane replicas (AC-C1).
package redis

import (
	"context"
	"sync"

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

// StubStore is a thread-safe, in-memory StateStore. The mutex stands in for
// Redis atomicity; swapping in a real Redis client keeps the same contract.
type StubStore struct {
	mu    sync.Mutex
	data  map[string]session.Session
	locks map[string]string // id -> holder token
	// addr is the configured Redis address, kept so the real client can be
	// wired in later without changing call sites.
	addr string
}

// NewStubStore returns an empty in-memory store. addr is recorded for the
// future real client but unused by the stub.
//
// TODO(go-redis): replace StubStore with a Redis-backed implementation:
//   - Put/Get/List → HSET/HGETALL on a `session:{id}` hash + a `sessions` set.
//   - CompareAndSwapState → a Lua script (GET state, compare, SET) for atomicity.
//   - Lock/Unlock → SET NX PX + a compare-and-del Lua script (AC-C1).
func NewStubStore(addr string) *StubStore {
	return &StubStore{data: map[string]session.Session{}, locks: map[string]string{}, addr: addr}
}

func (s *StubStore) Put(_ context.Context, sess *session.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[sess.ID] = *sess
	return nil
}

func (s *StubStore) Get(_ context.Context, id string) (*session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	cp := v
	return &cp, nil
}

func (s *StubStore) List(_ context.Context) ([]*session.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session.Session, 0, len(s.data))
	for _, v := range s.data {
		cp := v
		out = append(out, &cp)
	}
	return out, nil
}

func (s *StubStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	return nil
}

func (s *StubStore) CompareAndSwapState(_ context.Context, id string, from, to session.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	if !ok {
		return session.ErrNotFound
	}
	if v.State != from {
		return session.ErrConflict
	}
	v.State = to
	s.data[id] = v
	return nil
}

func (s *StubStore) Lock(_ context.Context, id, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.locks[id]; held {
		return session.ErrConflict
	}
	s.locks[id] = token
	return nil
}

func (s *StubStore) Unlock(_ context.Context, id, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[id] == token {
		delete(s.locks, id)
	}
	return nil
}
