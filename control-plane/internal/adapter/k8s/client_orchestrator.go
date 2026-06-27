// This file is the real, client-go backed PodOrchestrator. It drives one
// dedicated data plane pod per session in the control plane's own namespace
// (AC-A1/A2) and reclaims it on stop (AC-A3). The port and the in-memory stub
// live in orchestrator.go; main builds the client and namespace via BuildClient.
package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// LabelSessionID ties a data plane pod 1:1 to its session (AC-A2). The
	// orchestrator's selectors and the deferred e2e suite both key off it.
	LabelSessionID = "session-id"
	// labelManagedBy marks the pods this control plane owns so a stray selector
	// never reclaims something it did not create.
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "control-plane"

	// containerName is the single container in each placeholder data plane pod.
	containerName = "session"

	// defaultDataPlaneImage is the placeholder data plane image (see
	// data-plane/Dockerfile). It only has to stay running so the pod reports
	// Ready; the real session workload is out of scope for this milestone.
	defaultDataPlaneImage = "alpine:3.20"

	// serviceAccountNamespaceFile is where the kubelet mounts the pod's own
	// namespace when running in-cluster.
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	defaultPollInterval = 2 * time.Second
	defaultReadyTimeout = 2 * time.Minute
)

// ClientOrchestrator is the real PodOrchestrator: it provisions and reclaims a
// dedicated data plane pod per session through client-go.
type ClientOrchestrator struct {
	client       kubernetes.Interface
	namespace    string
	image        string
	pollInterval time.Duration
	readyTimeout time.Duration
}

// compile-time assertion that ClientOrchestrator satisfies the port.
var _ PodOrchestrator = (*ClientOrchestrator)(nil)

// Option customises a ClientOrchestrator.
type Option func(*ClientOrchestrator)

// WithImage overrides the data plane pod image (default: alpine placeholder).
func WithImage(image string) Option {
	return func(o *ClientOrchestrator) {
		if image != "" {
			o.image = image
		}
	}
}

// WithReadiness tunes how Start waits for a pod to report Ready. Tests inject a
// short interval/timeout; production keeps the defaults.
func WithReadiness(pollInterval, timeout time.Duration) Option {
	return func(o *ClientOrchestrator) {
		if pollInterval > 0 {
			o.pollInterval = pollInterval
		}
		if timeout > 0 {
			o.readyTimeout = timeout
		}
	}
}

// NewClientOrchestrator builds a real orchestrator from an injected client and
// namespace. Injecting kubernetes.Interface lets tests drive it with a fake
// clientset; main builds the client and namespace via BuildClient.
func NewClientOrchestrator(client kubernetes.Interface, namespace string, opts ...Option) *ClientOrchestrator {
	o := &ClientOrchestrator{
		client:       client,
		namespace:    namespace,
		image:        defaultDataPlaneImage,
		pollInterval: defaultPollInterval,
		readyTimeout: defaultReadyTimeout,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// BuildClient builds a Kubernetes client and resolves the namespace the control
// plane operates in. It uses the in-cluster config when running as a pod, and
// otherwise the ambient kubeconfig (KUBECONFIG / ~/.kube/config) — so local
// development can drive a kind cluster.
//
// Namespace resolution prefers the pod's own service account namespace (the
// real namespace in-cluster — the deferred kubeconfig loader does NOT read it),
// and falls back to the kubeconfig context for local runs.
func BuildClient() (kubernetes.Interface, string, error) {
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	ns := namespaceFromServiceAccount()
	if ns == "" {
		if ns, _, err = cc.Namespace(); err != nil {
			return nil, "", err
		}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("build kubernetes client: %w", err)
	}
	return client, ns, nil
}

// namespaceFromServiceAccount reads the pod's own namespace from the mounted
// service account file, returning "" when absent (i.e. not running in-cluster).
func namespaceFromServiceAccount() string {
	b, err := os.ReadFile(serviceAccountNamespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Namespace reports the namespace this orchestrator provisions pods in.
func (o *ClientOrchestrator) Namespace() string { return o.namespace }

// Start provisions a dedicated pod for sessionID, waits for it to report Ready,
// and returns its ref (AC-A1/A2).
func (o *ClientOrchestrator) Start(ctx context.Context, sessionID string) (PodRef, error) {
	created, err := o.client.CoreV1().Pods(o.namespace).Create(ctx, o.podSpec(sessionID), metav1.CreateOptions{})
	if err != nil {
		return PodRef{}, fmt.Errorf("create pod for session %s: %w", sessionID, err)
	}
	ref := PodRef{Name: created.Name, Namespace: o.namespace}
	if err := o.waitReady(ctx, ref.Name); err != nil {
		// Don't leak a pod that never came up (AC-A3 hygiene).
		o.cleanup(ref)
		return PodRef{}, err
	}
	return ref, nil
}

// Stop deletes the pod and reclaims its resources (AC-A3). A missing pod is
// treated as already reclaimed so the call is idempotent. PodRef.Namespace may
// be empty (the service layer builds refs from the stored pod name only); it
// falls back to the orchestrator's namespace.
func (o *ClientOrchestrator) Stop(ctx context.Context, ref PodRef) error {
	ns := ref.Namespace
	if ns == "" {
		ns = o.namespace
	}
	err := o.client.CoreV1().Pods(ns).Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s/%s: %w", ns, ref.Name, err)
	}
	return nil
}

// RestoreInto provisions a fresh pod for a checkpoint to be restored into
// (AC-B2). Supplying the new pod is all the orchestrator owns; the CRIU restore
// itself is the Checkpointer's job, so this mirrors Start.
func (o *ClientOrchestrator) RestoreInto(ctx context.Context, sessionID string) (PodRef, error) {
	return o.Start(ctx, sessionID)
}

func (o *ClientOrchestrator) podSpec(sessionID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(sessionID),
			Namespace: o.namespace,
			Labels: map[string]string{
				LabelSessionID: sessionID,
				labelManagedBy: managedByValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:            containerName,
				Image:           o.image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				// Keep the placeholder running so the pod reports Ready.
				Command: []string{"sh", "-c", "sleep infinity"},
			}},
		},
	}
}

// podName derives a deterministic, DNS-safe pod name from the session id so the
// session<->pod mapping is 1:1 and recoverable from the id alone (AC-A2).
func podName(sessionID string) string {
	return "sess-" + sessionID
}

// waitReady polls until the pod reports Ready or the readiness timeout elapses.
func (o *ClientOrchestrator) waitReady(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, o.readyTimeout)
	defer cancel()
	ticker := time.NewTicker(o.pollInterval)
	defer ticker.Stop()

	var last string
	for {
		pod, err := o.client.CoreV1().Pods(o.namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err != nil:
			last = err.Error()
		case podReady(pod):
			return nil
		default:
			last = "phase=" + string(pod.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pod %s/%s not Ready: %w (last: %s)", o.namespace, name, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// cleanup best-effort deletes a pod that failed to come up, on a fresh context
// so a cancelled parent context doesn't also abort the cleanup.
func (o *ClientOrchestrator) cleanup(ref PodRef) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = o.Stop(ctx, ref)
}

// podReady reports whether the pod is Running with a true Ready condition.
func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
