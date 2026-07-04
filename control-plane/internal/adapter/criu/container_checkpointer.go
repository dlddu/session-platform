// This file is the real Checkpointer: a K8s-native CRIU adapter built on the
// kubelet ContainerCheckpoint API (KEP-2008, alpha). It sits behind the same
// Checkpointer port as the gated stub and is wired in only when CRIU_ENABLED is
// on (cmd/control-plane/main.go); the stub still covers the gate-off happy path.
//
// The code is written but UNVERIFIED: proving it end-to-end needs a
// CRIU-capable runtime (containerd/runc CRIU build + the ContainerCheckpoint
// feature gate), which is provisioned separately. To keep it compiling and
// unit-testable without that runtime, the one runtime-specific call — hitting
// the kubelet checkpoint endpoint — is isolated behind CheckpointDriver: tests
// inject a fake, main injects the real kubeletDriver. See docs/criu-verification.md.
package criu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// CheckpointDriver performs the runtime-specific half of CRIU checkpoint and
// restore. Isolating it behind an interface keeps the ContainerCheckpointer (and
// everything above it) compiling and unit-testable without a CRIU-capable
// cluster: a fake driver stands in under test, while the real kubeletDriver is
// exercised only where the ContainerCheckpoint feature gate is on.
type CheckpointDriver interface {
	// Checkpoint freezes namespace/pod/container running on node and returns the
	// archive path(s) the kubelet wrote — in the node-local storage model, one
	// .tar under /var/lib/kubelet/checkpoints.
	Checkpoint(ctx context.Context, node, namespace, pod, container string) ([]string, error)
	// Restore resumes a checkpoint archive as the target pod's process tree. On
	// the K8s-native path the resume is driven by the restore-target pod the
	// orchestrator already created (k8s.ClientOrchestrator.RestoreInto annotates
	// it with the ref), so there is nothing to drive here; the runc-restore
	// alternative (docs/criu-verification.md) would apply the archive at this
	// seam instead.
	Restore(ctx context.Context, ref string, into k8s.PodRef) error
}

// CheckpointStore is a durable object store for checkpoint archives. When set on
// the ContainerCheckpointer, Checkpoint streams the kubelet's node-local archive
// to it and records the returned durable ref (e.g. s3://bucket/key) in the
// Checkpoint instead of the ephemeral node-local path — so the archive survives
// the node and is reachable from wherever the session restores. Defined here
// (consumer side) so the criu package carries no cloud SDK dependency; the S3
// implementation lives in internal/adapter/checkpointstore.
type CheckpointStore interface {
	// Put uploads the archive read from r under key and returns its durable ref.
	Put(ctx context.Context, key string, r io.Reader) (ref string, err error)
}

// archiveOpener opens the kubelet's node-local checkpoint archive so it can be
// streamed to the CheckpointStore, returning its size for Checkpoint.SizeBytes.
// The default (openLocalArchive) opens a local file, which assumes the archive
// path is readable by this process — i.e. the control plane or an uploader
// sidecar mounts the node's checkpoint dir. That mount is a provisioning concern
// (docs/criu-verification.md); the seam lets tests inject an in-memory archive.
type archiveOpener func(path string) (io.ReadCloser, int64, error)

func openLocalArchive(path string) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// ContainerCheckpointer is the real Checkpointer. Checkpoint drives the kubelet
// ContainerCheckpoint API (via the driver) to freeze a session pod's container
// into a node-local archive; with a CheckpointStore set it then uploads the
// archive to durable storage and records that ref. Restore hands the ref to the
// driver to resume it into the restore-target pod (AC-B1/B2/B3, AC-D4).
type ContainerCheckpointer struct {
	client    kubernetes.Interface
	namespace string
	container string
	driver    CheckpointDriver
	store     CheckpointStore // optional; nil keeps the node-local archive ref
	open      archiveOpener
	now       func() time.Time
}

// compile-time assertion that ContainerCheckpointer satisfies the port.
var _ Checkpointer = (*ContainerCheckpointer)(nil)

