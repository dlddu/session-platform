// This file is the real, client-go backed PodOrchestrator: one dedicated data
// plane pod per session. The workload itself is started by the data plane
// image's entrypoint, never by the control plane.
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

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const (
	// LabelSessionID ties a data plane pod 1:1 to its session.
	LabelSessionID = "session-id"
	// LabelWorkloadType records which workload type the pod runs.
	LabelWorkloadType = "session-platform.dev/workload-type"
	// labelManagedBy marks the pods this control plane owns so a stray selector
	// never reclaims something it did not create.
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "control-plane"

	// AnnotationRestoreCheckpoint marks a pod as a CRIU restore target and records
	// the checkpoint archive it must resume from. A CRIU-capable runtime maps this
	// to its concrete restore mechanism; see docs/criu-verification.md.
	AnnotationRestoreCheckpoint = "session-platform.dev/restore-checkpoint"

	// AnnotationRestoreArchive marks a claude-code restore target; the agent
	// unpacks a filesystem archive and never invokes CRIU.
	AnnotationRestoreArchive = "session-platform.dev/restore-archive"

	defaultClaudeCredentialsSecret = "claude-code-credentials"
	// ContainerName is the workload container in each data plane pod.
	ContainerName = "session"
	// DataPlaneServiceAccountName is the identity mounted into every session pod.
	DataPlaneServiceAccountName = "data-plane"

	// DefaultDataPlaneImage is the in-code fallback when no DATA_PLANE_IMAGE is
	// injected. It cannot pass the shell readiness probe (no session agent inside),
	// so real deployments MUST inject the published data plane agent image.
	defaultDataPlaneImage = "alpine:3.20"

	// ShellEnvVar propagates the session shell override into the pod.
	shellEnvVar = "DATA_PLANE_SHELL"

	// WorkloadEnvVar tells the pod which workload type it is running.
	workloadEnvVar = "DATA_PLANE_WORKLOAD"

	// Provider credential values are projected only into a separate localhost
	// credential-proxy container; the agent/CLI container gets a non-secret
	// placeholder and cannot read or transform the real provider token.
	ClaudeCodeModelEnvVar     = "CLAUDE_CODE_MODEL"
	K3SMCPTokenEnvVar         = "K3S_MCP_TOKEN"
	claudeCodeStateDirEnvVar  = "CLAUDE_CODE_STATE_DIR"
	claudeCodeStateVolumePath = "/session"
	// The state root must be a child of the volume mount, not the mount point
	// itself: archive restore atomically renames this directory into place, and
	// Linux rejects renaming an active mount point with EBUSY.
	claudeCodeStateDir             = claudeCodeStateVolumePath + "/state"
	claudeCodeStateVolumeName      = "claude-state"
	AnthropicBaseURLEnvVar         = "ANTHROPIC_BASE_URL"
	AnthropicAuthTokenEnvVar       = "ANTHROPIC_AUTH_TOKEN"
	ClaudeCodeModelSecretKey       = "model"
	ClaudeCodeBaseURLSecretKey     = "base-url"
	ClaudeCodeAuthTokenSecretKey   = "auth-token"
	ClaudeCodeK3SMCPTokenSecretKey = "k3s-mcp-token"
	// ClaudeCredentialsContainerName identifies the isolated Secret holder.
	ClaudeCredentialsContainerName = "claude-credentials"
	// ClaudeCredentialProxyURL is the loopback-only endpoint visible to the CLI.
	ClaudeCredentialProxyURL = "http://127.0.0.1:8091"

	credentialProxyWorkload         = "credential-proxy"
	credentialProxyListenAddr       = "127.0.0.1:8091"
	credentialProxyPlaceholderToken = "session-platform-proxy"
	agentAddrEnvVar                 = "DATA_PLANE_AGENT_ADDR"

	// restoreModeEnvVar tells a restore-target agent to wait for POST /restore
	// instead of starting a fresh workload.
	// Keep in sync with data-plane/cmd/agent (restoreModeEnv).
	restoreModeEnvVar = "DATA_PLANE_RESTORE_MODE"

	// AgentPort is where the session agent serves /attach and /healthz. Keep
	// in sync with data-plane/cmd/agent (defaultAddr).
	AgentPort     = 8090
	agentPortName = "agent"
	// AgentHealthzPath backs the pod readiness probe, so pod Ready implies a live
	// workload agent.
	agentHealthzPath = "/healthz"
	// AgentAttachPath is the readiness stream Reach opens and closes; user I/O
	// uses the workload-neutral /read and /write endpoints instead.
	agentAttachPath = "/attach"

	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	defaultPollInterval = 2 * time.Second
	defaultReadyTimeout = 2 * time.Minute
)

