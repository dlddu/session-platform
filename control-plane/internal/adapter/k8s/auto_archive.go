package k8s

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const (
	// AutoArchiveURLBaseEnvVar is the one upload destination a session pod
	// accepts an automatic-archive URL for (AC-B5): the URL without its
	// signature.
	AutoArchiveURLBaseEnvVar = "AUTO_ARCHIVE_URL_BASE"
	// AutoArchiveGracePeriodSeconds covers the archive and its upload, which
	// the agent bounds below it, and the spot interruption's two minutes.
	AutoArchiveGracePeriodSeconds int64 = 110
	// AutoArchiveRunPath archives and uploads, on the workload pod's agent.
	AutoArchiveRunPath = "/auto-archive/run"
	// AutoArchiveHoldPath keeps a gated session's helper pod relaying until
	// the workload pod's upload is through.
	AutoArchiveHoldPath = "/auto-archive/hold"

	autoArchivePreStopCommand = "auto-archive-prestop"
	agentBinaryPath           = "/agent"
)

// AutoArchiveBase returns the upload URL without its signature for a pod.
type AutoArchiveBase func(sessionID, pod string) (string, error)

// WithAutoArchive prepares the pods of the given workload types for AC-B5.
func WithAutoArchive(base AutoArchiveBase, types ...session.WorkloadType) Option {
	return func(o *ClientOrchestrator) {
		if base == nil || len(types) == 0 {
			return
		}
		o.autoArchiveBase = base
		o.autoArchiveTypes = map[session.WorkloadType]bool{}
		for _, t := range types {
			o.autoArchiveTypes[t] = true
		}
	}
}

// applyAutoArchive gives container the destination it may upload to and a
// preStop hook calling path on its own agent. The hook is exec rather than
// httpGet because a gated helper pod admits no traffic from the kubelet.
func (o *ClientOrchestrator) applyAutoArchive(pod *corev1.Pod, workloadType session.WorkloadType, sessionID, workloadPod, path string) error {
	if !o.autoArchiveTypes[workloadType] {
		return nil
	}
	base, err := o.autoArchiveBase(sessionID, workloadPod)
	if err != nil {
		return fmt.Errorf("automatic archive destination for pod %s: %w", workloadPod, err)
	}
	container := &pod.Spec.Containers[0]
	container.Env = append(container.Env, corev1.EnvVar{Name: AutoArchiveURLBaseEnvVar, Value: base})
	container.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
		Exec: &corev1.ExecAction{Command: []string{agentBinaryPath, autoArchivePreStopCommand, path}},
	}}
	grace := AutoArchiveGracePeriodSeconds
	pod.Spec.TerminationGracePeriodSeconds = &grace
	return nil
}
