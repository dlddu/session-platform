package k8s

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const (
	// MonitorPairURLEnvVar and MonitorPairTokenFileEnvVar tell an agent
	// workload where the control plane's internal monitor-pair API is and
	// which file holds the token for it. Both are empty when the feature is
	// off, and the agent then has no path to the API at all.
	MonitorPairURLEnvVar       = "MONITOR_PAIR_URL"
	MonitorPairTokenFileEnvVar = "MONITOR_PAIR_TOKEN_FILE"
	// MonitorPairTokenAudience must equal the control plane's verifier
	// audience. It is its own audience, not the apiserver's default and not
	// repo access's, so a token minted for one internal API cannot be replayed
	// against the other.
	// MonitorPairRoleEnvVar is the control plane's own statement of a session's
	// side of the pair, read by the work surface in the sidecar. The sidecar
	// holds no pair token and cannot ask, so the role must arrive as
	// configuration the model cannot set. Empty is an unpaired or legacy
	// session, which keeps the whole work surface.
	MonitorPairRoleEnvVar = "MONITOR_PAIR_ROLE"
	// MonitorPairWriterTokenEnvVar is what the sidecar requires on the routes
	// that move the slot's writer seal and carry the bound monitor's writes. It
	// goes on the sidecar **only**: the workload container shares the pod's
	// loopback with it, so being in the pod cannot be the authorisation, and the
	// pod shares no process namespace, so the workload cannot read it either.
	// Empty leaves those routes refusing every caller, which is what makes the
	// pair's opt-in fail closed on a deployment that has no key.
	MonitorPairWriterTokenEnvVar = "MONITOR_PAIR_WRITER_TOKEN"
	// MonitorPairRoleParent and MonitorPairRoleMonitor are its values.
	MonitorPairRoleParent     = "parent"
	MonitorPairRoleMonitor    = "monitor"
	MonitorPairTokenAudience  = "session-platform-monitor-pair"
	monitorPairTokenVolume    = "monitor-pair-token"
	monitorPairTokenDir       = "/var/run/secrets/session-platform-monitor-pair"
	monitorPairTokenPath      = "token"
	monitorPairTokenExpirySec = int64(3600)
)

// WithMonitorPair hands agent workload pods the control plane's in-cluster
// address for the internal monitor-pair API. Empty leaves the feature off: no
// token is projected and no env var is set.
func WithMonitorPair(controlPlaneURL string) Option {
	return func(o *ClientOrchestrator) {
		o.monitorPairURL = strings.TrimRight(strings.TrimSpace(controlPlaneURL), "/")
	}
}

// attachMonitorPair projects the pair token into the workload container only.
//
// Minimal exposure, deliberately:
//   - the workload pod, never a helper pod: the pair is about the session
//     itself, and the verifier only accepts pods labelled with the workload
//     role, so a helper's token would be refused anyway;
//   - the workload container, not the sidecars: the K3s MCP proxy already
//     holds the cluster token (AC-J6) and giving it this one too would let a
//     sidecar compromise speak as the session;
//   - one audience of its own, one hour, and a file rather than an env value,
//     so the token is never in the pod spec or in a log line.
func (o *ClientOrchestrator) attachMonitorPair(volumes *[]corev1.Volume, workloadContainer *corev1.Container) {
	if o.monitorPairURL == "" || workloadContainer == nil {
		return
	}
	expiry := monitorPairTokenExpirySec
	*volumes = append(*volumes, corev1.Volume{
		Name: monitorPairTokenVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{
				ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience:          MonitorPairTokenAudience,
					ExpirationSeconds: &expiry,
					Path:              monitorPairTokenPath,
				},
			}},
		}},
	})
	workloadContainer.VolumeMounts = append(workloadContainer.VolumeMounts, corev1.VolumeMount{
		Name:      monitorPairTokenVolume,
		MountPath: monitorPairTokenDir,
		ReadOnly:  true,
	})
	workloadContainer.Env = append(workloadContainer.Env,
		corev1.EnvVar{Name: MonitorPairURLEnvVar, Value: o.monitorPairURL},
		corev1.EnvVar{Name: MonitorPairTokenFileEnvVar, Value: monitorPairTokenDir + "/" + monitorPairTokenPath},
	)
}

// monitorPairRoleEnv is the env pair the sidecar's work surface reads. It is
// set from the session record and the pair lookup, never from a request.
func (o *ClientOrchestrator) monitorPairRoleEnv(sessionID string) []corev1.EnvVar {
	if o.monitorPairURL == "" || o.monitorPairRoleOf == nil {
		return nil
	}
	// The pair store is the only source: a workload cannot talk its way into
	// another role, and a session in no pair gets no env and keeps the legacy
	// surface.
	switch role := strings.TrimSpace(o.monitorPairRoleOf(sessionID)); role {
	case MonitorPairRoleParent, MonitorPairRoleMonitor:
		return []corev1.EnvVar{{Name: MonitorPairRoleEnvVar, Value: role}}
	}
	return nil
}

// monitorPairWriterEnv is the seal credential for this session's sidecar. It is
// derived from the deployment's key, so nothing is stored and a token taken
// from one pod cannot seal another. Every session gets it, not only paired
// ones: the handover has to be possible before a pair exists.
func (o *ClientOrchestrator) monitorPairWriterEnv(sessionID string) []corev1.EnvVar {
	token := session.MonitorPairWriterToken(o.monitorPairWriterKey, sessionID)
	if token == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: MonitorPairWriterTokenEnvVar, Value: token}}
}

// WithMonitorPairWriterKey gives the orchestrator the key the seal credential is
// derived from. Without it no sidecar gets one and no slot can be handed over.
func WithMonitorPairWriterKey(key []byte) Option {
	return func(o *ClientOrchestrator) { o.monitorPairWriterKey = key }
}

// WithMonitorPairRoles gives the orchestrator the pair lookup so a parent's
// pod can be told its role at provisioning. Without it only monitors are
// marked, and a parent keeps the legacy surface.
func WithMonitorPairRoles(roleOf func(sessionID string) string) Option {
	return func(o *ClientOrchestrator) { o.monitorPairRoleOf = roleOf }
}
