package k8s

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Repo access (docs/prd/repo-access.md, AC-H3/AC-H5): the helper pod's
// session MCP asks the control plane whether a repository is on the allow
// list, files requests for ones that are not, and mints a clone token for an
// allowed one through the K3s MCP proxy beside it. None of it reaches the
// workload pod.
const (
	// Keep in sync with data-plane/cmd/agent (repoAccessURLEnv,
	// repoAccessTokenFileEnv).
	RepoAccessURLEnvVar       = "REPO_ACCESS_URL"
	RepoAccessTokenFileEnvVar = "REPO_ACCESS_TOKEN_FILE"
	// RepoAccessTokenAudience must equal the control plane's helperauth
	// audience. Like the PocketAide token it is never the apiserver's default
	// audience, so the control plane cannot replay it.
	RepoAccessTokenAudience  = "session-platform"
	repoAccessTokenVolume    = "repo-access-token"
	repoAccessTokenDir       = "/var/run/secrets/session-platform"
	repoAccessTokenPath      = "token"
	repoAccessTokenExpirySec = int64(3600)
)

// WithRepoAccess hands helper pods the control plane's in-cluster address for
// the internal repo access API. Empty leaves the feature off: no token is
// projected and approval-gated helpers run no K3s MCP proxy.
func WithRepoAccess(controlPlaneURL string) Option {
	return func(o *ClientOrchestrator) {
		o.repoAccessURL = strings.TrimRight(strings.TrimSpace(controlPlaneURL), "/")
	}
}

func (o *ClientOrchestrator) repoAccessEnabled() bool { return o.repoAccessURL != "" }

// addRepoAccess gives the session MCP container its control-plane caller
// identity and the loopback address of the K3s MCP proxy in its own pod.
func (o *ClientOrchestrator) addRepoAccess(mcp *corev1.Container, volumes []corev1.Volume) []corev1.Volume {
	if !o.repoAccessEnabled() {
		return volumes
	}
	mcp.Env = append(mcp.Env,
		corev1.EnvVar{Name: RepoAccessURLEnvVar, Value: o.repoAccessURL},
		corev1.EnvVar{Name: RepoAccessTokenFileEnvVar, Value: repoAccessTokenDir + "/" + repoAccessTokenPath},
		corev1.EnvVar{Name: K3SMCPProxyURLEnvVar, Value: "http://127.0.0.1:" + strconv.Itoa(k3sMCPProxyPort)},
	)
	mcp.VolumeMounts = append(mcp.VolumeMounts, corev1.VolumeMount{
		Name: repoAccessTokenVolume, MountPath: repoAccessTokenDir, ReadOnly: true,
	})
	expiry := repoAccessTokenExpirySec
	return append(volumes, corev1.Volume{
		Name: repoAccessTokenVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{
				ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience:          RepoAccessTokenAudience,
					ExpirationSeconds: &expiry,
					Path:              repoAccessTokenPath,
				},
			}},
		}},
	})
}
