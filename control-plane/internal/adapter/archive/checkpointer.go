// Package archive contains the Checkpointer port and its workload snapshot
// strategy: every workload type freezes into a generation-fenced filesystem
// archive produced by its pod's agent. A disabled stub reports Enabled=false,
// and the service fails closed before reclaiming a pod.
package archive

import (
	"context"
	"io"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Checkpointer archives a session workload and restores it into a new pod.
//
// AC mapping:
//   - CheckpointWithGeneration → AC-B1 (snapshot on idle), AC-A3 (pod then
//     reclaimed), AC-B3 (preserved state).
//   - AbortCheckpoint → reopens the source workload when the durable
//     transaction rolls back instead of committing.
//   - Restore → AC-B2 (restore into a new pod on access), AC-B3 (integrity).
type Checkpointer interface {
	// Enabled reports whether the real durable snapshot strategy is active.
	Enabled() bool
	// CheckpointWithGeneration closes the workload's admission barrier under a
	// control-plane generation that is already durably recorded, and returns
	// the durable archive ref.
	CheckpointWithGeneration(ctx context.Context, ref k8s.PodRef, generation string) (*session.Checkpoint, error)
	// AbortCheckpoint reopens the workload for the generation in cp.AbortToken.
	AbortCheckpoint(ctx context.Context, ref k8s.PodRef, cp *session.Checkpoint) error
	// Restore applies an archive to an already-provisioned restore pod.
	Restore(ctx context.Context, cp *session.Checkpoint, into k8s.PodRef) error
}

// CheckpointStore is a durable object store for checkpoint archives: the
// AgentCheckpointer streams the pod agent's archive through Put and fetches it
// back with Get. Defined consumer-side so this package carries no cloud SDK
// dependency; the S3 implementation lives in internal/adapter/checkpointstore.
type CheckpointStore interface {
	// Put uploads the archive read from r under key and returns its durable ref.
	Put(ctx context.Context, key string, r io.Reader) (ref string, err error)
	// Get opens the archive at ref (as returned by Put) for reading.
	Get(ctx context.Context, ref string) (io.ReadCloser, error)
}

// StubCheckpointer is a no-op test adapter. main injects it with enabled=false
// for a workload whose archive gate is off; Service.checkpointerFor rejects it
// before CheckpointWithGeneration. Tests use enabled=true to exercise lifecycle
// flow.
type StubCheckpointer struct {
	enabled bool
}

var _ Checkpointer = (*StubCheckpointer)(nil)

// NewStubCheckpointer returns the no-op checkpointer.
func NewStubCheckpointer(enabled bool) *StubCheckpointer {
	return &StubCheckpointer{enabled: enabled}
}

func (c *StubCheckpointer) Enabled() bool { return c.enabled }

func (c *StubCheckpointer) CheckpointWithGeneration(_ context.Context, _ k8s.PodRef, generation string) (*session.Checkpoint, error) {
	return &session.Checkpoint{
		Ref:        "stub-checkpoint",
		SizeBytes:  0,
		CreatedAt:  time.Now().UTC(),
		Reclaimed:  "stub",
		AbortToken: generation,
	}, nil
}

func (c *StubCheckpointer) AbortCheckpoint(context.Context, k8s.PodRef, *session.Checkpoint) error {
	return nil
}

func (c *StubCheckpointer) Restore(_ context.Context, _ *session.Checkpoint, _ k8s.PodRef) error {
	return nil
}
