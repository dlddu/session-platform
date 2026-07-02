// This file is the real, client-go backed PodOrchestrator. It drives one
// dedicated data plane pod per session in the control plane's own namespace
// (AC-A1/A2), reclaims it on stop (AC-A3), and proves the pod's PTY shell
// agent is reachable by opening/closing its attach stream (AC-D1) — the shell
// itself is started by the data plane image's entrypoint, never by the control
// plane. The port and the in-memory stub live in orchestrator.go; main builds
// the client and namespace via BuildClient.
package k8s

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

	// containerName is the single container in each data plane pod.
	containerName = "session"

	// defaultDataPlaneImage is the in-code fallback when no DATA_PLANE_IMAGE
	// is injected. It cannot pass the shell readiness probe (no session agent
	// inside), so real deployments MUST inject the published data plane agent
	// image (data-plane/Dockerfile) — k8s/deployment.yaml and the e2e overlay
	// both do.
	defaultDataPlaneImage = "alpine:3.20"

	// shellEnvVar propagates the session shell override into the pod; the
	// agent's entrypoint launches ${DATA_PLANE_SHELL:-/bin/bash} (AC-D1).
	shellEnvVar = "DATA_PLANE_SHELL"

	// AgentPort is where the session agent serves /attach and /healthz. Keep
	// in sync with data-plane/cmd/agent (defaultAddr).
	AgentPort     = 8090
	agentPortName = "agent"
	// agentHealthzPath backs the pod readiness probe, so pod Ready implies a
	// live shell process (AC-D1).
	agentHealthzPath = "/healthz"
	// agentAttachPath is the shell attach stream Reach opens and closes.
	agentAttachPath = "/attach"

	// serviceAccountNamespaceFile is where the kubelet mounts the pod's own
	// namespace when running in-cluster.
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	defaultPollInterval = 2 * time.Second
	defaultReadyTimeout = 2 * time.Minute
)

// ClientOrchestrator is the real PodOrchestrator: it provisions and reclaims a
// dedicated data plane pod per session through client-go, and dials the pod's
// session agent to prove the shell is reachable (AC-D1).
type ClientOrchestrator struct {
	client       kubernetes.Interface
	namespace    string
	image        string
	shell        string // DATA_PLANE_SHELL override injected into pods ("" = agent default)
	agentPort    int
	pollInterval time.Duration
	readyTimeout time.Duration
}

// compile-time assertion that ClientOrchestrator satisfies the port.
var _ PodOrchestrator = (*ClientOrchestrator)(nil)

// Option customises a ClientOrchestrator.
type Option func(*ClientOrchestrator)

// WithImage overrides the data plane pod image (default: alpine fallback,
// which cannot pass the shell readiness probe — see defaultDataPlaneImage).
func WithImage(image string) Option {
	return func(o *ClientOrchestrator) {
		if image != "" {
			o.image = image
		}
	}
}

// WithShell overrides the interactive shell the session agent launches
// (AC-D1); empty keeps the agent's default (/bin/bash).
func WithShell(shell string) Option {
	return func(o *ClientOrchestrator) {
		if shell != "" {
			o.shell = shell
		}
	}
}

// WithAgentPort overrides the session agent port (default 8090). Tests point
// Reach at a local mock agent; production keeps the default.
func WithAgentPort(port int) Option {
	return func(o *ClientOrchestrator) {
		if port > 0 {
			o.agentPort = port
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
		agentPort:    AgentPort,
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

// Start provisions a dedicated pod for sessionID, waits for it to report Ready
// — which, via the agent readiness probe, means its PTY shell is alive (AC-D1)
// — and returns its ref with the pod IP recorded for the agent dial (AC-A1/A2).
func (o *ClientOrchestrator) Start(ctx context.Context, sessionID string) (PodRef, error) {
	created, err := o.client.CoreV1().Pods(o.namespace).Create(ctx, o.podSpec(sessionID), metav1.CreateOptions{})
	if err != nil {
		return PodRef{}, fmt.Errorf("create pod for session %s: %w", sessionID, err)
	}
	ref := PodRef{Name: created.Name, Namespace: o.namespace}
	pod, err := o.waitReady(ctx, ref.Name)
	if err != nil {
		// Don't leak a pod that never came up (AC-A3 hygiene).
		o.cleanup(ref)
		return PodRef{}, err
	}
	ref.IP = pod.Status.PodIP
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
	// No command override: the data plane image's entrypoint owns starting the
	// PTY-attached session shell (AC-D1) — the control plane only orchestrates.
	container := corev1.Container{
		Name:            containerName,
		Image:           o.image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{{
			Name:          agentPortName,
			ContainerPort: AgentPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		// The agent answers /healthz only while the shell process is alive, so
		// "pod Ready" — what Start waits for — reflects shell liveness.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: agentHealthzPath,
					Port: intstr.FromInt32(AgentPort),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
		},
	}
	if o.shell != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: shellEnvVar, Value: o.shell})
	}
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
			Containers:    []corev1.Container{container},
		},
	}
}

// podName derives a deterministic, DNS-safe pod name from the session id so the
// session<->pod mapping is 1:1 and recoverable from the id alone (AC-A2).
func podName(sessionID string) string {
	return "sess-" + sessionID
}

// waitReady polls until the pod reports Ready — returning its final state, so
// callers can record the pod IP — or the readiness timeout elapses.
func (o *ClientOrchestrator) waitReady(ctx context.Context, name string) (*corev1.Pod, error) {
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
			return pod, nil
		default:
			last = "phase=" + string(pod.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("pod %s/%s not Ready: %w (last: %s)", o.namespace, name, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

// Reach proves the control plane can reach the session shell (AC-D1): it opens
// the agent's attach WebSocket stream at the pod IP and closes it immediately.
// No payload moves — the stdin/stdout semantics on this stream are J5-S2/S3.
// The control plane dials over the pod network; it never execs into the pod.
func (o *ClientOrchestrator) Reach(ctx context.Context, ref PodRef) error {
	if ref.IP == "" {
		return fmt.Errorf("reach session pod %s: ref has no pod IP (Reach applies to freshly started pods)", ref.Name)
	}
	url := "ws://" + net.JoinHostPort(ref.IP, strconv.Itoa(o.agentPort)) + agentAttachPath
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return fmt.Errorf("open attach stream %s for pod %s: %w", url, ref.Name, err)
	}
	// Opening the stream is the proof; close it politely and hang up.
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return conn.Close()
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
