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
	// CodexModelEnvVar carries the session's model, or the ConfigMap's optional
	// `model` key for platform-default sessions.
	// Keep in sync with data-plane/cmd/agent (codexModelEnv).
	CodexModelEnvVar = "CODEX_MODEL"
	// CodexReasoningEffortEnvVar carries a codex-task session's reasoning
	// effort; it is absent when the session names none (AC-M4).
	// Keep in sync with data-plane/cmd/agent.
	CodexReasoningEffortEnvVar = "CODEX_REASONING_EFFORT"
	// K3SMCPProxyContainerName is the loopback sidecar that holds the K3s MCP
	// token in a claude-code, codex-chat or codex-task pod.
	K3SMCPProxyContainerName = "k3s-mcp-proxy"
	// The K3s MCP proxy's plugin seeder reads these whichever type runs it.
	// Keep in sync with data-plane/cmd/agent (pluginSeedPluginsEnv,
	// pluginSeedMarketplaceURLEnv).
	pluginSeedPluginsEnvVar        = "PLUGIN_SEED_PLUGINS"
	pluginSeedMarketplaceURLEnvVar = "PLUGIN_SEED_MARKETPLACE_URL"
	K3SMCPToolPolicyEnvVar         = "K3S_MCP_TOOL_POLICY"

	// The sidecar keeps the session's schedules and queues their runs on the
	// agent (docs/prd/session-scheduler.md AC-P1, AC-P2). Keep in sync with
	// data-plane/cmd/agent (schedulerAgentURLEnv, schedulerMCPURLEnv,
	// schedulerMCPPath).
	SchedulerAgentURLEnvVar = "SESSION_SCHEDULER_AGENT_URL"
	SchedulerMCPURLEnvVar   = "SESSION_SCHEDULER_MCP_URL"
	schedulerMCPPath        = "/scheduler/mcp"
)

// CodexSchedulerMCPURL is the scheduler MCP on the loopback sidecar.
var CodexSchedulerMCPURL = K3SMCPProxyLoopbackURL + schedulerMCPPath

// K3SMCPProxyLoopbackURL is where the workload container reaches its sidecar.
var K3SMCPProxyLoopbackURL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(k3sMCPProxyPort))

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

// codexProfile is what the two codex types configure separately: their own
// Secret and ConfigMap, so a codex-task deployment never hands a chat session
// its token (AC-M1).
type codexProfile struct {
	credentialsSecret string
	configConfigMap   string
}

func (o *ClientOrchestrator) codexProfileFor(workload session.WorkloadType) codexProfile {
	if workload == session.WorkloadTypeCodexTask {
		return o.codexTask
	}
	return codexProfile{
		credentialsSecret: o.codexCredentialsSecret,
		configConfigMap:   o.codexConfigConfigMap,
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
		corev1.EnvVar{Name: K3SMCPProxyURLEnvVar, Value: K3SMCPProxyLoopbackURL},
		corev1.EnvVar{Name: SchedulerMCPURLEnvVar, Value: CodexSchedulerMCPURL},
	)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: claudeCodeStateVolumeName, MountPath: workloadStateVolumePath,
	})
	volumes := []corev1.Volume{
		{Name: claudeCodeStateVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		pluginSeedMarketplaceEmptyDir(),
	}
	if workload.ReasoningEffort != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: CodexReasoningEffortEnvVar, Value: workload.ReasoningEffort})
	}
	return []corev1.Container{o.codexK3SMCPProxy(sessionID, image, profile, workload.Plugins)}, volumes
}

// codexK3SMCPProxy is the loopback K3s MCP proxy (AC-J6) with codex's
// additions. Its seeder clones the marketplace for the plugin skills (AC-J5):
// the session's own selection (AC-L1), and nothing when it chose none — no type
// has a platform default (AC-A5).
func (o *ClientOrchestrator) codexK3SMCPProxy(sessionID, image string, profile codexProfile, plugins []string) corev1.Container {
	proxy := o.loopbackK3SMCPProxy(sessionID, image, profile.credentialsSecret, profile.configConfigMap, strings.Join(plugins, ","))
	proxy.Env = append(proxy.Env, corev1.EnvVar{
		Name: SchedulerAgentURLEnvVar, Value: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(o.agentPort)),
	})
	return proxy
}

// loopbackK3SMCPProxy is minimax-code's K3s MCP proxy moved from the helper pod
// into the session pod, bound to loopback: the one holder of the type's K3s MCP
// token, and the seeder of the plugins the session selected. secret and
// configMap are the type's own, so no type reads another's token.
func (o *ClientOrchestrator) loopbackK3SMCPProxy(sessionID, image, secret, configMap, plugins string) corev1.Container {
	port := strconv.Itoa(k3sMCPProxyPort)
	proxy := o.credentialProxyContainer(K3SMCPProxyContainerName, image, "127.0.0.1:"+port, proxyPlacementSidecar)
	proxy.Env = []corev1.EnvVar{
		{Name: workloadEnvVar, Value: k3sMCPProxyWorkload},
		{Name: agentAddrEnvVar, Value: "127.0.0.1:" + port},
		{Name: CredentialProxyPlacementEnvVar, Value: proxyPlacementSidecar},
		{Name: SessionIDEnvVar, Value: sessionID},
		{Name: PluginSeedMarketplaceDirEnvVar, Value: pluginSeedMarketplacePath},
		{Name: pluginSeedPluginsEnvVar, Value: plugins},
		{Name: K3SMCPToolPolicyEnvVar, Value: "allow-all"},
		optionalSecretEnv(K3SMCPTokenEnvVar, secret, ClaudeCodeK3SMCPTokenSecretKey),
		optionalConfigMapEnv(K3SMCPURLEnvVar, configMap, ClaudeCodeK3SMCPURLConfigKey),
		optionalConfigMapEnv(pluginSeedMarketplaceURLEnvVar, configMap, ClaudeCodePluginMarketplaceURLConfigKey),
	}
	proxy.VolumeMounts = []corev1.VolumeMount{{Name: pluginSeedMarketplaceVolume, MountPath: pluginSeedMarketplacePath}}
	proxy.ReadinessProbe.Exec.Command = []string{"/bin/bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/" + port}
	return proxy
}

// pluginSeedMarketplaceEmptyDir is the scratch volume the loopback proxy clones
// the marketplace into; its root filesystem is read-only.
func pluginSeedMarketplaceEmptyDir() corev1.Volume {
	return corev1.Volume{
		Name:         pluginSeedMarketplaceVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}