// ClientOrchestrator is the real PodOrchestrator: it provisions and reclaims a
// dedicated data plane pod per session through client-go.
type ClientOrchestrator struct {
	client                  kubernetes.Interface
	namespace               string
	image                   string
	claudeCredentialsSecret string
	workloadImages          map[session.WorkloadType]string
	shell                   string // DATA_PLANE_SHELL override ("" = agent default)
	checkpointPrivileged    bool   // run session pods privileged for in-pod CRIU (CRIU_ENABLED)
	agentPort               int
	pollInterval            time.Duration
	readyTimeout            time.Duration
}

var _ PodOrchestrator = (*ClientOrchestrator)(nil)

// Option customises a ClientOrchestrator.
type Option func(*ClientOrchestrator)

// WithImage overrides the data plane pod image.
func WithImage(image string) Option {
	return func(o *ClientOrchestrator) {
		if image != "" {
			o.image = image
		}
	}
}

// WithWorkloadImage overrides the data plane image for one workload type. An
// empty image leaves the type unconfigured, and Start refuses such a type rather
// than provisioning it from the shell image.
func WithWorkloadImage(workload session.WorkloadType, image string) Option {
	return func(o *ClientOrchestrator) {
		if image == "" {
			return
		}
		if o.workloadImages == nil {
			o.workloadImages = map[session.WorkloadType]string{}
		}
		o.workloadImages[workload] = image
	}
}

// WithClaudeCredentialsSecret selects the platform-managed Secret whose
// base-url and auth-token keys are projected only into the credential sidecar.
func WithClaudeCredentialsSecret(name string) Option {
	return func(o *ClientOrchestrator) {
		if strings.TrimSpace(name) != "" {
			o.claudeCredentialsSecret = strings.TrimSpace(name)
		}
	}
}

// WithShell overrides the interactive shell the session agent launches.
func WithShell(shell string) Option {
	return func(o *ClientOrchestrator) {
		if shell != "" {
			o.shell = shell
		}
	}
}

// WithCheckpointPrivileged runs session pods privileged so the in-pod CRIU path
// works. On-cluster verification showed capabilities alone are not enough:
// CHECKPOINT_RESTORE + SYS_PTRACE hit netns EPERM, and even with SYS_ADMIN +
// NET_ADMIN added, containerd's default AppArmor blocks mounts and
// /proc/sys/kernel/ns_last_pid stays read-only — while a privileged pod passes
// `criu check` completely.
func WithCheckpointPrivileged(enabled bool) Option {
	return func(o *ClientOrchestrator) {
		o.checkpointPrivileged = enabled
	}
}

// WithAgentPort overrides the session agent port (default 8090).
func WithAgentPort(port int) Option {
	return func(o *ClientOrchestrator) {
		if port > 0 {
			o.agentPort = port
		}
	}
}

