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
	"crypto/rand"
	"encoding/hex"
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

	// AnnotationRestoreCheckpoint marks a pod the orchestrator provisions as a
	// CRIU *restore target* and records the checkpoint archive it must resume
	// from (RestoreInto). A CRIU-capable runtime maps this to its concrete
	// restore mechanism (e.g. CRI-O's io.kubernetes.cri-o.restore annotation or
	// a checkpoint OCI image built from the archive) so the container comes up
	// as the restored process tree instead of running the image entrypoint's
	// fresh shell. It is exported because that runtime mapping is part of the
	// restore contract the provisioning work wires up. See docs/criu-verification.md.
	AnnotationRestoreCheckpoint = "session-platform.dev/restore-checkpoint"

	// ContainerName is the single container in each data plane pod. The CRIU
	// checkpointer targets it by name when freezing the pod (internal/adapter/criu).
	ContainerName = "session"

	// defaultDataPlaneImage is the in-code fallback when no DATA_PLANE_IMAGE
	// is injected. It cannot pass the shell readiness probe (no session agent
	// inside), so real deployments MUST inject the published data plane agent
	// image (data-plane/Dockerfile) — k8s/deployment.yaml and the e2e overlay
	// both do.
	defaultDataPlaneImage = "alpine:3.20"

	// shellEnvVar propagates the session shell override into the pod; the
	// agent's entrypoint launches ${DATA_PLANE_SHELL:-/bin/bash} (AC-D1).
	shellEnvVar = "DATA_PLANE_SHELL"

	// restoreModeEnvVar tells a restore-target pod's agent to start WITHOUT a
	// shell and await the checkpoint on POST /restore (in-pod CRIU restore).
	// Keep in sync with data-plane/cmd/agent (restoreModeEnv).
	restoreModeEnvVar = "DATA_PLANE_RESTORE_MODE"

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
	client               kubernetes.Interface
	namespace            string
	image                string
	shell                string // DATA_PLANE_SHELL override injected into pods ("" = agent default)
	checkpointPrivileged bool   // run session pods privileged for in-pod CRIU (CRIU_ENABLED)
	agentPort            int
	pollInterval         time.Duration
	readyTimeout         time.Duration
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

