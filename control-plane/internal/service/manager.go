// Package service wires the adapter ports together into a concrete
// session.Manager. Non-active access follows one uniform "resume-on-access"
// rule (AC-C2/AC-C3): read, write and switch all bring the session to active
// first — promoting idle->active (AC-C1) or restoring snapshot->active (AC-B2)
// — and then serve from the live pod. The dispatch policy is fully implemented;
// only the data-plane payload itself is stubbed until the data plane workload
// exists (see data-plane/README.md). The remaining TODO(policy) is the
// idle->snapshot *trigger* timing in package session, a separate decision.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/store"
)

// Service is the concrete Manager. It owns no workload itself (AC-A1) — every
// pod operation goes through the orchestrator, every state mutation through the
// store, and every checkpoint through the checkpointer.
type Service struct {
	orch  k8s.PodOrchestrator
	store store.StateStore
	ckpt  criu.Checkpointer
	now   func() time.Time // injectable clock for tests
}

// New builds a Service from its adapter ports.
func New(orch k8s.PodOrchestrator, store store.StateStore, ckpt criu.Checkpointer) *Service {
	return &Service{orch: orch, store: store, ckpt: ckpt, now: func() time.Time { return time.Now().UTC() }}
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

// activate ensures the target session is active so a read/write can be served
// from a live pod, and reports which branch brought it there. This is the
// shared core of the uniform "resume-on-access" policy (AC-C2/AC-C3): an active
// session is served in place, an idle one is atomically promoted (AC-C1), and a
// snapshot is restored via CRIU (AC-B2). Switch (AC-C4) is just activate with no
// following read/write.
func (s *Service) activate(ctx context.Context, id string) (*session.Session, string, error) {
	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	switch sess.State {
	case session.StateActive:
		return sess, "active", nil
	case session.StateIdle:
		// idle still holds its pod; resume it to active atomically (AC-C1).
		if err := s.store.CompareAndSwapState(ctx, id, session.StateIdle, session.StateActive); err != nil {
			return nil, "", err
		}
		sess, err = s.store.Get(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return sess, "idle->active", nil
	case session.StateSnapshot:
		// pod was reclaimed at freeze; restore the checkpoint into a fresh pod
		// and go active (AC-B2). Restore is lock-guarded for atomicity (AC-C1).
		sess, err = s.Restore(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return sess, "snapshot->restore", nil
	default:
		return nil, "", session.ErrInvalidState
	}
}

// Read brings the session active (per activate) and reads from its pod (AC-C2).
// The dispatch policy is fully implemented; only the returned payload is a stub
// until the data plane workload exists (see data-plane/README.md).
func (s *Service) Read(ctx context.Context, id string) (*session.ReadResult, error) {
	sess, branch, err := s.activate(ctx, id)
	if err != nil {
		return nil, err
	}
	s.touch(ctx, id)
	return &session.ReadResult{
		Session: sess,
		Path:    dispatchPath(branch, "read"),
		Payload: "stub:read:" + id, // real data comes from the data plane pod
	}, nil
}

// Write brings the session active (per activate) and writes to its pod (AC-C3).
// snapshot/idle are restored/promoted first rather than rejected, matching the
// uniform rule. Applying the payload to the workload is stubbed until the data
// plane agent exists (see data-plane/README.md).
func (s *Service) Write(ctx context.Context, id, payload string) (*session.WriteResult, error) {
	sess, branch, err := s.activate(ctx, id)
	if err != nil {
		return nil, err
	}
	_ = payload // applied to the workload once the data plane agent lands
	s.touch(ctx, id)
	return &session.WriteResult{
		Session: sess,
		Path:    dispatchPath(branch, "write"),
	}, nil
}

// Switch makes the target session active — promoting idle or restoring a
// snapshot as needed — and is a no-op for an already-active session (AC-C4).
// It shares the activate core with Read/Write so switching, reading and writing
// resume a session identically, and switching never breaks isolation (AC-A2).
func (s *Service) Switch(ctx context.Context, id string) (*session.Session, error) {
	sess, branch, err := s.activate(ctx, id)
	if err != nil {
		return nil, err
	}
	if branch == "idle->active" {
		// resuming from idle counts as an access; record it.
		return s.touchGet(ctx, id)
	}
	return sess, nil
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

// dispatchPath renders the ReadResult/WriteResult Path label for the branch that
// activate took. Active is served directly; non-active branches append the op,
// e.g. "idle->active->read" or "snapshot->restore->write".
func dispatchPath(branch, op string) string {
	if branch == "active" {
		return "active"
	}
	return branch + "->" + op
}
