package k8s

import (
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Codex runtime configuration (docs/prd/codex-workload.md, AC-J1~J6, and
// docs/prd/codex-task-workload.md, AC-M1~M4). A codex-chat or codex-task
// session is one pod: the workload container running the Codex SDK runner and
// a loopback K3s MCP proxy sidecar. There is no provider credential to hide —
// the ChatGPT login is the session's own (AC-J3) or its auth slot's (AC-M2) —
// so the only platform secret in the pod is the K3s MCP token, and it stays in
// the sidecar (AC-J6).
const (
	defaultCodexCredentialsSecret     = "codex-credentials"
	defaultCodexConfigConfigMap       = "codex-config"
	defaultCodexTaskCredentialsSecret = "codex-task-credentials"
	defaultCodexTaskConfigConfigMap   = "codex-task-config"
	// DefaultCodexPlugins is the marketplace plugin whose skills a codex-chat
	// or codex-task session gets when the deployment names none (AC-J5, AC-M1).
	DefaultCodexPlugins = "codex-session"

	// CodexModelEnvVar carries the session's model, or the ConfigMap's optional
	// `model` key for platform-default sessions.
	// Keep in sync with data-plane/cmd/agent (codexModelEnv).
	CodexModelEnvVar = "CODEX_MODEL"
	// CodexReasoningEffortEnvVar carries a codex-task session's reasoning
	// effort; it is absent when the session names none (AC-M4).
	// Keep in sync with data-plane/cmd/agent.
	CodexReasoningEffortEnvVar = "CODEX_REASONING_EFFORT"
	// CodexK3SMCPProxyContainerName is the sidecar that holds the K3s MCP token.
	CodexK3SMCPProxyContainerName = "k3s-mcp-proxy"
	// Keep in sync with data-plane/cmd/agent (miniMaxPluginsEnv,
	// miniMaxMarketplaceURLEnv): the sidecar reuses minimax-code's seeder.
	pluginSeedPluginsEnvVar        = "MINIMAX_CODE_PLUGINS"
	pluginSeedMarketplaceURLEnvVar = "MINIMAX_CODE_PLUGIN_MARKETPLACE_URL"
	K3SMCPToolPolicyEnvVar         = "K3S_MCP_TOOL_POLICY"
)

// CodexK3SMCPProxyURL is where the workload container reaches its sidecar.
var CodexK3SMCPProxyURL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(k3sMCPProxyPort))

// WithCodexCredentialsSecret selects the Secret whose optional k3s-mcp-token
// reaches the codex sidecar.
func WithCodexCredentialsSecret(name string) Option {
	return func(o *ClientOrchestrator) {
		if name = strings.TrimSpace(name); name != "" {
			o.codexCredentialsSecret = name
		}
	}
}

// WithCodexConfigConfigMap selects the ConfigMap holding codex's
// non-credential keys (model, k3s-mcp-url, plugin-marketplace-url).
func WithCodexConfigConfigMap(name string) Option {
	return func(o *ClientOrchestrator) {
		if name = strings.TrimSpace(name); name != "" {
			o.codexConfigConfigMap = name
		}
	}
}

// WithCodexPlugins names the marketplace plugins (comma-separated) whose
// skills codex sessions get.
func WithCodexPlugins(plugins string) Option {
	return func(o *ClientOrchestrator) {
		if plugins = strings.TrimSpace(plugins); plugins != "" {
			o.codexPlugins = plugins
		}
	}
}

// WithCodexTaskCredentialsSecret selects the Secret whose optional
// k3s-mcp-token reaches the codex-task sidecar (AC-M1).
func WithCodexTaskCredentialsSecret(name string) Option {
	return func(o *ClientOrchestrator) {
		if name = strings.TrimSpace(name); name != "" {
			o.codexTask.credentialsSecret = name
		}
	}
}

// WithCodexTaskConfigConfigMap selects codex-task's non-credential ConfigMap.
func WithCodexTaskConfigConfigMap(name string) Option {
	return func(o *ClientOrchestrator) {
		if name = strings.TrimSpace(name); name != "" {
			o.codexTask.configConfigMap = name
		}
	}
}

// WithCodexTaskPlugins names the marketplace plugins codex-task sessions get.
func WithCodexTaskPlugins(plugins string) Option {
	return func(o *ClientOrchestrator) {
		if plugins = strings.TrimSpace(plugins); plugins != "" {
			o.codexTask.plugins = plugins
		}
	}
}

// codexProfile is what the two codex types configure separately: their own
// Secret, ConfigMap and plugin bundle, so a codex-task deployment never hands
// a chat session its token or skills (AC-M1).
type codexProfile struct {
	credentialsSecret string
	configConfigMap   string
	plugins           string
}

func (o *ClientOrchestrator) codexProfileFor(workload session.WorkloadType) codexProfile {
	if workload == session.WorkloadTypeCodexTask {
		return o.codexTask
	}
	return codexProfile{
		credentialsSecret: o.codexCredentialsSecret,
		configConfigMap:   o.codexConfigConfigMap,
		plugins:           o.codexPlugins,
	}
}

// codexPod fills in the workload container of a codex-chat or codex-task pod
// and returns its sidecar and volumes. The state volume and state-dir variable
// are the other agent types' on purpose: the data plane runs every CLI on one
// runner and archives one tree (AC-J4, AC-M3).
func (o *ClientOrchestrator) codexPod(container *corev1.Container, sessionID string, workload WorkloadSpec, workloadType session.WorkloadType, model, image string) ([]corev1.Container, []corev1.Volume) {
	profile := o.codexProfileFor(workloadType)
	modelEnv := corev1.EnvVar{Name: CodexModelEnvVar, Value: model}
	if model == session.PlatformDefaultModel {
		modelEnv = optionalConfigMapEnv(CodexModelEnvVar, profile.configConfigMap, ClaudeCodeModelConfigKey)
	}
	container.Env = append(container.Env,
		modelEnv,
		corev1.EnvVar{Name: claudeCodeStateDirEnvVar, Value: workloadStateDir},
		corev1.EnvVar{Name: K3SMCPProxyURLEnvVar, Value: CodexK3SMCPProxyURL},
	)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: claudeCodeStateVolumeName, MountPath: workloadStateVolumePath,
	})
	volumes := []corev1.Volume{
		{Name: claudeCodeStateVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: miniMaxMarketplaceVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if workload.ReasoningEffort != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: CodexReasoningEffortEnvVar, Value: workload.ReasoningEffort})
	}
	return []corev1.Container{o.codexK3SMCPProxy(sessionID, image, profile)}, volumes
}

