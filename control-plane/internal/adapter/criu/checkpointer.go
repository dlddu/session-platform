// Package criu contains the Checkpointer port and a gated stub. CRIU-based
// checkpoint/restore is non-trivial (K8s ContainerCheckpoint is alpha and
// "restore into a new pod" is even less mature), so it sits behind a feature
// gate. With the gate off the stub succeeds as a no-op, letting the happy path
// run without CRIU; with it on, the real implementation would be required.
package criu

import (
	"context"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Checkpointer creates CRIU checkpoints of a session's pod and restores them.
//
// AC mapping:
//   - Checkpoint → AC-B1 (snapshot on idle), AC-A3 (pod then reclaimed),
//     AC-B3 (in-memory state preserved).
//   - Restore    → AC-B2 (restore into a new pod on access), AC-B3 (integrity).
type Checkpointer interface {
	// Enabled reports whether real CRIU checkpoint/restore is active.
	Enabled() bool
	// Checkpoint freezes the pod into a checkpoint image and returns its ref.
	Checkpoint(ctx context.Context, ref k8s.PodRef) (*session.Checkpoint, error)
	// Restore restores a checkpoint into an already-provisioned pod.
	Restore(ctx context.Context, cp *session.Checkpoint, into k8s.PodRef) error
}

// StubCheckpointer is gated by CRIU_ENABLED. When disabled it is a no-op that
// returns synthetic checkpoint metadata so the snapshot/restore flow is
// exercisable without a CRIU-capable runtime.
type StubCheckpointer struct {
	enabled bool
}

// NewStubCheckpointer returns a checkpointer. When enabled is true the stub
// still does not perform real CRIU work — it marks the spot where the real
// implementation must be plugged in and where verification is required.
func NewStubCheckpointer(enabled bool) *StubCheckpointer {
	return &StubCheckpointer{enabled: enabled}
}

func (c *StubCheckpointer) Enabled() bool { return c.enabled }

func (c *StubCheckpointer) Checkpoint(_ context.Context, _ k8s.PodRef) (*session.Checkpoint, error) {
	// TODO(criu): drive `kubectl checkpoint`/kubelet ContainerCheckpoint (alpha)
	// or runc checkpoint, push the image to storage, and return its real ref +
	// size (AC-B1, AC-B3). Verification environment is still TBD — see
	// docs/criu-verification.md.
	return &session.Checkpoint{
		Ref:       "stub-checkpoint",
		SizeBytes: 0,
		CreatedAt: time.Now().UTC(),
		Reclaimed: "stub",
	}, nil
}

func (c *StubCheckpointer) Restore(_ context.Context, _ *session.Checkpoint, _ k8s.PodRef) error {
	// TODO(criu): restore the checkpoint image into the target pod and confirm
	// the process resumes with its pre-snapshot in-memory state (AC-B2, AC-B3).
	return nil
}
