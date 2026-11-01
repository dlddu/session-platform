package k8s

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// drop 파일 전달(docs/prd/drop-file-share.md AC-I2): 헬퍼 파드의 세션 MCP 가 drop 에 자기
// 서비스 어카운트로 닿는 자리. 워크로드 파드는 drop 에 닿지 않고(AC-F2 그대로) 토큰도 받지 않는다.
const (
	// data-plane/cmd/agent 의 dropURLEnv·dropTokenFileEnv 와 맞춘다.
	DropURLEnvVar       = "DROP_URL"
	DropTokenFileEnvVar = "DROP_TOKEN_FILE"
	// DropTokenAudience 는 drop 의 SA_TOKEN_AUDIENCE 와 같아야 한다. 다른 audience 토큰처럼
	// apiserver 의 기본 audience 가 아니므로 drop 이 이 토큰으로 apiserver 를 부를 수 없다.
	DropTokenAudience  = "drop"
	dropTokenVolume    = "drop-token"
	dropTokenDir       = "/var/run/secrets/drop"
	dropTokenPath      = "token"
	dropTokenExpirySec = int64(3600)
)

// WithDropURL 은 헬퍼 파드에 drop 의 클러스터 안 주소를 준다. 비우면 기능이 꺼진다 — 토큰을
// 투영하지 않고 세션 MCP 의 도구 목록에 drop 도구가 오르지 않는다.
func WithDropURL(url string) Option {
	return func(o *ClientOrchestrator) {
		o.dropURL = strings.TrimRight(strings.TrimSpace(url), "/")
	}
}

func (o *ClientOrchestrator) dropEnabled() bool { return o.dropURL != "" }

// addDrop 은 세션 MCP 컨테이너에만 drop 주소와 audience drop 의 projected 토큰을 준다.
func (o *ClientOrchestrator) addDrop(mcp *corev1.Container, volumes []corev1.Volume) []corev1.Volume {
	if !o.dropEnabled() {
		return volumes
	}
	mcp.Env = append(mcp.Env,
		corev1.EnvVar{Name: DropURLEnvVar, Value: o.dropURL},
		corev1.EnvVar{Name: DropTokenFileEnvVar, Value: dropTokenDir + "/" + dropTokenPath},
	)
	mcp.VolumeMounts = append(mcp.VolumeMounts, corev1.VolumeMount{
		Name: dropTokenVolume, MountPath: dropTokenDir, ReadOnly: true,
	})
	expiry := dropTokenExpirySec
	return append(volumes, corev1.Volume{
		Name: dropTokenVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{
				ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience:          DropTokenAudience,
					ExpirationSeconds: &expiry,
					Path:              dropTokenPath,
				},
			}},
		}},
	})
}