// codexK3SMCPProxy is minimax-code's K3s MCP proxy moved from the helper pod
// into the session pod, bound to loopback (AC-J6). Its seeder clones the
// marketplace for the plugin skills (AC-J5).
func (o *ClientOrchestrator) codexK3SMCPProxy(sessionID, image string, profile codexProfile) corev1.Container {
	port := strconv.Itoa(k3sMCPProxyPort)
	proxy := o.credentialProxyContainer(CodexK3SMCPProxyContainerName, image, "127.0.0.1:"+port, proxyPlacementSidecar)
	proxy.Env = []corev1.EnvVar{
		{Name: workloadEnvVar, Value: k3sMCPProxyWorkload},
		{Name: agentAddrEnvVar, Value: "127.0.0.1:" + port},
		{Name: CredentialProxyPlacementEnvVar, Value: proxyPlacementSidecar},
		{Name: SessionIDEnvVar, Value: sessionID},
		{Name: MiniMaxMarketplaceDirEnvVar, Value: miniMaxMarketplacePath},
		{Name: pluginSeedPluginsEnvVar, Value: profile.plugins},
		{Name: K3SMCPToolPolicyEnvVar, Value: "allow-all"},
		optionalSecretEnv(K3SMCPTokenEnvVar, profile.credentialsSecret, MiniMaxK3SMCPTokenSecretKey),
		optionalConfigMapEnv(K3SMCPURLEnvVar, profile.configConfigMap, ClaudeCodeK3SMCPURLConfigKey),
		optionalConfigMapEnv(pluginSeedMarketplaceURLEnvVar, profile.configConfigMap, ClaudeCodePluginMarketplaceURLConfigKey),
	}
	proxy.VolumeMounts = []corev1.VolumeMount{{Name: miniMaxMarketplaceVolume, MountPath: miniMaxMarketplacePath}}
	proxy.ReadinessProbe.Exec.Command = []string{"/bin/bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/" + port}
	return proxy
}
