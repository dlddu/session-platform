package criu_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// fakeAgentClient stands in for the pod agent's checkpoint endpoints so the
// AgentCheckpointer's orchestration is tested without a running agent.
type fakeAgentClient struct {
	archive       []byte
	checkpointErr error
	restoreErr    error

	gotCheckpointPod string
	gotRestorePod    string
	gotRestoreBytes  []byte
}

func (f *fakeAgentClient) Checkpoint(_ context.Context, pod string) (io.ReadCloser, error) {
	f.gotCheckpointPod = pod
	if f.checkpointErr != nil {
		return nil, f.checkpointErr
	}
	return io.NopCloser(bytes.NewReader(f.archive)), nil
}

func (f *fakeAgentClient) Restore(_ context.Context, pod string, archive io.Reader) error {
	f.gotRestorePod = pod
	f.gotRestoreBytes, _ = io.ReadAll(archive)
	return f.restoreErr
}

var _ criu.AgentCheckpointClient = (*fakeAgentClient)(nil)

// Checkpoint pulls the archive from the pod agent and streams it to the store,
// recording the durable ref and the streamed size (AC-B1/B3/D4).
func TestAgentCheckpointer_CheckpointStreamsToStore(t *testing.T) {
	archive := []byte("CRIU-ARCHIVE-TAR-BYTES")
	client := &fakeAgentClient{archive: archive}
	store := &fakeStore{ref: "s3://ckpt/checkpoints/sess-abcd/checkpoint.tar"}
	c := criu.NewAgentCheckpointer(client, store)

	if !c.Enabled() {
		t.Fatal("agent checkpointer must report Enabled() == true")
	}
	cp, err := c.Checkpoint(context.Background(), k8s.PodRef{Name: "sess-abcd"})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.Ref != store.ref {
		t.Errorf("Ref = %q, want the durable store ref %q", cp.Ref, store.ref)
	}
	if cp.SizeBytes != int64(len(archive)) {
		t.Errorf("SizeBytes = %d, want the streamed archive length %d", cp.SizeBytes, len(archive))
	}
	if cp.CreatedAt.IsZero() {
		t.Error("CreatedAt not stamped")
	}
	if client.gotCheckpointPod != "sess-abcd" {
		t.Errorf("agent checkpoint pod = %q, want sess-abcd", client.gotCheckpointPod)
	}
	if store.gotKey != "sess-abcd/checkpoint.tar" {
		t.Errorf("store key = %q, want sess-abcd/checkpoint.tar", store.gotKey)
	}
	if string(store.gotBytes) != string(archive) {
		t.Errorf("stored bytes = %q, want the agent archive verbatim", store.gotBytes)
	}
}

// Agent-driven checkpoint needs a durable store — the archive lives in a pod
// about to be reclaimed — so a nil store is a clear error, not a lost archive.
func TestAgentCheckpointer_CheckpointRequiresStore(t *testing.T) {
	c := criu.NewAgentCheckpointer(&fakeAgentClient{archive: []byte("x")}, nil)
	if _, err := c.Checkpoint(context.Background(), k8s.PodRef{Name: "p"}); err == nil {
		t.Fatal("checkpoint without a store succeeded; want error")
	}
}

// An agent failure (pod unreachable, criu missing) surfaces from Checkpoint.
func TestAgentCheckpointer_CheckpointSurfacesClientError(t *testing.T) {
	client := &fakeAgentClient{checkpointErr: errors.New("agent unreachable")}
	c := criu.NewAgentCheckpointer(client, &fakeStore{})
	if _, err := c.Checkpoint(context.Background(), k8s.PodRef{Name: "p"}); err == nil {
		t.Fatal("checkpoint succeeded despite agent failure; want error")
	}
}

// Restore fetches the archive from the store and streams it to the
// restore-target pod's agent (AC-B2).
func TestAgentCheckpointer_RestoreStreamsFromStore(t *testing.T) {
	store := &fakeStore{gotBytes: []byte("RESTORE-ARCHIVE")}
	client := &fakeAgentClient{}
	c := criu.NewAgentCheckpointer(client, store)

	cp := &session.Checkpoint{Ref: "s3://ckpt/checkpoints/sess-abcd/checkpoint.tar"}
	if err := c.Restore(context.Background(), cp, k8s.PodRef{Name: "sess-abcd-r1"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if client.gotRestorePod != "sess-abcd-r1" {
		t.Errorf("agent restore pod = %q, want the restore-target pod", client.gotRestorePod)
	}
	if string(client.gotRestoreBytes) != "RESTORE-ARCHIVE" {
		t.Errorf("agent received %q, want the fetched archive", client.gotRestoreBytes)
	}
}

// Restore refuses a nil/empty checkpoint rather than streaming nothing.
func TestAgentCheckpointer_RestoreRejectsEmpty(t *testing.T) {
	c := criu.NewAgentCheckpointer(&fakeAgentClient{}, &fakeStore{})
	if err := c.Restore(context.Background(), nil, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with nil checkpoint succeeded; want error")
	}
	if err := c.Restore(context.Background(), &session.Checkpoint{Ref: ""}, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with empty ref succeeded; want error")
	}
}
