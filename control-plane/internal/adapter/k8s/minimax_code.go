package k8s

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// MiniMax Code runtime configuration (AC-G3). The pod shape is claude-code's
// (AC-E6): the CLI container gets only the loopback proxy URL and the non-secret
// placeholder, and a sidecar credential proxy holds the real provider key. Only
// the Secret differs, so a deployment can hold an Anthropic token and a MiniMax
// subscription key side by side without one type ever seeing the other's.
const (
	defaultMiniMaxCredentialsSecret = "minimax-code-credentials"
	// MiniMaxCodeModelEnvVar carries the session's model, or the Secret's
	// optional `model` key for platform-default sessions. The Secret keys
	// themselves are claude-code's names (base-url, auth-token, model, ca-cert).
	// Keep in sync with data-plane/cmd/agent (miniMaxModelEnv).
	MiniMaxCodeModelEnvVar = "MINIMAX_CODE_MODEL"
	// MiniMaxCredentialsContainerName identifies this type's isolated Secret
	// holder, the counterpart of ClaudeCredentialsContainerName.
	MiniMaxCredentialsContainerName = "minimax-credentials"
)

// WithMiniMaxCredentialsSecret selects the platform-managed Secret minimax-code
// pods project their provider keys from (AC-G3).
func WithMiniMaxCredentialsSecret(name string) Option {
	return func(o *ClientOrchestrator) {
		if name != "" {
			o.miniMaxCredentialsSecret = name
		}
	}
}

// miniMaxCodePod fills in the workload container of a minimax-code pod and
// returns the sidecar and volumes the pod needs alongside it. The state volume
// and the state-dir variable are claude-code's on purpose: the data plane runs
// both CLIs on the same one-shot runner and archives the same tree (AC-G4).
func (o *ClientOrchestrator) miniMaxCodePod(container *corev1.Container, image, model string) ([]corev1.Container, []corev1.Volume) {
	modelEnv := corev1.EnvVar{Name: MiniMaxCodeModelEnvVar, Value: model}
	if model == session.PlatformDefaultModel {
		modelEnv = optionalSecretEnv(MiniMaxCodeModelEnvVar, o.miniMaxCredentialsSecret, ClaudeCodeModelSecretKey)
	}
	container.Env = append(container.Env,
		modelEnv,
		corev1.EnvVar{Name: claudeCodeStateDirEnvVar, Value: claudeCodeStateDir},
		corev1.EnvVar{Name: AnthropicBaseURLEnvVar, Value: ClaudeCredentialProxyURL},
		corev1.EnvVar{Name: AnthropicAuthTokenEnvVar, Value: credentialProxyPlaceholderToken},
	)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: claudeCodeStateVolumeName, MountPath: claudeCodeStateVolumePath,
	})
	volumes := []corev1.Volume{{
		Name:         claudeCodeStateVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	return []corev1.Container{o.miniMaxCredentialProxy(image)}, volumes
}

// miniMaxCredentialProxy is the claude-code sidecar (one behaviour contract,
// AC-E6) re-pointed at this type's Secret. Every provider key the proxy reads
// moves; none is left referring to the claude-code Secret.
func (o *ClientOrchestrator) miniMaxCredentialProxy(image string) corev1.Container {
	proxy := o.credentialProxyContainer(MiniMaxCredentialsContainerName, image, credentialProxyListenAddr, proxyPlacementSidecar)
	for i := range proxy.Env {
		ref := proxy.Env[i].ValueFrom
		if ref != nil && ref.SecretKeyRef != nil && ref.SecretKeyRef.Name == o.claudeCredentialsSecret {
			ref.SecretKeyRef.Name = o.miniMaxCredentialsSecret
		}
	}
	return proxy
}