// Option customises a ContainerCheckpointer. WithDriver/WithClock exist for
// tests; production uses the defaults (the real kubeletDriver, a UTC clock).
type Option func(*ContainerCheckpointer)

// WithContainer overrides the container name to checkpoint (default:
// k8s.ContainerName, the single data plane container).
func WithContainer(name string) Option {
	return func(c *ContainerCheckpointer) {
		if name != "" {
			c.container = name
		}
	}
}

// WithDriver injects a CheckpointDriver, letting tests exercise the adapter's
// orchestration without a CRIU-capable runtime.
func WithDriver(d CheckpointDriver) Option {
	return func(c *ContainerCheckpointer) {
		if d != nil {
			c.driver = d
		}
	}
}

// WithStore sets the durable checkpoint store. Without it Checkpoint records the
// ephemeral node-local archive path; with it the archive is uploaded and the
// durable ref (e.g. s3://…) is recorded instead.
func WithStore(s CheckpointStore) Option {
	return func(c *ContainerCheckpointer) {
		if s != nil {
			c.store = s
		}
	}
}

// WithArchiveOpener overrides how the node-local archive is opened for upload
// (tests inject an in-memory archive).
func WithArchiveOpener(o archiveOpener) Option {
	return func(c *ContainerCheckpointer) {
		if o != nil {
			c.open = o
		}
	}
}

// WithClock injects the clock stamped onto Checkpoint.CreatedAt (tests).
func WithClock(now func() time.Time) Option {
	return func(c *ContainerCheckpointer) {
		if now != nil {
			c.now = now
		}
	}
}

