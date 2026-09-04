// This file is the real, client-go backed PodOrchestrator. It drives one
// dedicated data plane pod per session in the control plane's own namespace
// (AC-A1/A2), reclaims it on stop (AC-A3), and proves the selected workload
// agent is reachable (AC-D1/E1). The workload itself is started by the data
// plane image's entrypoint, never by the control plane. The port and the
// in-memory stub live in orchestrator.go; main builds the client and namespace
// via BuildClient.
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
	// LabelSessionID ties a data plane pod 1:1 to its session (AC-A2). The
	// orchestrator's selectors and the deferred e2e suite both key off it.
	LabelSessionID = "session-id"
	// LabelWorkloadType records which workload type the pod runs (AC-E1), so
	// the type a session was created with is observable on the cluster object
	// and not only in control plane state.
	LabelWorkloadType = "session-platform.dev/workload-type"
	// LabelPodRole separates a session's *workload* pod — AC-A2's 1:1 subject —
	// from the session-scoped auxiliary pods that only serve it (AC-A2's
	// auxiliary-pod clause, AC-F4's helper pod). Every pod carries the session
	// id, so a selector that keyed off LabelSessionID alone would count a helper
	// pod as a second workload pod for the session; this label is what lets such
	// a selector stay narrow.
	LabelPodRole = "session-platform.dev/pod-role"
	// PodRoleWorkload marks the pod running the session's workload.
	PodRoleWorkload = "workload"
	// PodRoleHelper marks a session-scoped auxiliary pod (AC-F4).
	PodRoleHelper = "helper"
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

	// AnnotationRestoreArchive marks a claude-code restore target. Unlike the
	// shell annotation above, the agent unpacks a filesystem archive and never
	// invokes CRIU (AC-E5).
	AnnotationRestoreArchive = "session-platform.dev/restore-archive"

	defaultClaudeCredentialsSecret = "claude-code-credentials"
	// ContainerName is the workload container in each data plane pod. Shell pods
	// contain only it; Claude pods add an isolated credential-proxy sidecar. The
	// CRIU checkpointer targets this container by name and only for shell sessions.
	ContainerName = "session"
	// DataPlaneServiceAccountName is the dedicated identity mounted into every
	// fresh and restored session pod. Kubernetes manifests bind it to the
	// built-in read-only `view` ClusterRole, which deliberately excludes Secrets.
	DataPlaneServiceAccountName = "data-plane"

	// defaultDataPlaneImage is the in-code fallback when no DATA_PLANE_IMAGE
	// is injected. It cannot pass the shell readiness probe (no session agent
	// inside), so real deployments MUST inject the published data plane agent
	// image (data-plane/Dockerfile) — k8s/deployment.yaml and the e2e overlay
	// both do.
	defaultDataPlaneImage = "alpine:3.20"

	// shellEnvVar propagates the session shell override into the pod; the
	// agent's entrypoint launches ${DATA_PLANE_SHELL:-/bin/bash} (AC-D1).
	shellEnvVar = "DATA_PLANE_SHELL"

	// workloadEnvVar tells the pod which workload type it is running (AC-E1).
	// The data plane agent branches on it and it is set unconditionally so the
	// pod is self-describing rather than inferring its role from its image.
	workloadEnvVar = "DATA_PLANE_WORKLOAD"

	// Claude Code runtime configuration. Provider credential values are
	// projected only into a separate localhost credential-proxy container. The
	// agent/CLI container gets a non-secret placeholder and cannot read or
	// transform the real provider token through its coding tools (AC-E6). The
	// session-platform plugin's K3s MCP token is intentionally projected into the
	// tool-running container, while the non-secret platform model may be selected
	// from the same Secret through its own key.
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

	// The plugin bootstrap reaches two endpoints before the agent starts: the
	// K3s MCP that issues the marketplace read token, and the marketplace git
	// remote itself. Both are *addresses*, not credentials, and both are
	// optional keys — when the Secret omits them the entrypoint keeps its
	// built-in production defaults, so k8s/ needs no change. Projecting them
	// lets an environment that cannot reach the organisation endpoints (the kind
	// e2e SUT) point the same code path at in-cluster stand-ins.
	K3SMCPURLEnvVar                         = "K3S_MCP_URL"
	ClaudeCodePluginMarketplaceURLEnvVar    = "CLAUDE_CODE_PLUGIN_MARKETPLACE_URL"
	ClaudeCodeK3SMCPURLSecretKey            = "k3s-mcp-url"
	ClaudeCodePluginMarketplaceURLSecretKey = "plugin-marketplace-url"
	// ClaudeCredentialsContainerName identifies the isolated Secret holder.
	ClaudeCredentialsContainerName = "claude-credentials"
	// ClaudeCredentialProxyURL is the loopback-only endpoint visible to the
	// Claude CLI in the pod's shared network namespace.
	ClaudeCredentialProxyURL = "http://127.0.0.1:8091"

	credentialProxyWorkload         = "credential-proxy"
	credentialProxyListenAddr       = "127.0.0.1:8091"
	credentialProxyPort             = 8091
	credentialProxyPlaceholderToken = "session-platform-proxy"
	agentAddrEnvVar                 = "DATA_PLANE_AGENT_ADDR"

	// Approval-gated runtime configuration (AC-F4/AC-F6). The session's helper
	// pod holds both external credentials, split per container: the approval
	// gateway's URL/key/user id reach only the session MCP container, and the
	// provider base-url/auth-token reach only the credential-proxy container.
	// The workload pod itself receives neither — only this pod's addresses and
	// the same non-secret placeholder claude-code uses.
	defaultApprovalGatewaySecret = "approval-gateway-credentials"
	// SessionMCPContainerName is the helper pod container that will expose the
	// session's external tools behind the approval gate (AC-F3). This slice
	// provisions the container; the gate itself is not implemented yet.
	SessionMCPContainerName = "session-mcp"
	// HelperCredentialProxyContainerName is the helper pod's provider proxy. It
	// runs the same credential-proxy workload as claude-code's sidecar (AC-E6);
	// only its placement and bind address differ (AC-F6).
	HelperCredentialProxyContainerName = "credential-proxy"
	sessionMCPWorkload                 = "mcp"
	// SessionMCPPort is where the helper pod's MCP container serves the session
	// MCP endpoint and its readiness probe.
	SessionMCPPort     = 8092
	sessionMCPPortName = "mcp"
	// SessionMCPURLEnvVar tells the workload pod's agent where its session MCP
	// is. AC-F6 makes this MCP the only tool surface that reaches outside the
	// pod, so no marketplace plugin and no K3s MCP token are projected here.
	SessionMCPURLEnvVar = "SESSION_MCP_URL"
	// helperProxyListenAddr binds the helper pod's proxy to the pod network
	// rather than loopback: its client is the *workload* pod, one network hop
	// away (AC-F6). What keeps that reachable-from-anywhere bind safe is AC-F2's
	// ingress policy, created with the pod in network_policy.go — subject to the
	// cluster's CNI actually enforcing NetworkPolicy (docs/doc-tracker.md).
	helperProxyListenAddr = "0.0.0.0:8091"
	// CredentialProxyPlacementEnvVar tells a credential-proxy container which of
	// its two sanctioned placements it is in, so the data plane can open the
	// pod-network bind for the helper without opening it for the sidecar too.
	// Keep the values in sync with data-plane/cmd/agent (proxyPlacementEnv).
	CredentialProxyPlacementEnvVar = "DATA_PLANE_PROXY_PLACEMENT"
	proxyPlacementSidecar          = "sidecar"
	proxyPlacementHelper           = "helper"
	// ApprovalGatewayURLEnvVar, ApprovalGatewayAPIKeyEnvVar and
	// ApprovalGatewayUserIDEnvVar are projected into the MCP container only, so
	// the agent can neither address the gateway nor approve its own requests.
	ApprovalGatewayURLEnvVar       = "APPROVAL_GATEWAY_URL"
	ApprovalGatewayAPIKeyEnvVar    = "APPROVAL_GATEWAY_API_KEY"
	ApprovalGatewayUserIDEnvVar    = "APPROVAL_GATEWAY_USER_ID"
	ApprovalGatewayURLSecretKey    = "url"
	ApprovalGatewayAPIKeySecretKey = "api-key"
	ApprovalGatewayUserIDSecretKey = "user-id"
	// SessionIDEnvVar tells the MCP container which session it serves. AC-F3
	// builds each approval request's external identifier as
	// {sessionID}:{requestID}, and the gateway rejects a duplicate, so the
	// session half has to reach the container that talks to the gateway. It is
	// not a credential — it is the same id the pod already carries as a label.
	// Keep in sync with data-plane/cmd/agent (sessionIDEnv).
	SessionIDEnvVar = "SESSION_ID"

	// restoreModeEnvVar tells a restore-target agent to wait for POST /restore
	// instead of starting a fresh workload. Shell restores use CRIU; Claude Code
	// restores unpack the filesystem/output archive (AC-D4, AC-E5).
	// Keep in sync with data-plane/cmd/agent (restoreModeEnv).
	restoreModeEnvVar = "DATA_PLANE_RESTORE_MODE"

	// AgentPort is where the session agent serves /attach and /healthz. Keep
	// in sync with data-plane/cmd/agent (defaultAddr).
	AgentPort     = 8090
	agentPortName = "agent"
	// agentHealthzPath backs the pod readiness probe, so pod Ready implies a
	// live workload agent (AC-D1/E1).
	agentHealthzPath = "/healthz"
	// agentAttachPath is the readiness stream Reach opens and closes. User I/O
	// uses the workload-neutral /read and /write endpoints instead.
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
	client    kubernetes.Interface
	namespace string
	image     string
	// workloadImages holds the per-type image overrides (AC-E1). The default
	// type falls back to `image`; a type with no image configured is refused
	// rather than silently provisioned from the shell image.
	claudeCredentialsSecret string // platform Secret referenced by claude-code pods
	approvalGatewaySecret   string // platform Secret referenced by approval-gated helper pods
	workloadImages          map[session.WorkloadType]string
	shell                   string // DATA_PLANE_SHELL override injected into pods ("" = agent default)
	checkpointPrivileged    bool   // run session pods privileged for in-pod CRIU (CRIU_ENABLED)
	agentPort               int
	pollInterval            time.Duration
	readyTimeout            time.Duration
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