// WithReadiness tunes how Start waits for a pod to report Ready.
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
// namespace.
func NewClientOrchestrator(client kubernetes.Interface, namespace string, opts ...Option) *ClientOrchestrator {
	o := &ClientOrchestrator{
		client:                  client,
		namespace:               namespace,
		image:                   defaultDataPlaneImage,
		agentPort:               AgentPort,
		claudeCredentialsSecret: defaultClaudeCredentialsSecret,
		pollInterval:            defaultPollInterval,
		readyTimeout:            defaultReadyTimeout,
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// BuildClient builds a Kubernetes client and resolves the namespace the control
// plane operates in.
//
// Namespace resolution prefers the pod's own service account namespace, because
// the kubeconfig loader does NOT read it, and falls back to the kubeconfig
// context for local runs.
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

// NamespaceFromServiceAccount returns "" when not running in-cluster.
func namespaceFromServiceAccount() string {
	b, err := os.ReadFile(serviceAccountNamespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Namespace reports the namespace this orchestrator provisions pods in.
func (o *ClientOrchestrator) Namespace() string { return o.namespace }

// Start provisions a dedicated pod for sessionID, waits for its workload agent
// to report Ready, and returns its ref with the pod IP recorded.
func (o *ClientOrchestrator) Start(ctx context.Context, sessionID string, workload WorkloadSpec) (PodRef, error) {
	spec, err := o.podSpec(sessionID, workload)
	if err != nil {
		return PodRef{}, err
	}
	return o.provision(ctx, spec)
}

// Provision creates a pod from spec and waits for its agent to report Ready.
func (o *ClientOrchestrator) provision(ctx context.Context, spec *corev1.Pod) (PodRef, error) {
	created, err := o.client.CoreV1().Pods(o.namespace).Create(ctx, spec, metav1.CreateOptions{})
	if err != nil {
		return PodRef{}, fmt.Errorf("create pod %s: %w", spec.Name, err)
	}
	ref := PodRef{Name: created.Name, Namespace: o.namespace}
	pod, err := o.waitReady(ctx, ref.Name)
	if err != nil {
		// Don't leak a pod that never came up.
		o.cleanup(ref)
		return PodRef{}, err
	}
	ref.IP = pod.Status.PodIP
	return ref, nil
}

// Stop deletes the pod and reclaims its resources. A missing pod is treated as
// already reclaimed so the call is idempotent. PodRef.Namespace may be empty and
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

// RestoreInto provisions the pod a session archive is restored into. Applying
// the archive bytes is the Checkpointer's job; supplying the correctly shaped
// pod is all the orchestrator owns.
func (o *ClientOrchestrator) RestoreInto(ctx context.Context, sessionID, checkpointRef string, workload WorkloadSpec) (PodRef, error) {
	spec, err := o.restorePodSpec(sessionID, checkpointRef, workload)
	if err != nil {
		return PodRef{}, err
	}
	return o.provision(ctx, spec)
}

// PodSpec is a fresh-session pod whose entrypoint starts the workload agent.
func (o *ClientOrchestrator) podSpec(sessionID string, workload WorkloadSpec) (*corev1.Pod, error) {
	return o.buildPod(sessionID, "", workload)
}

// restorePodSpec is the workload-specific restore target handed to provision.
func (o *ClientOrchestrator) restorePodSpec(sessionID, checkpointRef string, workload WorkloadSpec) (*corev1.Pod, error) {
	return o.buildPod(sessionID, checkpointRef, workload)
}

// ImageFor resolves the data plane image for a workload type. A non-default type
// must have been configured with WithWorkloadImage, otherwise provisioning fails
// loudly instead of handing the session a pod that does not run its workload.
func (o *ClientOrchestrator) imageFor(workload session.WorkloadType) (string, error) {
	if img, ok := o.workloadImages[workload]; ok {
		return img, nil
	}
	if workload == session.WorkloadTypeShell {
		return o.image, nil
	}
	return "", fmt.Errorf("no data plane image configured for workload type %q", workload)
}

// buildPod assembles the data plane pod. checkpointRef == "" yields a fresh
// session pod under the session's deterministic name; a non-empty ref yields a
// restore-target pod under a fresh unique name.
func (o *ClientOrchestrator) buildPod(sessionID, checkpointRef string, workload WorkloadSpec) (*corev1.Pod, error) {
	workloadType, err := session.NormalizeWorkloadType(workload.Type)
	if err != nil {
		return nil, err
	}
	model, err := session.NormalizeModel(workloadType, workload.Model)
	if err != nil {
		return nil, err
	}
	image, err := o.imageFor(workloadType)
	if err != nil {
		return nil, err
	}
	// No command override: the data plane image's entrypoint owns starting the
	// selected agent. The control plane only orchestrates.
	container := corev1.Container{
		Name:            ContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicyForImage(image),
		Ports: []corev1.ContainerPort{{
			Name:          agentPortName,
			ContainerPort: AgentPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		// The agent answers /healthz only while it can serve the selected
		// workload, so pod Ready is a workload-neutral readiness signal.
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
	container.Env = append(container.Env, corev1.EnvVar{Name: workloadEnvVar, Value: string(workloadType)})
	var sidecars []corev1.Container
	var volumes []corev1.Volume
	switch workloadType {
	case session.WorkloadTypeShell:
		if o.shell != "" {
			container.Env = append(container.Env, corev1.EnvVar{Name: shellEnvVar, Value: o.shell})
		}
	case session.WorkloadTypeClaudeCode:
		modelEnv := corev1.EnvVar{Name: ClaudeCodeModelEnvVar, Value: model}
		if model == session.PlatformDefaultModel {
			// Keep platform-default as the immutable alias while resolving its
			// effective value at pod start. The key is optional so existing Secrets
			// retain the previous Claude CLI default behaviour.
			modelEnv = optionalSecretEnv(
				ClaudeCodeModelEnvVar,
				o.claudeCredentialsSecret,
				ClaudeCodeModelSecretKey,
			)
		}
		container.Env = append(container.Env,
			modelEnv,
			secretEnv(K3SMCPTokenEnvVar, o.claudeCredentialsSecret, ClaudeCodeK3SMCPTokenSecretKey),
			corev1.EnvVar{Name: claudeCodeStateDirEnvVar, Value: claudeCodeStateDir},
			corev1.EnvVar{Name: AnthropicBaseURLEnvVar, Value: ClaudeCredentialProxyURL},
			corev1.EnvVar{Name: AnthropicAuthTokenEnvVar, Value: credentialProxyPlaceholderToken},
		)
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: claudeCodeStateVolumeName, MountPath: claudeCodeStateVolumePath,
		})
		volumes = append(volumes, corev1.Volume{
			Name:         claudeCodeStateVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})

		sidecars = append(sidecars, o.claudeCredentialProxy(image))
	}
	if o.checkpointPrivileged && workloadType == session.WorkloadTypeShell {
		// In-pod CRIU needs a privileged container: capability sets alone are
		// defeated by the runtime's AppArmor profile and read-only /proc (see
		// WithCheckpointPrivileged). This makes CRIU-enabled shells node-root.
		privileged := true
		container.SecurityContext = &corev1.SecurityContext{Privileged: &privileged}
	}
	name := podName(sessionID)
	var annotations map[string]string
	if checkpointRef != "" {
		name, err = restorePodName(sessionID)
		if err != nil {
			return nil, err
		}
		annotation := AnnotationRestoreCheckpoint
		if workloadType == session.WorkloadTypeClaudeCode {
			annotation = AnnotationRestoreArchive
		}
		annotations = map[string]string{annotation: checkpointRef}
		// The agent waits for the archive on POST /restore before accepting I/O.
		container.Env = append(container.Env, corev1.EnvVar{Name: restoreModeEnvVar, Value: "1"})
	}
	containers := append([]corev1.Container{container}, sidecars...)
	// Session workloads intentionally receive a Kubernetes identity. The binding
	// uses the built-in `view` role rather than a wildcard rule because RBAC
	// cannot subtract Secrets from an otherwise-wildcard grant.
	automountServiceAccountToken := true

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   o.namespace,
			Annotations: annotations,
			Labels: map[string]string{
				LabelSessionID:    sessionID,
				LabelWorkloadType: string(workloadType),
				labelManagedBy:    managedByValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			ServiceAccountName:           DataPlaneServiceAccountName,
			AutomountServiceAccountToken: &automountServiceAccountToken,
			Containers:                   containers,
			Volumes:                      volumes,
		},
	}, nil
}

// claudeCredentialProxy holds provider credentials outside the tool-running
// container. Kubernetes gives pod containers a shared network namespace but
// separate PID/filesystem namespaces, so Claude can call this loopback service
// without being able to read its Secret-backed environment or /proc entries.
func (o *ClientOrchestrator) claudeCredentialProxy(image string) corev1.Container {
	runAsNonRoot := true
	runAsUser := int64(65532)
	disallowPrivilegeEscalation := false
	readOnlyRootFilesystem := true
	return corev1.Container{
		Name:            ClaudeCredentialsContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicyForImage(image),
		Env: []corev1.EnvVar{
			{Name: workloadEnvVar, Value: credentialProxyWorkload},
			{Name: agentAddrEnvVar, Value: credentialProxyListenAddr},
			secretEnv(AnthropicBaseURLEnvVar, o.claudeCredentialsSecret, ClaudeCodeBaseURLSecretKey),
			secretEnv(AnthropicAuthTokenEnvVar, o.claudeCredentialsSecret, ClaudeCodeAuthTokenSecretKey),
		},
		// An exec probe reaches loopback from inside this container. An HTTP
		// probe would target the pod IP and force the credential proxy to listen
		// on the cluster network instead of localhost.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"/bin/bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8091",
			}}},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             &runAsNonRoot,
			RunAsUser:                &runAsUser,
			RunAsGroup:               &runAsUser,
			AllowPrivilegeEscalation: &disallowPrivilegeEscalation,
			ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}

func secretEnv(envName, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			Key:                  key,
		}},
	}
}

