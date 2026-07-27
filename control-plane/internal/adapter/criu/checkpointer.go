// Package criu contains the Checkpointer port, a gated no-op stub, and the real
// K8s-native adapter (ContainerCheckpointer, see container_checkpointer.go).
// CRIU-based checkpoint/restore is non-trivial (K8s ContainerCheckpoint is alpha
// and "restore into a new pod" is even less mature), so it sits behind a feature
// gate: with CRIU_ENABLED off the stub succeeds as a no-op, letting the happy
// path run without CRIU; with it on, main injects the real ContainerCheckpointer
// (unverified until a CRIU-capable runtime is provisioned — docs/criu-verification.md).
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

// StubCheckpointer is the gate-off no-op: it returns synthetic checkpoint
// metadata so the snapshot/restore flow is exercisable without a CRIU-capable
// runtime. main injects it with enabled=false when CRIU_ENABLED is off; the
// gate-on path uses the real ContainerCheckpointer instead.
type StubCheckpointer struct {
	enabled bool
}

// NewStubCheckpointer returns the no-op checkpointer. main passes enabled=false
// (the gate-on path swaps in the real ContainerCheckpointer), but the flag is
// kept so the stub can still report Enabled() for tests that want it.
func NewStubCheckpointer(enabled bool) *StubCheckpointer {
	return &StubCheckpointer{enabled: enabled}
}

func (c *StubCheckpointer) Enabled() bool { return c.enabled }

func (c *StubCheckpointer) Checkpoint(_ context.Context, _ k8s.PodRef) (*session.Checkpoint, error) {
	// No-op: the real kubelet ContainerCheckpoint drive lives in
	// ContainerCheckpointer.Checkpoint (container_checkpointer.go). This stub just
	// keeps the snapshot/reclaim flow running without a CRIU runtime.
	return &session.Checkpoint{
		Ref:       "stub-checkpoint",
		SizeBytes: 0,
		CreatedAt: time.Now().UTC(),
		Reclaimed: "stub",
	}, nil
}

func (c *StubCheckpointer) Restore(_ context.Context, _ *session.Checkpoint, _ k8s.PodRef) error {
	// No-op: the real restore lives in ContainerCheckpointer.Restore. The stub
	// leaves the freshly-provisioned pod as-is (a fresh shell), which is why the
	// gate-off path does not preserve pre-snapshot state.
	return nil
}