// WithWorkloadImage overrides the data plane image for one workload type
// (AC-E1: the control plane provisions a different data plane workload per
// type). An empty image is ignored, which is what leaves a type unconfigured —
// and an unconfigured non-default type is refused by Start rather than being
// provisioned from the shell image, so a session can never claim a type whose
// workload is not actually there.
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
// base-url and auth-token keys are projected only into the credential sidecar,
// whose required k3s-mcp-token key is projected into the main Claude container,
// and whose optional model key selects the platform default there.
func WithClaudeCredentialsSecret(name string) Option {
	return func(o *ClientOrchestrator) {
		if strings.TrimSpace(name) != "" {
			o.claudeCredentialsSecret = strings.TrimSpace(name)
		}
	}
}

// WithApprovalGatewaySecret selects the platform-managed Secret whose url,
// api-key and user-id keys are projected only into the session MCP container of
// an approval-gated session's helper pod (AC-F6). It never reaches the workload
// pod, so the agent can neither address the approval gateway nor approve its
// own requests.
func WithApprovalGatewaySecret(name string) Option {
	return func(o *ClientOrchestrator) {
		if strings.TrimSpace(name) != "" {
			o.approvalGatewaySecret = strings.TrimSpace(name)
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
		client:                  client,
		namespace:               namespace,
		image:                   defaultDataPlaneImage,
		agentPort:               AgentPort,
		claudeCredentialsSecret: defaultClaudeCredentialsSecret,
		approvalGatewaySecret:   defaultApprovalGatewaySecret,
		pollInterval:            defaultPollInterval,
		readyTimeout:            defaultReadyTimeout,
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

// Start provisions the pod set for sessionID — its dedicated workload pod and,
// for the workload types that require them, the session-scoped auxiliary pods
// that serve it — waits for each to report Ready, and returns the set with pod
// IPs recorded for later agent calls (AC-A1/A2, AC-D1/E1, AC-F4).
func (o *ClientOrchestrator) Start(ctx context.Context, sessionID string, workload WorkloadSpec) (SessionPods, error) {
	return o.startSet(ctx, sessionID, "", workload)
}

// startSet provisions a session's whole pod set. checkpointRef == "" yields a
// fresh set; a non-empty ref yields a restore-target set under fresh unique
// names. Auxiliary pods come up *first* because the workload pod is configured
// with their addresses: AC-F4 states the restored workload pod is injected with
// the helper pod created for that restore, which is only knowable once it is
// Ready and has an IP. If the workload pod then fails, the auxiliary pods are
// reclaimed with it rather than left behind (AC-A3 hygiene) — and AC-F2's
// network policies go with them, because the helper pod owns them.
func (o *ClientOrchestrator) startSet(ctx context.Context, sessionID, checkpointRef string, workload WorkloadSpec) (SessionPods, error) {
	workloadType, err := session.NormalizeWorkloadType(workload.Type)
	if err != nil {
		return SessionPods{}, err
	}
	suffix := ""
	if checkpointRef != "" {
		if suffix, err = restoreSuffix(); err != nil {
			return SessionPods{}, err
		}
	}

	var auxiliary []PodRef
	var helper helperEndpoints
	if workloadType == session.WorkloadTypeApprovalGated {
		spec, err := o.helperPodSpec(sessionID, suffix, workloadType)
		if err != nil {
			return SessionPods{}, err
		}
		ref, err := o.provision(ctx, spec, func(created *corev1.Pod) error {
			return o.applySessionNetworkPolicies(ctx, sessionID, created)
		})
		if err != nil {
			return SessionPods{}, err
		}
		if ref.IP == "" {
			o.cleanup(ref)
			return SessionPods{}, fmt.Errorf("helper pod %s reported Ready without a pod IP", ref.Name)
		}
		auxiliary = append(auxiliary, ref)
		helper = endpointsFor(ref.IP)
	}

	spec, err := o.buildPod(sessionID, checkpointRef, suffix, workload, helper)
	if err != nil {
		o.cleanup(auxiliary...)
		return SessionPods{}, err
	}
	ref, err := o.provision(ctx, spec, nil)
	if err != nil {
		o.cleanup(auxiliary...)
		return SessionPods{}, err
	}
	return SessionPods{Workload: ref, Auxiliary: auxiliary}, nil
}

// helperEndpoints are the in-cluster addresses of a session's helper pod, as
// injected into its workload pod (AC-F6). The zero value means the session has
// no helper pod.
type helperEndpoints struct {
	ProxyURL string
	MCPURL   string
}

func endpointsFor(podIP string) helperEndpoints {
	return helperEndpoints{
		ProxyURL: "http://" + net.JoinHostPort(podIP, strconv.Itoa(credentialProxyPort)),
		MCPURL:   "http://" + net.JoinHostPort(podIP, strconv.Itoa(SessionMCPPort)),
	}
}

// provision creates a pod from spec, waits for its workload agent to report
// Ready, and returns its ref with the pod IP recorded. Start and RestoreInto
// differ only in the fresh-workload or restore-target spec they hand in.
func (o *ClientOrchestrator) provision(ctx context.Context, spec *corev1.Pod, afterCreate func(created *corev1.Pod) error) (PodRef, error) {
	created, err := o.client.CoreV1().Pods(o.namespace).Create(ctx, spec, metav1.CreateOptions{})
	if err != nil {
		return PodRef{}, fmt.Errorf("create pod %s: %w", spec.Name, err)
	}
	ref := PodRef{Name: created.Name, Namespace: o.namespace}
	// afterCreate runs against the created object — the only point where its UID
	// is known and the pod is not yet Ready. AC-F2's policies are attached here
	// so the boundary predates any traffic the pod could carry.
	if afterCreate != nil {
		if err := afterCreate(created); err != nil {
			o.cleanup(ref)
			return PodRef{}, err
		}
	}
	pod, err := o.waitReady(ctx, ref.Name)
	if err != nil {
		// Don't leak a pod that never came up (AC-A3 hygiene).
		o.cleanup(ref)
		return PodRef{}, err
	}
	ref.IP = pod.Status.PodIP
	return ref, nil
}

// Stop deletes the given pods and reclaims their resources (AC-A3). A missing
// pod is treated as already reclaimed so the call is idempotent. PodRef.Namespace
// may be empty (the service layer builds refs from the stored pod name only); it
// falls back to the orchestrator's namespace. Deletion stops at the first real
// error so the caller sees it rather than a partially reclaimed session.
func (o *ClientOrchestrator) Stop(ctx context.Context, refs ...PodRef) error {
	for _, ref := range refs {
		if err := o.stopOne(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

func (o *ClientOrchestrator) stopOne(ctx context.Context, ref PodRef) error {
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

// RestoreInto provisions the pod set a session archive is restored into
// (AC-B2). The workload-specific annotation and restore mode tell the agent
// whether to accept a shell CRIU archive or a filesystem archive. Applying the
// archive bytes is the Checkpointer's job; supplying the correctly shaped pods
// is all the orchestrator owns. A type with auxiliary pods gets a fresh set of
// those too — helper pods keep no state of their own (AC-F4).
func (o *ClientOrchestrator) RestoreInto(ctx context.Context, sessionID, checkpointRef string, workload WorkloadSpec) (SessionPods, error) {
	return o.startSet(ctx, sessionID, checkpointRef, workload)
}

// imageFor resolves the data plane image for a workload type (AC-E1). The
// default type falls back to the orchestrator's `image` (DATA_PLANE_IMAGE);
// any other type must have been configured with WithWorkloadImage, otherwise
// provisioning fails loudly instead of handing the session a shell pod that
// does not run the workload its type promises.
func (o *ClientOrchestrator) imageFor(workload session.WorkloadType) (string, error) {
	if img, ok := o.workloadImages[workload]; ok {
		return img, nil
	}
	if workload == session.WorkloadTypeShell {
		return o.image, nil
	}
	return "", fmt.Errorf("no data plane image configured for workload type %q", workload)
}

// buildPod assembles the session's workload pod. checkpointRef == "" yields a
// fresh session pod (no annotation) under the session's deterministic name; a
// non-empty ref yields a restore-target pod named with the provisioning round's
// suffix, which its helper pod shares so a set is recognisable as one round.
// The workload type picks the image and is recorded on the pod (label + env) so
// the pod runs — and advertises — the workload its session selected (AC-E1).
// helper carries the addresses of the session's helper pod for the types that
// have one; it is the zero value otherwise.
func (o *ClientOrchestrator) buildPod(sessionID, checkpointRef, suffix string, workload WorkloadSpec, helper helperEndpoints) (*corev1.Pod, error) {
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
	// selected agent. In restore mode that agent waits for the Checkpointer to
	// stream the workload archive. The control plane only orchestrates.
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
			// Keep platform-default as the immutable API/session alias while
			// resolving its effective value when the pod starts. The key is
			// optional so existing Secrets retain the previous Claude CLI default
			// behaviour; explicit per-session models never read this key.
			modelEnv = optionalSecretEnv(
				ClaudeCodeModelEnvVar,
				o.claudeCredentialsSecret,
				ClaudeCodeModelSecretKey,
			)
		}
		container.Env = append(container.Env,
			modelEnv,
			secretEnv(K3SMCPTokenEnvVar, o.claudeCredentialsSecret, ClaudeCodeK3SMCPTokenSecretKey),
			optionalSecretEnv(K3SMCPURLEnvVar, o.claudeCredentialsSecret, ClaudeCodeK3SMCPURLSecretKey),
			optionalSecretEnv(
				ClaudeCodePluginMarketplaceURLEnvVar,
				o.claudeCredentialsSecret,
				ClaudeCodePluginMarketplaceURLSecretKey,
			),
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
	case session.WorkloadTypeApprovalGated:
		// AC-F6: this container holds *no* external credential. The provider
		// token stays in the helper pod's proxy container and the gateway key in
		// its MCP container, so the workload pod only learns two in-cluster
		// addresses and the same non-secret placeholder claude-code uses. The
		// K3s MCP token and the marketplace plugin bootstrap are deliberately
		// absent (AC-F6's 2026-09-03 decision): the session MCP is this type's
		// only tool surface that leaves the pod.
		modelEnv := corev1.EnvVar{Name: ClaudeCodeModelEnvVar, Value: model}
		if model == session.PlatformDefaultModel {
			modelEnv = optionalSecretEnv(
				ClaudeCodeModelEnvVar,
				o.claudeCredentialsSecret,
				ClaudeCodeModelSecretKey,
			)
		}
		container.Env = append(container.Env,
			modelEnv,
			corev1.EnvVar{Name: claudeCodeStateDirEnvVar, Value: claudeCodeStateDir},
			corev1.EnvVar{Name: AnthropicBaseURLEnvVar, Value: helper.ProxyURL},
			corev1.EnvVar{Name: AnthropicAuthTokenEnvVar, Value: credentialProxyPlaceholderToken},
			corev1.EnvVar{Name: SessionMCPURLEnvVar, Value: helper.MCPURL},
		)
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: claudeCodeStateVolumeName, MountPath: claudeCodeStateVolumePath,
		})
		volumes = append(volumes, corev1.Volume{
			Name:         claudeCodeStateVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		// No sidecar: moving the proxy out of this pod is the whole point of
		// AC-F2's arrangement. AC-F5's shared RWX volume is not implemented yet.
	}
	if o.checkpointPrivileged && workloadType == session.WorkloadTypeShell {
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
		if suffix == "" {
			return nil, fmt.Errorf("restore target for session %s has no provisioning suffix", sessionID)
		}
		name = restorePodName(sessionID, suffix)
		annotation := AnnotationRestoreCheckpoint
		if workloadType != session.WorkloadTypeShell {
			// The agent types unpack a filesystem archive and never invoke CRIU
			// (AC-E5; AC-F5 adds the shared volume to that archive once it lands).
			annotation = AnnotationRestoreArchive
		}
		annotations = map[string]string{annotation: checkpointRef}
		// The agent waits for the shell checkpoint or Claude filesystem archive
		// on POST /restore before accepting workload I/O.
		container.Env = append(container.Env, corev1.EnvVar{Name: restoreModeEnvVar, Value: "1"})
	}
	containers := append([]corev1.Container{container}, sidecars...)
	// Session workloads intentionally receive a Kubernetes identity so they can
	// inspect cluster resources. Its ClusterRoleBinding uses the built-in `view`
	// role rather than a wildcard rule because RBAC cannot subtract Secrets from
	// an otherwise-wildcard grant.
	automountServiceAccountToken := true

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   o.namespace,
			Annotations: annotations,
			Labels: map[string]string{
				LabelSessionID:    sessionID,
				LabelWorkloadType: string(workloadType),
				LabelPodRole:      PodRoleWorkload,
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

// helperPodSpec assembles an approval-gated session's helper pod: one
// session-scoped auxiliary pod holding two containers — the session MCP that
// will front the approval gate (AC-F3) and the provider credential proxy that
// AC-F2 moved out of the workload pod (AC-F4/AC-F6). It runs no session
// workload, so it is an auxiliary pod under AC-A2 rather than a second workload
// pod, and it keeps no state of its own: freeze discards it and restore builds
// a new one.
//
// Unlike the workload pod it gets no Kubernetes identity — nothing in it needs
// the API server, and the pod holds both of the platform's external secrets.
func (o *ClientOrchestrator) helperPodSpec(sessionID, suffix string, workloadType session.WorkloadType) (*corev1.Pod, error) {
	image, err := o.imageFor(workloadType)
	if err != nil {
		return nil, err
	}
	name := helperPodName(sessionID)
	if suffix != "" {
		name = helperRestorePodName(sessionID, suffix)
	}
	automountServiceAccountToken := false

	mcp := corev1.Container{
		Name:            SessionMCPContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicyForImage(image),
		Ports: []corev1.ContainerPort{{
			Name:          sessionMCPPortName,
			ContainerPort: SessionMCPPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env: []corev1.EnvVar{
			{Name: workloadEnvVar, Value: sessionMCPWorkload},
			{Name: agentAddrEnvVar, Value: ":" + strconv.Itoa(SessionMCPPort)},
			// Not a secret: the session half of AC-F3's external identifier.
			{Name: SessionIDEnvVar, Value: sessionID},
			// AC-F6: the gateway triple lives here and nowhere else. The
			// notification target is a platform-wide value, never per session.
			secretEnv(ApprovalGatewayURLEnvVar, o.approvalGatewaySecret, ApprovalGatewayURLSecretKey),
			secretEnv(ApprovalGatewayAPIKeyEnvVar, o.approvalGatewaySecret, ApprovalGatewayAPIKeySecretKey),
			secretEnv(ApprovalGatewayUserIDEnvVar, o.approvalGatewaySecret, ApprovalGatewayUserIDSecretKey),
		},
		// An exec probe for the same reason the proxy container uses one, plus
		// one specific to this pod: an HTTP probe originates at the kubelet, not
		// at a pod, and AC-F2's ingress policy admits only this session's
		// workload pod. Under a CNI that enforces the policy, an HTTP probe
		// would be the one caller the boundary is designed to reject — and the
		// helper pod would never reach Ready.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"/bin/bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(SessionMCPPort),
			}}},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
		},
		SecurityContext: hardenedSecurityContext(),
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: o.namespace,
			Labels: map[string]string{
				LabelSessionID:    sessionID,
				LabelWorkloadType: string(workloadType),
				LabelPodRole:      PodRoleHelper,
				labelManagedBy:    managedByValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: &automountServiceAccountToken,
			Containers: []corev1.Container{
				mcp,
				o.credentialProxyContainer(HelperCredentialProxyContainerName, image, helperProxyListenAddr, proxyPlacementHelper),
			},
		},
	}, nil
}

// claudeCredentialProxy holds provider credentials outside the tool-running
// container. Kubernetes gives pod containers a shared network namespace but
// separate PID/filesystem namespaces, so Claude can call this loopback service
// without being able to read its Secret-backed environment or /proc entries.
func (o *ClientOrchestrator) claudeCredentialProxy(image string) corev1.Container {
	return o.credentialProxyContainer(ClaudeCredentialsContainerName, image, credentialProxyListenAddr, proxyPlacementSidecar)
}

// credentialProxyContainer builds the provider proxy in either of its two
// placements. AC-F6 keeps one behaviour contract (AC-E6) for both and lets only
// the placement differ: claude-code runs it as a loopback sidecar in the
// workload pod, approval-gated runs it in the session's helper pod bound to the
// pod network, because its client is a pod away. The Secret projection is the
// same in both — the provider token exists only in this container's environment.
func (o *ClientOrchestrator) credentialProxyContainer(name, image, listenAddr, placement string) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: pullPolicyForImage(image),
		Env: []corev1.EnvVar{
			{Name: workloadEnvVar, Value: credentialProxyWorkload},
			{Name: agentAddrEnvVar, Value: listenAddr},
			// Declared rather than inferred from the bind address: the data
			// plane refuses a bind its placement does not sanction, and a
			// missing declaration means the restrictive one.
			{Name: CredentialProxyPlacementEnvVar, Value: placement},
			secretEnv(AnthropicBaseURLEnvVar, o.claudeCredentialsSecret, ClaudeCodeBaseURLSecretKey),
			secretEnv(AnthropicAuthTokenEnvVar, o.claudeCredentialsSecret, ClaudeCodeAuthTokenSecretKey),
		},
		// An exec probe reaches the proxy over loopback from inside its own
		// container, which works for both placements. An HTTP probe would come
		// from the kubelet at the pod IP and would force the loopback-bound
		// sidecar placement onto the cluster network.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"/bin/bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(credentialProxyPort),
			}}},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
		},
		SecurityContext: hardenedSecurityContext(),
	}
}