// WithCheckpointPrivileged runs session pods privileged so the in-pod CRIU path
// (agent-driven checkpoint/restore) works. The 2026-07-23 on-cluster
// verification showed capabilities alone are not enough: CHECKPOINT_RESTORE +
// SYS_PTRACE hit netns EPERM, and even with SYS_ADMIN + NET_ADMIN added,
// containerd's default AppArmor blocks mounts and /proc/sys/kernel/ns_last_pid
// stays read-only — while a privileged pod passes `criu check` completely.
// Wired from CRIU_ENABLED so gate-off pods stay unprivileged. Narrowing this
// (caps + AppArmor unconfined + unmasked /proc) is a documented follow-up.
func WithCheckpointPrivileged(enabled bool) Option {
	return func(o *ClientOrchestrator) {
		o.checkpointPrivileged = enabled
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
	return o.provision(ctx, o.podSpec(sessionID))
}

// provision creates a pod from spec, waits for it to report Ready — which, via
// the agent readiness probe, means its PTY shell is alive (AC-D1) — and returns
// its ref with the pod IP recorded. Start and RestoreInto differ only in the
// spec they hand in (a fresh-shell pod vs. a restore-target pod).
func (o *ClientOrchestrator) provision(ctx context.Context, spec *corev1.Pod) (PodRef, error) {
	created, err := o.client.CoreV1().Pods(o.namespace).Create(ctx, spec, metav1.CreateOptions{})
	if err != nil {
		return PodRef{}, fmt.Errorf("create pod %s: %w", spec.Name, err)
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

// RestoreInto provisions the pod a session's checkpoint is restored into
// (AC-B2). Unlike Start — which brings up a *fresh* pod whose image entrypoint
// launches a brand-new PTY shell — the restore pod carries checkpointRef in an
// annotation so a CRIU-capable runtime resumes the checkpointed process tree
// instead; "Ready" then means the *restored* shell is alive, not an empty new
// one. Applying the checkpoint bytes is the Checkpointer's job; supplying the
// correctly-shaped pod is all the orchestrator owns.
func (o *ClientOrchestrator) RestoreInto(ctx context.Context, sessionID, checkpointRef string) (PodRef, error) {
	return o.provision(ctx, o.restorePodSpec(sessionID, checkpointRef))
}

// podSpec is the fresh-session pod: its image entrypoint starts a new PTY shell.
func (o *ClientOrchestrator) podSpec(sessionID string) *corev1.Pod {
	return o.buildPod(sessionID, "")
}

// restorePodSpec is the pod RestoreInto hands to provision: the same shell-agent
// container as a fresh session, plus AnnotationRestoreCheckpoint carrying the
// checkpoint ref so a CRIU-capable runtime resumes the checkpointed process tree
// rather than running the entrypoint's fresh shell. The container is otherwise
// identical, so once the runtime restores it the agent's /healthz reflects the
// *restored* shell and pod-Ready keeps its AC-D1 meaning.
func (o *ClientOrchestrator) restorePodSpec(sessionID, checkpointRef string) *corev1.Pod {
	return o.buildPod(sessionID, checkpointRef)
}

// buildPod assembles the data plane pod. checkpointRef == "" yields a fresh
// session pod (no annotation) under the session's deterministic name; a
// non-empty ref yields a restore-target pod under a fresh unique name.
func (o *ClientOrchestrator) buildPod(sessionID, checkpointRef string) *corev1.Pod {
	// No command override: on a fresh start the data plane image's entrypoint
	// owns launching the PTY-attached session shell (AC-D1); on a restore the
	// runtime resumes the checkpointed process tree and the entrypoint never
	// runs. Either way the control plane only orchestrates.
	container := corev1.Container{
		Name:            ContainerName,
		Image:           o.image,
		ImagePullPolicy: pullPolicyForImage(o.image),
		Ports: []corev1.ContainerPort{{
			Name:          agentPortName,
			ContainerPort: AgentPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		// The agent answers /healthz only while the shell process is alive, so
		// "pod Ready" — what provision waits for — reflects shell liveness.
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
	if o.checkpointPrivileged {
		// In-pod CRIU needs a privileged container: the verified-on-cluster
		// configuration where `criu check` fully passes (capability sets alone
		// are defeated by the runtime's AppArmor profile and read-only
		// /proc/sys — see WithCheckpointPrivileged). This makes CRIU-enabled
		// session shells node-root; narrowing is a documented follow-up.
		privileged := true
		container.SecurityContext = &corev1.SecurityContext{Privileged: &privileged}
	}
	name := podName(sessionID)
	var annotations map[string]string
	if checkpointRef != "" {
		name = restorePodName(sessionID)
		annotations = map[string]string{AnnotationRestoreCheckpoint: checkpointRef}
		// Restore-target pod: the agent starts without a shell and awaits the
		// checkpoint on POST /restore (in-pod CRIU restore) so the pod can become
		// Ready before the control plane pushes the archive.
		container.Env = append(container.Env, corev1.EnvVar{Name: restoreModeEnvVar, Value: "1"})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   o.namespace,
			Annotations: annotations,
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

// restorePodName names a restore-target pod: the session's deterministic name
// plus a fresh per-restore suffix. The frozen pod carried the deterministic
// name, and its deletion (snapshot's Stop) is asynchronous — when a
// restore-on-access follows the snapshot immediately, that pod may still be
// Terminating, so reusing the name would race an AlreadyExists on create.
// A unique name removes the race and matches the service contract that restore
// provisions a *new* pod rather than reusing the old name (the session-id
// label, not the name, carries the 1:1 session mapping — AC-A2).
func restorePodName(sessionID string) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return podName(sessionID) + "-r" + hex.EncodeToString(b)
}

// pullPolicyForImage mirrors kubelet's own default: a mutable :latest (or
// untagged) reference is pulled Always, so a freshly published data plane agent
// is picked up on the next session instead of the node serving a stale cache of
// the same tag — the exact failure where a new control plane calls /read on an
// old cached agent that never had the route. An immutable reference (a :<sha>
// tag, a digest, or the kind-loaded :dev image) stays IfNotPresent so it is
// used from the node without a registry round-trip.
func pullPolicyForImage(image string) corev1.PullPolicy {
	ref := image
	// Drop any registry host[:port]/repo prefix so a registry port colon is not
	// mistaken for a tag separator.
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	// A digest pin (name@sha256:...) is immutable.
	if strings.Contains(ref, "@") {
		return corev1.PullIfNotPresent
	}
	tag := ""
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		tag = ref[i+1:]
	}
	if tag == "" || tag == "latest" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
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