func optionalSecretEnv(envName, secretName, key string) corev1.EnvVar {
	env := secretEnv(envName, secretName, key)
	optional := true
	env.ValueFrom.SecretKeyRef.Optional = &optional
	return env
}

// PodName derives a deterministic, DNS-safe pod name from the session id.
func podName(sessionID string) string {
	return "sess-" + sessionID
}

// restorePodName names a restore-target pod: the session's deterministic name
// plus a fresh per-restore suffix. The frozen pod's deletion is asynchronous, so
// reusing the deterministic name would race an AlreadyExists on create when a
// restore-on-access follows the snapshot immediately.
func restorePodName(sessionID string) (string, error) {
	// Maximum entropy that still keeps the 32-hex session ID in a 63-char DNS label.
	// "sess-" + id + "-r" + 24 hex chars = 63.
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate restore pod suffix: %w", err)
	}
	return podName(sessionID) + "-r" + hex.EncodeToString(b), nil
}

// pullPolicyForImage mirrors kubelet's own default: a mutable :latest (or
// untagged) reference is pulled Always, so a freshly published data plane agent
// is picked up instead of a stale node cache of the same tag. An immutable
// reference (a :<sha> tag, a digest, or the kind-loaded :dev image) stays
// IfNotPresent.
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

// WaitReady polls until the pod reports Ready, returning its final state.
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

// Reach proves the control plane can reach the session workload: it opens the
// agent's attach WebSocket at the pod IP and closes it immediately. The control
// plane dials over the pod network; it never execs into the pod.
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
