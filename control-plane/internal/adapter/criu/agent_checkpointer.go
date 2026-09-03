// This file contains the wired agent-driven workload archive strategies. Shell
// sessions ask their agent to CRIU-dump the process tree; Claude sessions use a
// generation-fenced filesystem archive. Both streams go to durable storage and
// are later streamed to the selected workload's restore endpoint.
//
// It replaces the kubelet ContainerCheckpoint approach (container_checkpointer.go,
// kept only as the CRI-O alternative) because containerd cannot restore a
// container checkpoint. The one part that still needs a CRIU-capable node is the
// shell agent's criu invocation; Claude archive mode does not invoke CRIU.
package criu

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AgentCheckpointClient is the control plane's channel to a session pod's agent
// checkpoint endpoints.
type AgentCheckpointClient interface {
	// Checkpoint returns the archive stream the pod's agent produces by dumping
	// its shell tree. The caller closes it.
	Checkpoint(ctx context.Context, pod string) (archive io.ReadCloser, checkpointID string, err error)
	// CheckpointWithGeneration uses a control-plane-owned, durably recorded
	// generation for a crash-recoverable filesystem archive transaction.
	CheckpointWithGeneration(ctx context.Context, pod, generation string) (archive io.ReadCloser, checkpointID string, err error)
	// Restore streams a workload archive to a restore-target pod's agent.
	Restore(ctx context.Context, pod string, archive io.Reader) error
	// AbortCheckpoint reopens a filesystem workload whose archive transfer
	// completed but the durable control-plane transaction failed.
	AbortCheckpoint(ctx context.Context, pod, checkpointID string) error
}

// AgentCheckpointer is the agent-driven Checkpointer. It requires a
// CheckpointStore: the archive is produced inside the pod that is about to be
// reclaimed, so it must be streamed to durable storage to survive.
type AgentCheckpointer struct {
	client    AgentCheckpointClient
	store     CheckpointStore
	now       func() time.Time
	abortable bool
}

var _ Checkpointer = (*AgentCheckpointer)(nil)

// NewAgentCheckpointer builds the agent-driven checkpointer over the agent
// checkpoint client and a durable store.
func NewAgentCheckpointer(client AgentCheckpointClient, store CheckpointStore) *AgentCheckpointer {
	return &AgentCheckpointer{
		client: client,
		store:  store,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// NewAgentArchiveCheckpointer builds the filesystem-archive variant used by
// claude-code. Unlike a completed CRIU dump, this workload can reopen prompt
// admission if durable storage or pod reclamation fails.
func NewAgentArchiveCheckpointer(client AgentCheckpointClient, store CheckpointStore) *AgentCheckpointer {
	c := NewAgentCheckpointer(client, store)
	c.abortable = true
	return c
}

// Enabled reports that a real durable checkpoint/archive strategy is active.
func (c *AgentCheckpointer) Enabled() bool { return true }

// Checkpoint asks the pod's agent to dump its shell tree and streams the archive
// to durable storage, recording the durable ref.
func (c *AgentCheckpointer) Checkpoint(ctx context.Context, ref k8s.PodRef) (*session.Checkpoint, error) {
	return c.checkpoint(ctx, ref, "")
}

// CheckpointWithGeneration is the archive-only variant used by the service's
// durable snapshot transaction. The generation must already be stored before
// this method closes the data-plane admission barrier.
func (c *AgentCheckpointer) CheckpointWithGeneration(ctx context.Context, ref k8s.PodRef, generation string) (*session.Checkpoint, error) {
	if !c.abortable || !validArchiveGeneration(generation) {
		return nil, fmt.Errorf("checkpoint pod %s: durable archive generation is unavailable", ref.Name)
	}
	return c.checkpoint(ctx, ref, generation)
}

func (c *AgentCheckpointer) checkpoint(ctx context.Context, ref k8s.PodRef, generation string) (*session.Checkpoint, error) {
	if c.store == nil {
		return nil, fmt.Errorf("agent-driven checkpoint of pod %s requires a durable store (CHECKPOINT_S3_*)", ref.Name)
	}
	var rc io.ReadCloser
	var checkpointID string
	var err error
	if generation == "" {
		rc, checkpointID, err = c.client.Checkpoint(ctx, ref.Name)
	} else {
		rc, checkpointID, err = c.client.CheckpointWithGeneration(ctx, ref.Name, generation)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint pod %s: %w", ref.Name, err)
	}
	if generation != "" && checkpointID != generation {
		_ = rc.Close()
		return nil, fmt.Errorf("checkpoint pod %s: agent returned generation %q, want %q", ref.Name, checkpointID, generation)
	}
	if c.abortable && checkpointID == "" {
		_ = rc.Close()
		return nil, fmt.Errorf("checkpoint pod %s: agent omitted checkpoint generation ID", ref.Name)
	}

	counting := &countingReader{r: rc}
	key := agentCheckpointKey(ref.Name)
	if generation != "" {
		key = agentCheckpointGenerationKey(ref.Name, generation)
	}
	uri, err := c.store.Put(ctx, key, counting)
	closeErr := rc.Close()
	if err != nil {
		storeErr := fmt.Errorf("store checkpoint archive for pod %s: %w", ref.Name, err)
		if generation != "" {
			return nil, storeErr
		}
		return nil, c.abortAfterFailure(ref, checkpointID, storeErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("close checkpoint stream for pod %s: %w", ref.Name, closeErr)
		if generation != "" {
			return nil, closeErr
		}
		return nil, c.abortAfterFailure(ref, checkpointID, closeErr)
	}
	return &session.Checkpoint{
		Ref:        uri,
		SizeBytes:  counting.n,
		CreatedAt:  c.now(),
		Reclaimed:  "pod " + ref.Name,
		AbortToken: checkpointID,
	}, nil
}

// AbortCheckpoint rolls back only the claude-code archive admission barrier.
// Shell AgentCheckpointer instances deliberately no-op: CRIU already stopped
// the process tree and cannot be resumed by the filesystem protocol.
func (c *AgentCheckpointer) AbortCheckpoint(ctx context.Context, ref k8s.PodRef, cp *session.Checkpoint) error {
	if !c.abortable {
		return nil
	}
	if cp == nil || cp.AbortToken == "" {
		return fmt.Errorf("abort checkpoint for pod %s: checkpoint generation ID is empty", ref.Name)
	}
	return c.client.AbortCheckpoint(ctx, ref.Name, cp.AbortToken)
}

func (c *AgentCheckpointer) abortAfterFailure(ref k8s.PodRef, checkpointID string, cause error) error {
	if !c.abortable {
		return cause
	}
	abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.client.AbortCheckpoint(abortCtx, ref.Name, checkpointID); err != nil {
		return errors.Join(
			cause,
			fmt.Errorf("abort checkpoint for pod %s: %w", ref.Name, err),
		)
	}
	return cause
}

// Restore fetches the archive from durable storage and streams it to the
// restore-target agent, which applies its workload-specific format.
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
// The pod name is unique per restore cycle (RestoreInto mints a fresh name), so
// keying by it never collides across a session's snapshot→restore history.
func agentCheckpointKey(pod string) string {
	return pod + "/checkpoint.tar"
}

func agentCheckpointGenerationKey(pod, generation string) string {
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
