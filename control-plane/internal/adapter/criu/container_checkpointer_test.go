package criu_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// fakeDriver stands in for the kubelet ContainerCheckpoint call so the adapter's
// orchestration is unit-tested without a CRIU-capable runtime (the runtime side
// is verified separately — see docs/criu-verification.md). It records its args.
type fakeDriver struct {
	items         []string
	checkpointErr error
	restoreErr    error

	checkpointCalls                      int
	gotNode, gotNS, gotPod, gotContainer string

	restoreCalls   int
	gotRestoreRef  string
	gotRestoreInto k8s.PodRef
}

func (f *fakeDriver) Checkpoint(_ context.Context, node, ns, pod, container string) ([]string, error) {
	f.checkpointCalls++
	f.gotNode, f.gotNS, f.gotPod, f.gotContainer = node, ns, pod, container
	return f.items, f.checkpointErr
}

func (f *fakeDriver) Restore(_ context.Context, ref string, into k8s.PodRef) error {
	f.restoreCalls++
	f.gotRestoreRef, f.gotRestoreInto = ref, into
	return f.restoreErr
}

var _ criu.CheckpointDriver = (*fakeDriver)(nil)

// fakeStore stands in for the durable checkpoint store (S3) so the upload wiring
// is unit-tested without AWS. It records the key and the streamed bytes.
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

var _ criu.CheckpointStore = (*fakeStore)(nil)

func podOn(node, ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

// Checkpoint resolves the pod's node, drives the checkpoint through the driver,
// and returns the archive path the kubelet reported as Checkpoint.Ref (AC-B1/B3).
func TestContainerCheckpointer_CheckpointReturnsArchiveRef(t *testing.T) {
	const ns = "sessions"
	archive := "/var/lib/kubelet/checkpoints/checkpoint-sess-abcd_sessions-session-2026.tar"
	drv := &fakeDriver{items: []string{archive}}
	cs := fake.NewSimpleClientset(podOn("node-1", ns, "sess-abcd"))
	fixed := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	ckpt := criu.NewContainerCheckpointer(cs, ns,
		criu.WithDriver(drv), criu.WithClock(func() time.Time { return fixed }))

	if !ckpt.Enabled() {
		t.Fatal("real checkpointer must report Enabled() == true")
	}

	cp, err := ckpt.Checkpoint(context.Background(), k8s.PodRef{Name: "sess-abcd"})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.Ref != archive {
		t.Errorf("Ref = %q, want the kubelet archive path %q", cp.Ref, archive)
	}
	if !cp.CreatedAt.Equal(fixed) {
		t.Errorf("CreatedAt = %v, want the injected clock %v", cp.CreatedAt, fixed)
	}

	// The driver must be dialed for the right node/pod/container (the checkpoint
	// endpoint lives on the pod's node; the container is the data plane shell).
	if drv.checkpointCalls != 1 {
		t.Fatalf("driver Checkpoint called %d times, want 1", drv.checkpointCalls)
	}
	if drv.gotNode != "node-1" {
		t.Errorf("driver node = %q, want node-1 (pod's node)", drv.gotNode)
	}
	if drv.gotNS != ns || drv.gotPod != "sess-abcd" {
		t.Errorf("driver target = %s/%s, want %s/sess-abcd", drv.gotNS, drv.gotPod, ns)
	}
	if drv.gotContainer != k8s.ContainerName {
		t.Errorf("driver container = %q, want %q", drv.gotContainer, k8s.ContainerName)
	}
	// No store configured: the ref is the ephemeral node-local archive path and
	// size is unknown (0).
	if cp.SizeBytes != 0 {
		t.Errorf("SizeBytes = %d, want 0 without a durable store", cp.SizeBytes)
	}
}

// With a CheckpointStore the node-local archive is uploaded and its durable ref
// (e.g. s3://…) and size are recorded instead of the ephemeral node path
// (decision ③ — checkpoints survive their node).
func TestContainerCheckpointer_CheckpointUploadsToStore(t *testing.T) {
	const ns = "sessions"
	archive := "/var/lib/kubelet/checkpoints/checkpoint-sess-abcd_sessions-session-9.tar"
	drv := &fakeDriver{items: []string{archive}}
	cs := fake.NewSimpleClientset(podOn("node-1", ns, "sess-abcd"))
	store := &fakeStore{ref: "s3://ckpt/checkpoints/sessions/sess-abcd/checkpoint-sess-abcd_sessions-session-9.tar"}
	content := []byte("CRIU-ARCHIVE-BYTES")
	opener := func(path string) (io.ReadCloser, int64, error) {
		if path != archive {
			t.Errorf("opener path = %q, want the kubelet archive %q", path, archive)
		}
		return io.NopCloser(bytes.NewReader(content)), int64(len(content)), nil
	}

	ckpt := criu.NewContainerCheckpointer(cs, ns,
		criu.WithDriver(drv), criu.WithStore(store), criu.WithArchiveOpener(opener))

	cp, err := ckpt.Checkpoint(context.Background(), k8s.PodRef{Name: "sess-abcd"})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.Ref != store.ref {
		t.Errorf("Ref = %q, want the durable store ref %q", cp.Ref, store.ref)
	}
	if cp.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want the uploaded archive size %d", cp.SizeBytes, len(content))
	}
	// Stored under namespace/pod/<kubelet-filename> with the bytes streamed verbatim.
	if want := "sessions/sess-abcd/checkpoint-sess-abcd_sessions-session-9.tar"; store.gotKey != want {
		t.Errorf("store key = %q, want %q", store.gotKey, want)
	}
	if string(store.gotBytes) != string(content) {
		t.Errorf("stored bytes = %q, want the archive bytes verbatim", store.gotBytes)
	}
}