// NewContainerCheckpointer builds the real checkpointer over an injected client
// and namespace. Without WithDriver it drives the real kubelet endpoint.
func NewContainerCheckpointer(client kubernetes.Interface, namespace string, opts ...Option) *ContainerCheckpointer {
	c := &ContainerCheckpointer{
		client:    client,
		namespace: namespace,
		container: k8s.ContainerName,
		open:      openLocalArchive,
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.driver == nil {
		c.driver = newKubeletDriver(client)
	}
	return c
}

// Enabled reports that real CRIU checkpoint/restore is active.
func (c *ContainerCheckpointer) Enabled() bool { return true }

// Checkpoint freezes the pod's session container into a node-local checkpoint
// archive and returns its ref (AC-B1, AC-B3, AC-D4). The archive path — where
// the whole process tree, its memory (including the agent's scrollback) and open
// FDs are captured — is recorded in Checkpoint.Ref for the restore to resume.
func (c *ContainerCheckpointer) Checkpoint(ctx context.Context, ref k8s.PodRef) (*session.Checkpoint, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = c.namespace
	}
	// The checkpoint endpoint lives on the node running the pod, so resolve the
	// pod's node before dialing the kubelet through it.
	pod, err := c.client.CoreV1().Pods(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("resolve pod %s/%s for checkpoint: %w", ns, ref.Name, err)
	}
	node := pod.Spec.NodeName
	if node == "" {
		return nil, fmt.Errorf("pod %s/%s has no node assigned; cannot checkpoint", ns, ref.Name)
	}

	items, err := c.driver.Checkpoint(ctx, node, ns, ref.Name, c.container)
	if err != nil {
		return nil, fmt.Errorf("checkpoint pod %s/%s container %s: %w", ns, ref.Name, c.container, err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("checkpoint pod %s/%s: kubelet returned no archive path", ns, ref.Name)
	}
	localPath := items[0]

	checkpoint := &session.Checkpoint{
		Ref:       localPath,
		SizeBytes: 0,
		CreatedAt: c.now(),
		Reclaimed: "pod " + ref.Name,
	}
	if c.store == nil {
		// No durable store configured: record the ephemeral node-local archive
		// path. Its size is not stat-able from the control plane, so leave 0.
		return checkpoint, nil
	}

	// Upload the node-local archive to durable storage (S3) and record that ref
	// instead, so the archive survives the node and is reachable when the session
	// restores onto a different node (decision ③).
	rc, size, err := c.open(localPath)
	if err != nil {
		return nil, fmt.Errorf("open checkpoint archive %s: %w", localPath, err)
	}
	defer rc.Close()
	uri, err := c.store.Put(ctx, checkpointKey(ns, ref.Name, localPath), rc)
	if err != nil {
		return nil, fmt.Errorf("store checkpoint archive %s: %w", localPath, err)
	}
	checkpoint.Ref = uri
	checkpoint.SizeBytes = size
	return checkpoint, nil
}

// checkpointKey derives the durable object key for a pod's checkpoint archive,
// keeping the kubelet-assigned filename (which carries a timestamp, so it is
// unique) under a namespace/pod path.
func checkpointKey(namespace, pod, localPath string) string {
	return namespace + "/" + pod + "/" + filepath.Base(localPath)
}

// Restore resumes the checkpoint into the restore-target pod (AC-B2, AC-B3). The
// pod was already shaped as a restore target by RestoreInto (carrying cp.Ref in
// an annotation), so on the K8s-native path this delegates to the driver, which
// relies on the runtime resuming from that annotation; the service layer then
// proves the resumed shell is reachable (Reach, AC-D1). When cp.Ref is an s3://
// URI (a durable store was configured at checkpoint time), the node-side restore
// fetches the archive from the store using the same assume-role identity before
// resuming — see docs/criu-verification.md.
func (c *ContainerCheckpointer) Restore(ctx context.Context, cp *session.Checkpoint, into k8s.PodRef) error {
	if cp == nil || cp.Ref == "" {
		return fmt.Errorf("restore into pod %s: checkpoint ref is empty", into.Name)
	}
	if err := c.driver.Restore(ctx, cp.Ref, into); err != nil {
		return fmt.Errorf("restore checkpoint %s into pod %s: %w", cp.Ref, into.Name, err)
	}
	return nil
}

// kubeletDriver drives the kubelet ContainerCheckpoint endpoint through the API
// server's node/proxy subresource:
//
//	POST /api/v1/nodes/{node}/proxy/checkpoint/{namespace}/{pod}/{container}
//
// It needs the ContainerCheckpoint feature gate on the kubelet and RBAC for the
// nodes/proxy subresource. The kubelet replies with
// {"items":["/var/lib/kubelet/checkpoints/checkpoint-<pod>_<ns>-<container>-<ts>.tar"]}.
type kubeletDriver struct {
	client kubernetes.Interface
}

func newKubeletDriver(client kubernetes.Interface) *kubeletDriver {
	return &kubeletDriver{client: client}
}

func (d *kubeletDriver) Checkpoint(ctx context.Context, node, namespace, pod, container string) ([]string, error) {
	raw, err := d.client.CoreV1().RESTClient().
		Post().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("checkpoint", namespace, pod, container).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("kubelet checkpoint %s/%s/%s on node %s: %w", namespace, pod, container, node, err)
	}
	var resp struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode checkpoint response %q: %w", string(raw), err)
	}
	return resp.Items, nil
}

func (d *kubeletDriver) Restore(ctx context.Context, ref string, into k8s.PodRef) error {
	// K8s-native restore has no kubelet API: the resume is triggered when the
	// restore-target pod (created by k8s.ClientOrchestrator.RestoreInto, carrying
	// annotationRestoreCheckpoint=ref) is started by a CRIU-capable runtime,
	// which maps the annotation to its restore mechanism (e.g. CRI-O's
	// io.kubernetes.cri-o.restore, or a checkpoint OCI image built from the
	// archive). The service layer then proves the resumed shell is reachable via
	// Reach (AC-D1). There is thus nothing to drive here on the K8s-native path;
	// the runc-restore alternative (docs/criu-verification.md) would apply the
	// archive at this seam instead.
	_, _, _ = ctx, ref, into
	return nil
}
