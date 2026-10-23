package k8s

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// MiniMax Code runtime configuration (AC-G3, AC-G6). The pod shape is
// approval-gated's (AC-F2~F6): the CLI container holds no credential and
// reaches the world only through its session's helper pod, which carries the
// session MCP, a provider credential proxy and a K3s MCP proxy. Only the Secret
// the two proxies read differs from approval-gated, so a deployment can hold an
// Anthropic token and a MiniMax subscription key side by side without one type
// ever seeing the other's.
const (
	defaultMiniMaxCredentialsSecret = "minimax-code-credentials"
	// MiniMaxCodeModelEnvVar carries the session's model, or the Secret's
	// optional `model` key for platform-default sessions. The Secret keys
	// themselves are claude-code's names (base-url, auth-token, model, ca-cert).
	// Keep in sync with data-plane/cmd/agent (miniMaxModelEnv).
	MiniMaxCodeModelEnvVar = "MINIMAX_CODE_MODEL"
	// MiniMaxK3SMCPProxyContainerName is the helper pod container that holds
	// this type's K3s MCP token (AC-G5). It is the only holder: the workload
	// pod reaches the K3s MCP through it, so the token never enters the
	// container whose HOME is archived.
	MiniMaxK3SMCPProxyContainerName = "k3s-mcp-proxy"
	// MiniMaxK3SMCPTokenSecretKey is optional: without it the proxy starts
	// unconfigured and the session runs without the K3s MCP tools. Same key
	// name as claude-code's, in this type's own Secret.
	MiniMaxK3SMCPTokenSecretKey = ClaudeCodeK3SMCPTokenSecretKey
	// K3SMCPProxyURLEnvVar tells the workload agent where its K3s MCP proxy is.
	// Keep in sync with data-plane/cmd/agent (k3sMCPProxyURLEnv).
	K3SMCPProxyURLEnvVar = "K3S_MCP_PROXY_URL"
	// Keep in sync with data-plane/cmd/agent (workloadK3SMCPProxy, k3sMCPProxyPort).
	k3sMCPProxyWorkload = "k3s-mcp-proxy"
	k3sMCPProxyPort     = 8093

	// MiniMaxMarketplaceDirEnvVar is where the K3s MCP proxy clones the plugin
	// marketplace (AC-G5). Its root filesystem is read-only, so the clone gets
	// an emptyDir of its own; nothing else mounts it.
	// Keep in sync with data-plane/cmd/agent (miniMaxMarketplaceDirEnv).
	MiniMaxMarketplaceDirEnvVar = "MINIMAX_CODE_MARKETPLACE_DIR"
	miniMaxMarketplaceVolume    = "marketplace"
	miniMaxMarketplacePath      = "/var/lib/session-platform-marketplace"
)

// WithMiniMaxCredentialsSecret selects the platform-managed Secret minimax-code
// pods project their provider keys from (AC-G3).
func WithMiniMaxCredentialsSecret(name string) Option {
	return func(o *ClientOrchestrator) {
		if name = strings.TrimSpace(name); name != "" {
			o.miniMaxCredentialsSecret = name
		}
	}
}

