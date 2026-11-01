package archive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const testGeneration = "0123456789abcdef0123456789abcdef"

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

var _ archive.AgentCheckpointClient = (*fakeAgentClient)(nil)

type fakeStore struct {
	ref      string
	err      error
	gotKey   string
	gotBytes []byte
}

func (f *fakeStore) Put(_ context.Context, key string, r io.Reader) (string, error) {
	f.gotKey = key
	f.gotBytes, _ = io.ReadAll(r)
	return f.ref, f.err
}

func (f *fakeStore) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.gotBytes)), nil
}

var _ archive.CheckpointStore = (*fakeStore)(nil)

func TestAgentCheckpointer_CheckpointStreamsToStore(t *testing.T) {
	payload := []byte("FILESYSTEM-ARCHIVE-TAR-BYTES")
	client := &fakeAgentClient{archive: payload}
	store := &fakeStore{ref: "s3://ckpt/checkpoints/sess-abcd/checkpoint.tar"}
	c := archive.NewAgentCheckpointer(client, store)

	if !c.Enabled() {
		t.Fatal("agent checkpointer must report Enabled() == true")
	}
	cp, err := c.CheckpointWithGeneration(context.Background(), k8s.PodRef{Name: "sess-abcd"}, testGeneration)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.Ref != store.ref {
		t.Errorf("Ref = %q, want the durable store ref %q", cp.Ref, store.ref)
	}
	if cp.SizeBytes != int64(len(payload)) {
		t.Errorf("SizeBytes = %d, want the streamed archive length %d", cp.SizeBytes, len(payload))
	}
	if cp.CreatedAt.IsZero() {
		t.Error("CreatedAt not stamped")
	}
	if client.gotCheckpointPod != "sess-abcd" {
		t.Errorf("agent checkpoint pod = %q, want sess-abcd", client.gotCheckpointPod)
	}
	if string(store.gotBytes) != string(payload) {
		t.Errorf("stored bytes = %q, want the agent archive verbatim", store.gotBytes)
	}
}

func TestAgentCheckpointer_UsesControlPlaneGeneration(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive")}
	store := &fakeStore{ref: "s3://bucket/archive.tar"}
	c := archive.NewAgentCheckpointer(client, store)

	cp, err := c.CheckpointWithGeneration(context.Background(), k8s.PodRef{Name: "agent-pod"}, testGeneration)
	if err != nil {
		t.Fatalf("checkpoint with generation: %v", err)
	}
	if client.gotGeneration != testGeneration {
		t.Fatalf("agent generation = %q, want %q", client.gotGeneration, testGeneration)
	}
	if cp.AbortToken != testGeneration {
		t.Fatalf("checkpoint abort token = %q, want %q", cp.AbortToken, testGeneration)
	}
	if want := "agent-pod/" + testGeneration + "/checkpoint.tar"; store.gotKey != want {
		t.Fatalf("archive key = %q, want %q", store.gotKey, want)
	}
}

func TestAgentCheckpointer_RejectsInvalidGenerationBeforeAgent(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive")}
	c := archive.NewAgentCheckpointer(client, &fakeStore{})
	for _, generation := range []string{"", "short", "0123456789ABCDEF0123456789ABCDEF"} {
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

func TestAgentCheckpointer_RejectsMismatchedAgentGeneration(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive"), checkpointID: "ffffffffffffffffffffffffffffffff"}
	store := &fakeStore{}
	c := archive.NewAgentCheckpointer(client, store)
	if _, err := c.CheckpointWithGeneration(context.Background(), k8s.PodRef{Name: "agent-pod"}, testGeneration); err == nil {
		t.Fatal("checkpoint accepted an archive echoing a different generation")
	}
	if store.gotKey != "" {
		t.Fatalf("mismatched archive reached the store under %q", store.gotKey)
	}
}

func TestAgentCheckpointer_CheckpointRequiresStore(t *testing.T) {
	c := archive.NewAgentCheckpointer(&fakeAgentClient{archive: []byte("x")}, nil)
	if _, err := c.CheckpointWithGeneration(context.Background(), k8s.PodRef{Name: "p"}, testGeneration); err == nil {
		t.Fatal("checkpoint without a store succeeded; want error")
	}
}

func TestAgentCheckpointer_CheckpointSurfacesClientError(t *testing.T) {
	client := &fakeAgentClient{checkpointErr: errors.New("agent unreachable")}
	c := archive.NewAgentCheckpointer(client, &fakeStore{})
	if _, err := c.CheckpointWithGeneration(context.Background(), k8s.PodRef{Name: "p"}, testGeneration); err == nil {
		t.Fatal("checkpoint succeeded despite agent failure; want error")
	}
}

func TestAgentCheckpointer_StoreFailureDefersAbortToService(t *testing.T) {
	client := &fakeAgentClient{archive: []byte("archive")}
	store := &fakeStore{err: errors.New("s3 unavailable")}
	c := archive.NewAgentCheckpointer(client, store)

	if _, err := c.CheckpointWithGeneration(
		context.Background(), k8s.PodRef{Name: "agent-pod"}, testGeneration,
	); !errors.Is(err, store.err) {
		t.Fatalf("checkpoint error = %v, want store failure", err)
	}
	if client.abortCalls != 0 {
		t.Fatalf("generation checkpoint made %d autonomous aborts; service owner must decide", client.abortCalls)
	}
}

func TestAgentCheckpointer_AbortCallsAgentWithGeneration(t *testing.T) {
	client := &fakeAgentClient{}
	c := archive.NewAgentCheckpointer(client, &fakeStore{})
	if err := c.AbortCheckpoint(
		context.Background(), k8s.PodRef{Name: "shell-pod"}, &session.Checkpoint{AbortToken: testGeneration},
	); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if client.abortCalls != 1 || client.gotAbortPod != "shell-pod" || client.gotAbortID != testGeneration {
		t.Fatalf("abort = (%d, %q, %q), want (1, shell-pod, %s)", client.abortCalls, client.gotAbortPod, client.gotAbortID, testGeneration)
	}
	if err := c.AbortCheckpoint(context.Background(), k8s.PodRef{Name: "shell-pod"}, &session.Checkpoint{}); err == nil {
		t.Fatal("abort without a generation succeeded; want error")
	}
}

func TestAgentCheckpointer_RestoreStreamsFromStore(t *testing.T) {
	store := &fakeStore{gotBytes: []byte("RESTORE-ARCHIVE")}
	client := &fakeAgentClient{}
	c := archive.NewAgentCheckpointer(client, store)

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
	c := archive.NewAgentCheckpointer(&fakeAgentClient{}, &fakeStore{})
	if err := c.Restore(context.Background(), nil, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with nil checkpoint succeeded; want error")
	}
	if err := c.Restore(context.Background(), &session.Checkpoint{Ref: ""}, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with empty ref succeeded; want error")
	}
}
