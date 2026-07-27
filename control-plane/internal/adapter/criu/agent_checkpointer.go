// This file is the wired CRIU Checkpointer: agent-driven in-pod checkpoint and
// restore. The control plane asks the session pod's own agent to CRIU-dump its
// shell tree (POST /checkpoint), streams the resulting archive to durable
// storage (S3), and on restore streams it back to a restore-target pod's agent
// (POST /restore) which CRIU-restores the tree.
//
// This replaces the kubelet ContainerCheckpoint approach (container_checkpointer.go,
// kept only as the CRI-O alternative) because the k3s/containerd verification on
// 2026-07-22 confirmed containerd cannot restore a container checkpoint — there
// is no runtime component to resume the AnnotationRestoreCheckpoint pod. Doing
// CRIU in-agent keeps the whole path inside this repo (decision ⑤). The one part
// that still needs a CRIU-capable node is the agent's criu invocation itself
// (data-plane execCriuEngine); everything here is unit-tested with a fake client.
package criu

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AgentCheckpointClient is the control plane's channel to a session pod's agent
// checkpoint endpoints. The agent HTTP client implements it, resolving the pod
// IP per call like the shell I/O client does.
type AgentCheckpointClient interface {
	// Checkpoint returns the archive stream the pod's agent produces by dumping
	// its shell tree. The caller closes it.
	Checkpoint(ctx context.Context, pod string) (io.ReadCloser, error)
	// Restore streams a checkpoint archive to a restore-target pod's agent.
	Restore(ctx context.Context, pod string, archive io.Reader) error
}

// AgentCheckpointer is the agent-driven Checkpointer. It requires a
// CheckpointStore: the archive is produced inside the pod that is about to be
// reclaimed, so it must be streamed to durable storage to survive.
type AgentCheckpointer struct {
	client AgentCheckpointClient
	store  CheckpointStore
	now    func() time.Time
}

// compile-time assertion that AgentCheckpointer satisfies the port.
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

// Enabled reports that real CRIU checkpoint/restore is active.
func (c *AgentCheckpointer) Enabled() bool { return true }

// Checkpoint asks the pod's agent to dump its shell tree and streams the archive
// to durable storage, recording the durable ref (AC-B1, AC-B3, AC-D4).
func (c *AgentCheckpointer) Checkpoint(ctx context.Context, ref k8s.PodRef) (*session.Checkpoint, error) {
	if c.store == nil {
		return nil, fmt.Errorf("agent-driven checkpoint of pod %s requires a durable store (CHECKPOINT_S3_*)", ref.Name)
	}
	rc, err := c.client.Checkpoint(ctx, ref.Name)
	if err != nil {
		return nil, fmt.Errorf("checkpoint pod %s: %w", ref.Name, err)
	}
	defer rc.Close()

	counting := &countingReader{r: rc}
	uri, err := c.store.Put(ctx, agentCheckpointKey(ref.Name), counting)
	if err != nil {
		return nil, fmt.Errorf("store checkpoint archive for pod %s: %w", ref.Name, err)
	}
	return &session.Checkpoint{
		Ref:       uri,
		SizeBytes: counting.n,
		CreatedAt: c.now(),
		Reclaimed: "pod " + ref.Name,
	}, nil
}

// Restore fetches the archive from durable storage and streams it to the
// restore-target pod's agent, which CRIU-restores the shell tree (AC-B2, AC-B3).
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
