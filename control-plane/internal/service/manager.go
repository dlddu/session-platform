// Package service wires the adapter ports together into a concrete
// session.Manager. It implements the happy path (create -> active, list,
// switch) with stub adapters and leaves the state-dependent read/write/snapshot
// policy decisions marked with TODO(policy).
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/redis"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Service is the concrete Manager. It owns no workload itself (AC-A1) — every
// pod operation goes through the orchestrator, every state mutation through the
// store, and every checkpoint through the checkpointer.
type Service struct {
	orch   k8s.PodOrchestrator
	store  redis.StateStore
	ckpt   criu.Checkpointer
	region string
	now    func() time.Time // injectable clock for tests
}

// New builds a Service from its adapter ports.
func New(orch k8s.PodOrchestrator, store redis.StateStore, ckpt criu.Checkpointer, region string) *Service {
	if region == "" {
		region = "us-east-1"
	}
	return &Service{orch: orch, store: store, ckpt: ckpt, region: region, now: func() time.Time { return time.Now().UTC() }}
}

// compile-time assertion that Service satisfies the port.
var _ session.Manager = (*Service)(nil)

func newID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create provisions a dedicated pod and registers the session as active
// (AC-A1, AC-A2).
func (s *Service) Create(ctx context.Context, req session.CreateRequest) (*session.Session, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, session.ErrInvalidInput
	}
	id := newID()
	region := req.Region
	if region == "" {
		region = s.region
	}

	pod, err := s.orch.Start(ctx, id)
	if err != nil {
		return nil, err
	}

	now := s.now()
	sess := &session.Session{
		ID:         id,
		Name:       name,
		State:      session.StateActive,
		Pod:        pod.Name,
		Region:     region,
		CreatedAt:  now,
		LastAccess: now,
	}
	if err := s.store.Put(ctx, sess); err != nil {
		// best-effort rollback of the pod we just started
		_ = s.orch.Stop(ctx, pod)
		return nil, err
	}
	return sess, nil
}

func (s *Service) Get(ctx context.Context, id string) (*session.Session, error) {
	return s.store.Get(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]*session.Session, error) {
	return s.store.List(ctx)
}

// Read dispatches on the session's state (AC-C2).
func (s *Service) Read(ctx context.Context, id string) (*session.ReadResult, error) {
	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &session.ReadResult{Session: sess}
	switch sess.State {
	case session.StateActive:
		res.Path = "active"
	case session.StateIdle:
		// TODO(policy: idle-read): read directly from the idle pod, or promote
		// to active first? Deferred product decision (AC-C2). Stub reads in place.
		res.Path = "idle->read-in-place"
	case session.StateSnapshot:
		// TODO(policy: snapshot-read): restore via CRIU then read, or return
		// snapshot metadata only? Deferred (AC-C2). Stub restores.
		if _, err := s.Restore(ctx, id); err != nil {
			return nil, err
		}
		res.Path = "snapshot->restore->read"
		sess, _ = s.store.Get(ctx, id)
		res.Session = sess
	default:
		return nil, session.ErrInvalidState
	}
	res.Payload = "stub:read:" + id
	s.touch(ctx, id)
	return res, nil
}

// Write dispatches on the session's state (AC-C3).
func (s *Service) Write(ctx context.Context, id, _ string) (*session.WriteResult, error) {
	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &session.WriteResult{Session: sess}
	switch sess.State {
	case session.StateActive:
		res.Path = "active"
	case session.StateIdle:
		// TODO(policy: idle-write): promote to active before write? Deferred (AC-C3).
		res.Path = "idle->write-in-place"
	case session.StateSnapshot:
		// TODO(policy: snapshot-write): allow (restore then write) or reject?
		// Deferred product decision (AC-C3). Stub restores then writes.
		if _, err := s.Restore(ctx, id); err != nil {
			return nil, err
		}
		res.Path = "snapshot->restore->write"
		sess, _ = s.store.Get(ctx, id)
		res.Session = sess
	default:
		return nil, session.ErrInvalidState
	}
	s.touch(ctx, id)
	return res, nil
}

// Switch makes the target session active, restoring it from a snapshot if
// needed; a no-op for already-active sessions (AC-C4).
func (s *Service) Switch(ctx context.Context, id string) (*session.Session, error) {
	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	switch sess.State {
	case session.StateActive:
		return sess, nil
	case session.StateIdle:
		// promote idle -> active (atomic).
		if err := s.store.CompareAndSwapState(ctx, id, session.StateIdle, session.StateActive); err != nil {
			return nil, err
		}
		return s.touchGet(ctx, id)
	case session.StateSnapshot:
		return s.Restore(ctx, id)
	default:
		return nil, session.ErrInvalidState
	}
}

// Snapshot checkpoints an active/idle session and reclaims its pod (AC-B1, AC-A3).
func (s *Service) Snapshot(ctx context.Context, id string) (*session.Session, error) {
	token := newID()
	if err := s.store.Lock(ctx, id, token); err != nil {
		return nil, err
	}
	defer s.store.Unlock(ctx, id, token)

	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.State == session.StateSnapshot {
		return sess, nil // already frozen
	}

	cp, err := s.ckpt.Checkpoint(ctx, k8s.PodRef{Name: sess.Pod, Namespace: ""})
	if err != nil {
		return nil, err
	}
	// reclaim the pod (AC-A3)
	if err := s.orch.Stop(ctx, k8s.PodRef{Name: sess.Pod}); err != nil {
		return nil, err
	}
	if err := s.store.CompareAndSwapState(ctx, id, sess.State, session.StateSnapshot); err != nil {
		return nil, err
	}
	sess.State = session.StateSnapshot
	sess.Pod = ""
	sess.Checkpoint = cp
	if err := s.store.Put(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// Restore restores a snapshotted session into a new pod and marks it active
// (AC-B2). Guarded by the per-session lock so concurrent restores are atomic
// (AC-C1).
func (s *Service) Restore(ctx context.Context, id string) (*session.Session, error) {
	token := newID()
	if err := s.store.Lock(ctx, id, token); err != nil {
		return nil, err
	}
	defer s.store.Unlock(ctx, id, token)

	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.State != session.StateSnapshot {
		return sess, nil // already live; nothing to restore
	}

	pod, err := s.orch.RestoreInto(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.ckpt.Restore(ctx, sess.Checkpoint, pod); err != nil {
		_ = s.orch.Stop(ctx, pod)
		return nil, err
	}
	if err := s.store.CompareAndSwapState(ctx, id, session.StateSnapshot, session.StateActive); err != nil {
		_ = s.orch.Stop(ctx, pod)
		return nil, err
	}
	now := s.now()
	sess.State = session.StateActive
	sess.Pod = pod.Name
	sess.Checkpoint = nil
	sess.LastAccess = now
	if err := s.store.Put(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// Terminate reclaims the pod (if any) and removes the session (AC-A3).
func (s *Service) Terminate(ctx context.Context, id string) error {
	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if sess.Pod != "" {
		if err := s.orch.Stop(ctx, k8s.PodRef{Name: sess.Pod}); err != nil {
			return err
		}
	}
	return s.store.Delete(ctx, id)
}

// touch updates LastAccess without changing state.
func (s *Service) touch(ctx context.Context, id string) {
	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return
	}
	sess.LastAccess = s.now()
	_ = s.store.Put(ctx, sess)
}

func (s *Service) touchGet(ctx context.Context, id string) (*session.Session, error) {
	s.touch(ctx, id)
	return s.store.Get(ctx, id)
}
