package k8s

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
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