// hardenedSecurityContext is the non-root, no-capability profile the platform's
// own helper containers run under. They execute no user or model code, so
// nothing in them needs to write to the root filesystem or keep a capability.
func hardenedSecurityContext() *corev1.SecurityContext {
	runAsNonRoot := true
	runAsUser := int64(65532)
	disallowPrivilegeEscalation := false
	readOnlyRootFilesystem := true
	return &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		RunAsUser:                &runAsUser,
		RunAsGroup:               &runAsUser,
		AllowPrivilegeEscalation: &disallowPrivilegeEscalation,
		ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
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

// podName derives a deterministic, DNS-safe pod name from the session id so the
// session<->pod mapping is 1:1 and recoverable from the id alone (AC-A2).
func podName(sessionID string) string {
	return "sess-" + sessionID
}

// helperPodName derives the deterministic name of a session's helper pod
// (AC-F4). It is distinct from the workload pod's name and, like it, recoverable
// from the session id alone.
func helperPodName(sessionID string) string {
	return podName(sessionID) + "-helper"
}

// restoreSuffix generates the per-provisioning-round suffix shared by a restore
// target's pods, so a set is recognisable as one round.
//
// Maximum entropy that still keeps the 32-hex session ID in a 63-char DNS label:
// "sess-" + id + "-r" + 24 hex chars = 63.
func restoreSuffix() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate restore pod suffix: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// restorePodName names a restore-target workload pod: the session's
// deterministic name plus that round's suffix. The frozen pod carried the
// deterministic name, and its deletion (snapshot's Stop) is asynchronous — when
// a restore-on-access follows the snapshot immediately, that pod may still be
// Terminating, so reusing the name would race an AlreadyExists on create.
// A unique name removes the race and matches the service contract that restore
// provisions a *new* pod rather than reusing the old name (the session-id
// label, not the name, carries the 1:1 session mapping — AC-A2).
func restorePodName(sessionID, suffix string) string {
	return podName(sessionID) + "-r" + suffix
}

// helperRestorePodName names a restore round's helper pod. It shares the
// workload pod's suffix and differs only in the role letter, so both stay
// inside the 63-char DNS label limit and stay obviously paired.
func helperRestorePodName(sessionID, suffix string) string {
	return podName(sessionID) + "-h" + suffix
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
// No payload moves — the stdin/stdout semantics on this stream are STP-command-input/STP-output-read.
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

// cleanup best-effort deletes pods that failed to come up (or that a later
// failure in the same provisioning round orphaned), on a fresh context so a
// cancelled parent context doesn't also abort the cleanup.
func (o *ClientOrchestrator) cleanup(refs ...PodRef) {
	if len(refs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = o.Stop(ctx, refs...)
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