// miniMaxCodePod fills in the workload container of a minimax-code pod and
// returns the volumes the pod needs; it has no sidecar (AC-G6). The state
// volume and the state-dir variable are claude-code's on purpose: the data
// plane runs both CLIs on the same one-shot runner and archives the same tree
// (AC-G4). helperPod names the round's helper pod, whose shared claim this pod
// mounts when the deployment serves one (AC-F5).
func (o *ClientOrchestrator) miniMaxCodePod(container *corev1.Container, model string, helper helperEndpoints, helperPod string) ([]corev1.Container, []corev1.Volume) {
	modelEnv := corev1.EnvVar{Name: MiniMaxCodeModelEnvVar, Value: model}
	if model == session.PlatformDefaultModel {
		modelEnv = optionalSecretEnv(MiniMaxCodeModelEnvVar, o.miniMaxCredentialsSecret, ClaudeCodeModelSecretKey)
	}
	container.Env = append(container.Env,
		modelEnv,
		corev1.EnvVar{Name: claudeCodeStateDirEnvVar, Value: claudeCodeStateDir},
		corev1.EnvVar{Name: AnthropicBaseURLEnvVar, Value: helper.ProxyURL},
		corev1.EnvVar{Name: AnthropicAuthTokenEnvVar, Value: credentialProxyPlaceholderToken},
		corev1.EnvVar{Name: SessionMCPURLEnvVar, Value: helper.MCPURL},
		corev1.EnvVar{Name: K3SMCPProxyURLEnvVar, Value: helper.K3SMCPURL},
	)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: claudeCodeStateVolumeName, MountPath: claudeCodeStateVolumePath,
	})
	volumes := []corev1.Volume{{
		Name:         claudeCodeStateVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	if o.sharedVolumeEnabled() {
		container.VolumeMounts = append(container.VolumeMounts, sharedVolumeMount())
		container.Env = append(container.Env,
			corev1.EnvVar{Name: SharedVolumeDirEnvVar, Value: SharedVolumeMountPath})
		volumes = append(volumes, sharedVolume(sharedClaimName(helperPod)))
	}
	return nil, volumes
}

// helperProxyContainers are the helper pod's containers beside the session
// MCP. approval-gated has one, the provider proxy reading the claude-code
// Secret (AC-F4). minimax-code has that proxy re-pointed at its own Secret and
// the K3s MCP proxy (AC-G5, AC-G6).
func (o *ClientOrchestrator) helperProxyContainers(workloadType session.WorkloadType, image string) []corev1.Container {
	provider := o.credentialProxyContainer(HelperCredentialProxyContainerName, image, helperProxyListenAddr, proxyPlacementHelper)
	if workloadType != session.WorkloadTypeMiniMaxCode {
		return []corev1.Container{provider}
	}
	o.repointToMiniMaxSecret(&provider)
	return []corev1.Container{provider, o.miniMaxK3SMCPProxy(image)}
}

// repointToMiniMaxSecret moves every Secret reference of a credential proxy
// from the claude-code Secret to this type's. None is left behind (AC-G3).
func (o *ClientOrchestrator) repointToMiniMaxSecret(proxy *corev1.Container) {
	for i := range proxy.Env {
		ref := proxy.Env[i].ValueFrom
		if ref != nil && ref.SecretKeyRef != nil && ref.SecretKeyRef.Name == o.claudeCredentialsSecret {
			ref.SecretKeyRef.Name = o.miniMaxCredentialsSecret
		}
	}
}

// miniMaxK3SMCPProxy is the helper pod container in front of the K3s MCP
// (AC-G5). It reuses the provider proxy's hardening, helper placement and
// in-container probe; its workload, port, the one Secret key it reads and the
// scratch volume its marketplace clone needs differ.
func (o *ClientOrchestrator) miniMaxK3SMCPProxy(image string) corev1.Container {
	port := strconv.Itoa(k3sMCPProxyPort)
	proxy := o.credentialProxyContainer(MiniMaxK3SMCPProxyContainerName, image, ":"+port, proxyPlacementHelper)
	proxy.Env = []corev1.EnvVar{
		{Name: workloadEnvVar, Value: k3sMCPProxyWorkload},
		{Name: agentAddrEnvVar, Value: ":" + port},
		{Name: CredentialProxyPlacementEnvVar, Value: proxyPlacementHelper},
		{Name: MiniMaxMarketplaceDirEnvVar, Value: miniMaxMarketplacePath},
		optionalSecretEnv(K3SMCPTokenEnvVar, o.miniMaxCredentialsSecret, MiniMaxK3SMCPTokenSecretKey),
	}
	proxy.VolumeMounts = []corev1.VolumeMount{{Name: miniMaxMarketplaceVolume, MountPath: miniMaxMarketplacePath}}
	proxy.ReadinessProbe.Exec.Command = []string{"/bin/bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/" + port}
	return proxy
}

// helperProxyVolumes are the helper pod volumes its proxy containers need:
// the K3s MCP proxy's marketplace scratch space, for the type that runs it.
func helperProxyVolumes(workloadType session.WorkloadType) []corev1.Volume {
	if workloadType != session.WorkloadTypeMiniMaxCode {
		return nil
	}
	return []corev1.Volume{{
		Name:         miniMaxMarketplaceVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
}

// helperPolicyPorts are the helper pod ports a session's network policies
// admit (AC-F2). The K3s MCP proxy's port is admitted only for the type whose
// helper pod runs it, so approval-gated's boundary stays exactly two ports.
func helperPolicyPorts(owner *corev1.Pod) []networkingv1.NetworkPolicyPort {
	ports := []networkingv1.NetworkPolicyPort{
		tcpPort(credentialProxyPort),
		tcpPort(SessionMCPPort),
	}
	if owner.Labels[LabelWorkloadType] == string(session.WorkloadTypeMiniMaxCode) {
		ports = append(ports, tcpPort(k3sMCPProxyPort))
	}
	return ports
}
