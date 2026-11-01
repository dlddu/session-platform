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

type fakeAgentClient struct {
	archive       []byte
	checkpointID  string
	checkpointErr error
	restoreErr    error
	abortErr      error

	gotCheckpointPod string
	gotGeneration    string
	gotRestorePod    string
	gotRestoreBytes  []byte
	gotAbortPod      string
	gotAbortID       string
	abortCalls       int
}

func (f *fakeAgentClient) Checkpoint(_ context.Context, pod string) (io.ReadCloser, string, error) {
	f.gotCheckpointPod = pod
	if f.checkpointErr != nil {
		return nil, "", f.checkpointErr
	}
	return io.NopCloser(bytes.NewReader(f.archive)), f.checkpointID, nil
}

func (f *fakeAgentClient) CheckpointWithGeneration(_ context.Context, pod, generation string) (io.ReadCloser, string, error) {
	f.gotCheckpointPod = pod
	f.gotGeneration = generation
	if f.checkpointErr != nil {
		return nil, "", f.checkpointErr
	}
	responseID := f.checkpointID
	if responseID == "" {
		responseID = generation
	}
	return io.NopCloser(bytes.NewReader(f.archive)), responseID, nil
}

func (f *fakeAgentClient) Restore(_ context.Context, pod string, archive io.Reader) error {
	f.gotRestorePod = pod
	f.gotRestoreBytes, _ = io.ReadAll(archive)
	return f.restoreErr
}

func (f *fakeAgentClient) AbortCheckpoint(_ context.Context, pod, checkpointID string) error {
	f.abortCalls++
	f.gotAbortPod = pod
	f.gotAbortID = checkpointID
	return f.abortErr
}

var _ criu.AgentCheckpointClient = (*fakeAgentClient)(nil)

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

func TestAgentArchiveCheckpointer_UsesControlPlaneGeneration(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	client := &fakeAgentClient{archive: []byte("archive")}
	store := &fakeStore{ref: "s3://bucket/archive.tar"}
	c := criu.NewAgentArchiveCheckpointer(client, store)

	cp, err := c.CheckpointWithGeneration(context.Background(), k8s.PodRef{Name: "agent-pod"}, generation)
	if err != nil {
		t.Fatalf("checkpoint with generation: %v", err)
	}
	if client.gotGeneration != generation {
		t.Fatalf("agent generation = %q, want %q", client.gotGeneration, generation)
	}
	if cp.AbortToken != generation {
		t.Fatalf("checkpoint abort token = %q, want %q", cp.AbortToken, generation)
	}
	if want := "agent-pod/" + generation + "/checkpoint.tar"; store.gotKey != want {
		t.Fatalf("archive key = %q, want %q", store.gotKey, want)
	}
}

func TestAgentArchiveCheckpointer_RejectsInvalidGenerationBeforeAgent(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive")}
	c := criu.NewAgentArchiveCheckpointer(client, &fakeStore{})
	for _, generation := range []string{"short", "0123456789ABCDEF0123456789ABCDEF"} {
		if _, err := c.CheckpointWithGeneration(
			context.Background(), k8s.PodRef{Name: "agent-pod"}, generation,
		); err == nil {
			t.Fatalf("generation %q was accepted", generation)
		}
	}
	if client.gotCheckpointPod != "" {
		t.Fatalf("invalid generation reached agent pod %q", client.gotCheckpointPod)
	}
}

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

func TestAgentArchiveCheckpointer_StoreFailureAbortsAgent(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive"), checkpointID: "generation-1"}
	store := &fakeStore{err: errors.New("s3 unavailable")}
	c := criu.NewAgentArchiveCheckpointer(client, store)

	if _, err := c.Checkpoint(context.Background(), k8s.PodRef{Name: "agent-pod"}); err == nil {
		t.Fatal("checkpoint succeeded despite store failure")
	}
	if client.abortCalls != 1 || client.gotAbortPod != "agent-pod" || client.gotAbortID != "generation-1" {
		t.Fatalf("abort = (%d, %q, %q), want (1, agent-pod, generation-1)", client.abortCalls, client.gotAbortPod, client.gotAbortID)
	}
}

func TestAgentArchiveCheckpointer_GenerationStoreFailureDefersAbortToService(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	client := &fakeAgentClient{archive: []byte("archive")}
	store := &fakeStore{err: errors.New("s3 unavailable")}
	c := criu.NewAgentArchiveCheckpointer(client, store)

	if _, err := c.CheckpointWithGeneration(
		context.Background(), k8s.PodRef{Name: "agent-pod"}, generation,
	); !errors.Is(err, store.err) {
		t.Fatalf("checkpoint error = %v, want store failure", err)
	}
	if client.abortCalls != 0 {
		t.Fatalf("generation checkpoint made %d autonomous aborts; service owner must decide", client.abortCalls)
	}
}

func TestAgentArchiveCheckpointer_ReportsAbortFailure(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive"), checkpointID: "generation-1", abortErr: errors.New("agent unavailable")}
	store := &fakeStore{err: errors.New("s3 unavailable")}
	c := criu.NewAgentArchiveCheckpointer(client, store)

	_, err := c.Checkpoint(context.Background(), k8s.PodRef{Name: "agent-pod"})
	if err == nil || !errors.Is(err, store.err) || !errors.Is(err, client.abortErr) {
		t.Fatalf("checkpoint error = %v, want joined store and abort errors", err)
	}
}

func TestShellAgentCheckpointer_StoreFailureDoesNotUseArchiveAbort(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive")}
	c := criu.NewAgentCheckpointer(client, &fakeStore{err: errors.New("s3 unavailable")})
	_, _ = c.Checkpoint(context.Background(), k8s.PodRef{Name: "shell-pod"})
	if client.abortCalls != 0 {
		t.Fatalf("shell abort calls = %d, want 0", client.abortCalls)
	}
}

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

func TestAgentCheckpointer_RestoreRejectsEmpty(t *testing.T) {
	c := criu.NewAgentCheckpointer(&fakeAgentClient{}, &fakeStore{})
	if err := c.Restore(context.Background(), nil, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with nil checkpoint succeeded; want error")
	}
	if err := c.Restore(context.Background(), &session.Checkpoint{Ref: ""}, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with empty ref succeeded; want error")
	}
}
