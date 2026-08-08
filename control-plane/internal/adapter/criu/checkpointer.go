// Package criu contains the historical Checkpointer port and its workload
// snapshot strategies. A disabled stub reports Enabled=false, and the service
// fails closed before reclaiming a pod. Production shell
// snapshots use agent-driven CRIU; claude-code uses a filesystem archive.
package criu

import (
	"context"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Checkpointer archives a session workload and restores it into a new pod.
//
// AC mapping:
//   - Checkpoint → AC-B1 (snapshot on idle), AC-A3 (pod then reclaimed),
//     AC-B3 (in-memory state preserved).
//   - Restore    → AC-B2 (restore into a new pod on access), AC-B3 (integrity).
type Checkpointer interface {
	// Enabled reports whether the real durable snapshot strategy is active.
	Enabled() bool
	// Checkpoint captures the workload and returns its durable archive ref.
	Checkpoint(ctx context.Context, ref k8s.PodRef) (*session.Checkpoint, error)
	// Restore applies an archive to an already-provisioned restore pod.
	Restore(ctx context.Context, cp *session.Checkpoint, into k8s.PodRef) error
}

// StubCheckpointer is a no-op test adapter. main injects it with enabled=false
// when CRIU_ENABLED is off; Service.checkpointerFor rejects it before
// Checkpoint, so a gate-off deployment never deletes a live pod behind
// synthetic metadata. Tests use enabled=true to exercise lifecycle flow.
type StubCheckpointer struct {
	enabled bool
}

// NewStubCheckpointer returns the no-op checkpointer. main passes enabled=false
// (the gate-on path swaps in an agent-driven strategy), but the flag is
// kept so the stub can still report Enabled() for tests that want it.
func NewStubCheckpointer(enabled bool) *StubCheckpointer {
	return &StubCheckpointer{enabled: enabled}
}

func (c *StubCheckpointer) Enabled() bool { return c.enabled }

func (c *StubCheckpointer) Checkpoint(_ context.Context, _ k8s.PodRef) (*session.Checkpoint, error) {
	// No-op: production uses AgentCheckpointer to stream a workload archive from
	// the pod. This stub keeps isolated lifecycle tests independent of a cluster
	// and durable archive store.
	return &session.Checkpoint{
		Ref:       "stub-checkpoint",
		SizeBytes: 0,
		CreatedAt: time.Now().UTC(),
		Reclaimed: "stub",
	}, nil
}

// CheckpointWithGeneration makes the enabled test stub exercise the same
// durable-generation service protocol as the Claude archive adapter.
func (c *StubCheckpointer) CheckpointWithGeneration(ctx context.Context, ref k8s.PodRef, generation string) (*session.Checkpoint, error) {
	cp, err := c.Checkpoint(ctx, ref)
	if cp != nil {
		cp.AbortToken = generation
	}
	return cp, err
}

func (c *StubCheckpointer) AbortCheckpoint(context.Context, k8s.PodRef, *session.Checkpoint) error {
	return nil
}

func (c *StubCheckpointer) Restore(_ context.Context, _ *session.Checkpoint, _ k8s.PodRef) error {
	// No-op: production uses an agent-driven workload restore strategy. The
	// stub exists only for isolated lifecycle tests; gate-off production paths
	// fail closed before reclaiming a live pod.
	return nil
}
