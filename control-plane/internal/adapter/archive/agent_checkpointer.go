// The wired agent-driven workload archive strategy. Every workload type —
// shell, claude-code, approval-gated, minimax-code — freezes through the same
// generation-fenced filesystem archive: the agent closes its admission barrier,
// streams a tar of its state, and the stream goes to durable storage to be
// streamed back later to the restore-target pod's agent.
package archive

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AgentCheckpointClient is the control plane's channel to a session pod's agent
// checkpoint endpoints. The agent HTTP client implements it, resolving the pod
// IP per call like the workload I/O client does.
type AgentCheckpointClient interface {
	// CheckpointWithGeneration uses a control-plane-owned, durably recorded
	// generation for a crash-recoverable filesystem archive transaction.
	CheckpointWithGeneration(ctx context.Context, pod, generation string) (archive io.ReadCloser, checkpointID string, err error)
	// Restore streams a workload archive to a restore-target pod's agent.
	Restore(ctx context.Context, pod string, archive io.Reader) error
	// AbortCheckpoint reopens a workload whose archive transfer completed but
	// the durable control-plane transaction failed.
	AbortCheckpoint(ctx context.Context, pod, checkpointID string) error
}

// AgentCheckpointer is the agent-driven Checkpointer. It requires a
// CheckpointStore: the archive is produced inside the pod that is about to be
// reclaimed, so it must be streamed to durable storage to survive.
type AgentCheckpointer struct {
	client AgentCheckpointClient
	store  CheckpointStore
	now    func() time.Time
}

var _ Checkpointer = (*AgentCheckpointer)(nil)

// NewAgentCheckpointer builds the agent-driven filesystem-archive checkpointer.
func NewAgentCheckpointer(client AgentCheckpointClient, store CheckpointStore) *AgentCheckpointer {
	return &AgentCheckpointer{
		client: client,
		store:  store,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Enabled reports that a real durable archive strategy is active.
func (c *AgentCheckpointer) Enabled() bool { return true }

// CheckpointWithGeneration is used by the service's durable snapshot
// transaction. The generation must already be stored before this method closes
// the data-plane admission barrier. A failure after the agent answered is left
// for the service to roll back: it owns the decision between abort and commit.
func (c *AgentCheckpointer) CheckpointWithGeneration(ctx context.Context, ref k8s.PodRef, generation string) (*session.Checkpoint, error) {
	if !validArchiveGeneration(generation) {
		return nil, fmt.Errorf("checkpoint pod %s: durable archive generation is unavailable", ref.Name)
	}
	if c.store == nil {
		return nil, fmt.Errorf("agent-driven checkpoint of pod %s requires a durable store (CHECKPOINT_S3_*)", ref.Name)
	}
	rc, checkpointID, err := c.client.CheckpointWithGeneration(ctx, ref.Name, generation)
	if err != nil {
		return nil, fmt.Errorf("checkpoint pod %s: %w", ref.Name, err)
	}
	if checkpointID != generation {
		_ = rc.Close()
		return nil, fmt.Errorf("checkpoint pod %s: agent returned generation %q, want %q", ref.Name, checkpointID, generation)
	}

	counting := &countingReader{r: rc}
	uri, err := c.store.Put(ctx, agentCheckpointKey(ref.Name, generation), counting)
	closeErr := rc.Close()
	if err != nil {
		return nil, fmt.Errorf("store checkpoint archive for pod %s: %w", ref.Name, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close checkpoint stream for pod %s: %w", ref.Name, closeErr)
	}
	return &session.Checkpoint{
		Ref:        uri,
		SizeBytes:  counting.n,
		CreatedAt:  c.now(),
		Reclaimed:  "pod " + ref.Name,
		AbortToken: checkpointID,
	}, nil
}

// AbortCheckpoint reopens the source workload's admission barrier for the
// generation recorded in cp.AbortToken.
func (c *AgentCheckpointer) AbortCheckpoint(ctx context.Context, ref k8s.PodRef, cp *session.Checkpoint) error {
	if cp == nil || cp.AbortToken == "" {
		return fmt.Errorf("abort checkpoint for pod %s: checkpoint generation ID is empty", ref.Name)
	}
	return c.client.AbortCheckpoint(ctx, ref.Name, cp.AbortToken)
}

// Restore fetches the archive from durable storage and streams it to the
// restore-target agent, which applies its workload-specific format (AC-B2/B3,
// AC-E5).
func (c *AgentCheckpointer) Restore(ctx context.Context, cp *session.Checkpoint, into k8s.PodRef) error {
	if cp == nil || cp.Ref == "" {
		return fmt.Errorf("restore into pod %s: checkpoint ref is empty", into.Name)
	}
	if c.store == nil {
		return fmt.Errorf("restore into pod %s requires a durable store (CHECKPOINT_S3_*)", into.Name)
	}
	rc, err := c.store.Get(ctx, cp.Ref)
	if err != nil {
		return fmt.Errorf("fetch checkpoint %s: %w", cp.Ref, err)
	}
	defer rc.Close()
	if err := c.client.Restore(ctx, into.Name, rc); err != nil {
		return fmt.Errorf("restore checkpoint %s into pod %s: %w", cp.Ref, into.Name, err)
	}
	return nil
}

// agentCheckpointKey is the durable object key for a pod's checkpoint archive.
// The pod name is unique per restore cycle (RestoreInto mints a fresh name) and
// the generation is unique per attempt, so keys never collide across a
// session's snapshot→restore history.
func agentCheckpointKey(pod, generation string) string {
	return pod + "/" + generation + "/checkpoint.tar"
}

func validArchiveGeneration(generation string) bool {
	if len(generation) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(generation)
	if err != nil {
		return false
	}
	return hex.EncodeToString(decoded) == generation
}

// countingReader tallies bytes as they stream through, so the checkpointer can
// report Checkpoint.SizeBytes without buffering the archive or a second pass.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
