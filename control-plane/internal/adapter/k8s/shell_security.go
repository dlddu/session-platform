package k8s

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// ShellUserID is the identity a shell session's container runs as. It must
// match the `session` account data-plane/Dockerfile creates, so that `id`,
// the prompt and git see a named user rather than a bare number.
const ShellUserID int64 = 1000

// shellSecurityContext confines the one workload that runs arbitrary user
// commands. The root filesystem stays writable on purpose: the home lives on
// the state EmptyDir, but ordinary shell use still writes /tmp and /var/tmp,
// and without root a writable image layer cannot be used to alter the system.
func shellSecurityContext() *corev1.SecurityContext {
	runAsNonRoot := true
	uid := ShellUserID
	allowPrivilegeEscalation := false
	return &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		RunAsUser:                &uid,
		RunAsGroup:               &uid,
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// workloadPodSecurityContext gives a shell pod an fsGroup so the state
// EmptyDir, and the restore that renames a state root into it, are owned by
// the shell user's group. Other workload types keep the pod default.
func workloadPodSecurityContext(workloadType session.WorkloadType) *corev1.PodSecurityContext {
	if workloadType != session.WorkloadTypeShell {
		return nil
	}
	gid := ShellUserID
	return &corev1.PodSecurityContext{FSGroup: &gid}
}
