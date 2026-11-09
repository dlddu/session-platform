//go:build e2e

// What the approval gate's e2e files (docs/prd/approval-gate.md AC-F2~F6) share:
// which type they provision and how its helper pod is found and named. The gate
// is a capability of a type, not a type of its own, so these files stand up a
// session of a type behind it — minimax-code, today the only one. Its own
// half (AC-G) is not asserted here.
package e2e_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// gatedWorkloadType is the type the gate's e2e files create.
	gatedWorkloadType = "minimax-code"

	// That type's provider Secret and ConfigMap (AC-G3), which the helper
	// pod's provider proxy reads in place of claude-code's pair. The kind
	// overlay provisions both (deploy/overlays/e2e/minimax-code.yaml).
	gatedCredentialsSecret = "minimax-code-credentials"
	gatedConfigConfigMap   = "minimax-code-config"
	// The workload container's model variable for that type (AC-G1).
	gatedModelEnvVar = "MINIMAX_CODE_MODEL"
)

// Written out rather than imported: this suite asserts the deployed ground
// truth, so it must fail when the control plane's labels drift away from what
// the ledger and the PRD describe — importing the constants would make the
// assertion agree with any rename by construction.
const (
	labelSessionID = "session-id"
	labelPodRole   = "session-platform.dev/pod-role"
	podRoleHelper  = "helper"

	// Named as the deployed pod spec names them, for the same reason. The
	// first two are the gate's (AC-F4); the third is the gated type's own
	// (AC-G5), present beside them.
	sessionMCPContainer      = "session-mcp"
	helperCredProxyContainer = "credential-proxy"
	k3sMCPProxyContainer     = "k3s-mcp-proxy"
)

// k3sMCPProxyPortText is the gated type's third helper port (AC-G6), admitted
// beside the gate's two (helperProxyPort, sessionMCPPortText).
const k3sMCPProxyPortText = "8093"

// gatedHelperPorts are the helper ports a gated session's workload pod may dial.
var gatedHelperPorts = []string{sessionMCPPortText, helperProxyPort, k3sMCPProxyPortText}

// gatedHelperContainers is the helper pod's full container set for
// gatedWorkloadType: the gate's two plus the type's own.
var gatedHelperContainers = []string{sessionMCPContainer, helperCredProxyContainer, k3sMCPProxyContainer}

// Selecting on the session id *and* the pod role is what makes "the helper pod
// is this session's own" observable — a helper shared across sessions would show
// up here for both.
func helperPodsFor(t *testing.T, cs kubernetes.Interface, ns, sessionID string) []corev1.Pod {
	t.Helper()
	list, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSessionID + "=" + sessionID + "," + labelPodRole + "=" + podRoleHelper,
	})
	if err != nil {
		t.Fatalf("list helper pods for session %s in %s: %v", sessionID, ns, err)
	}
	return list.Items
}

func containerReady(pod *corev1.Pod, name string) (found, ready bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return true, cs.Ready
		}
	}
	return false, false
}