// A store upload failure surfaces from Checkpoint rather than yielding a
// checkpoint whose ref points at an archive that was never durably stored.
func TestContainerCheckpointer_CheckpointSurfacesStoreError(t *testing.T) {
	const ns = "sessions"
	drv := &fakeDriver{items: []string{"/var/lib/kubelet/checkpoints/c.tar"}}
	cs := fake.NewSimpleClientset(podOn("node-1", ns, "sess-abcd"))
	store := &fakeStore{err: errors.New("access denied")}
	opener := func(string) (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader([]byte("x"))), 1, nil
	}
	ckpt := criu.NewContainerCheckpointer(cs, ns,
		criu.WithDriver(drv), criu.WithStore(store), criu.WithArchiveOpener(opener))

	if _, err := ckpt.Checkpoint(context.Background(), k8s.PodRef{Name: "sess-abcd"}); err == nil {
		t.Fatal("checkpoint succeeded despite store failure; want error")
	}
}

// A pod not yet scheduled to a node cannot be checkpointed — there is no kubelet
// to dial — so Checkpoint fails without calling the driver.
func TestContainerCheckpointer_CheckpointRequiresNode(t *testing.T) {
	const ns = "sessions"
	drv := &fakeDriver{items: []string{"unused"}}
	cs := fake.NewSimpleClientset(podOn("", ns, "sess-nonode"))
	ckpt := criu.NewContainerCheckpointer(cs, ns, criu.WithDriver(drv))

	if _, err := ckpt.Checkpoint(context.Background(), k8s.PodRef{Name: "sess-nonode"}); err == nil {
		t.Fatal("checkpoint of an unscheduled pod succeeded; want error")
	}
	if drv.checkpointCalls != 0 {
		t.Errorf("driver called %d times for an unscheduled pod, want 0", drv.checkpointCalls)
	}
}

// A driver failure (kubelet unreachable, gate off, runtime lacks CRIU) surfaces
// from Checkpoint rather than being swallowed into a bogus checkpoint.
func TestContainerCheckpointer_CheckpointSurfacesDriverError(t *testing.T) {
	const ns = "sessions"
	drv := &fakeDriver{checkpointErr: errors.New("checkpointing not supported")}
	cs := fake.NewSimpleClientset(podOn("node-1", ns, "sess-abcd"))
	ckpt := criu.NewContainerCheckpointer(cs, ns, criu.WithDriver(drv))

	if _, err := ckpt.Checkpoint(context.Background(), k8s.PodRef{Name: "sess-abcd"}); err == nil {
		t.Fatal("checkpoint succeeded despite driver failure; want error")
	}
}

// An empty archive list is an error, not a checkpoint with an empty ref (nothing
// to restore from later).
func TestContainerCheckpointer_CheckpointRejectsEmptyArchive(t *testing.T) {
	const ns = "sessions"
	drv := &fakeDriver{items: nil}
	cs := fake.NewSimpleClientset(podOn("node-1", ns, "sess-abcd"))
	ckpt := criu.NewContainerCheckpointer(cs, ns, criu.WithDriver(drv))

	if _, err := ckpt.Checkpoint(context.Background(), k8s.PodRef{Name: "sess-abcd"}); err == nil {
		t.Fatal("checkpoint with no archive path succeeded; want error")
	}
}

// Restore hands the recorded ref and target pod to the driver (AC-B2).
func TestContainerCheckpointer_RestoreDrivesRefIntoPod(t *testing.T) {
	const ns = "sessions"
	drv := &fakeDriver{}
	cs := fake.NewSimpleClientset()
	ckpt := criu.NewContainerCheckpointer(cs, ns, criu.WithDriver(drv))

	cp := &session.Checkpoint{Ref: "/var/lib/kubelet/checkpoints/checkpoint-x.tar"}
	into := k8s.PodRef{Name: "sess-restored", Namespace: ns}
	if err := ckpt.Restore(context.Background(), cp, into); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if drv.restoreCalls != 1 {
		t.Fatalf("driver Restore called %d times, want 1", drv.restoreCalls)
	}
	if drv.gotRestoreRef != cp.Ref {
		t.Errorf("driver restore ref = %q, want %q", drv.gotRestoreRef, cp.Ref)
	}
	if drv.gotRestoreInto != into {
		t.Errorf("driver restore into = %+v, want %+v", drv.gotRestoreInto, into)
	}
}

// Restore refuses a nil/empty checkpoint rather than dialing the driver with
// nothing to resume.
func TestContainerCheckpointer_RestoreRejectsEmptyCheckpoint(t *testing.T) {
	drv := &fakeDriver{}
	ckpt := criu.NewContainerCheckpointer(fake.NewSimpleClientset(), "sessions", criu.WithDriver(drv))

	if err := ckpt.Restore(context.Background(), nil, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with nil checkpoint succeeded; want error")
	}
	if err := ckpt.Restore(context.Background(), &session.Checkpoint{Ref: ""}, k8s.PodRef{Name: "p"}); err == nil {
		t.Error("restore with empty ref succeeded; want error")
	}
	if drv.restoreCalls != 0 {
		t.Errorf("driver Restore called %d times for empty checkpoints, want 0", drv.restoreCalls)
	}
}
