//go:build integration

// Helper pod shape with the K3s MCP proxy on (roadmap entry 2, "K3s MCP
// 재노출", docs/session-mcp-tool-surface.md).
package integration_test

import (
	"context"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Roadmap entry 2's read slice: the approval-gated helper pod runs the K3s
// MCP proxy, and the session MCP container receives only its loopback
// address. The token stays in the proxy container (AC-F6 C1), and the
// workload pod gets neither the token nor a route to the proxy (AC-F6 C2).
func TestK3SMCPProxy_ApprovalGatedHelperRunsTheProxyWithoutRepoAccess(t *testing.T) {
	orch, cs := newMiniMaxOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage),
		k8s.WithK3SMCPProxy(true),
	)
	started, err := orch.Start(context.Background(), "k3s1", k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	set := newPodSet(t, listPods(t, cs), started)

	proxy := container(t, set.helper, k8s.MiniMaxK3SMCPProxyContainerName)
	if got, _ := envValue(proxy, "DATA_PLANE_WORKLOAD"); got != "k3s-mcp-proxy" {
		t.Errorf("workload = %q, want k3s-mcp-proxy", got)
	}
	secret, key, ok := secretKey(proxy, k8s.K3SMCPTokenEnvVar)
	if !ok || secret != "claude-provider" || key != "k3s-mcp-token" {
		t.Errorf("token selector = %s/%s (%v), want claude-provider/k3s-mcp-token", secret, key, ok)
	}

	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	if _, ok := envValue(mcp, k8s.K3SMCPTokenEnvVar); ok {
		t.Error("the session MCP container holds the K3s MCP token — it belongs to the proxy container")
	}
	if got, ok := envValue(mcp, k8s.K3SMCPProxyURLEnvVar); !ok || got != "http://127.0.0.1:8093" {
		t.Errorf("%s = %q (%v), want the loopback address of the proxy", k8s.K3SMCPProxyURLEnvVar, got, ok)
	}

	// The workload pod is where the agent lives, so it must hold neither the
	// token nor a route to the proxy.
	for _, c := range set.workload.Spec.Containers {
		if _, ok := envValue(c, k8s.K3SMCPTokenEnvVar); ok {
			t.Errorf("workload container %s holds the K3s MCP token", c.Name)
		}
		if _, ok := envValue(c, k8s.K3SMCPProxyURLEnvVar); ok {
			t.Errorf("workload container %s holds the K3s MCP proxy address", c.Name)
		}
	}
}

// Without the opt-in the helper pod is what it was: two containers, and no
// address to a K3s MCP proxy.
func TestK3SMCPProxy_ApprovalGatedHelperIsUnchangedWithoutTheOptIn(t *testing.T) {
	orch, cs := newMiniMaxOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage),
	)
	started, err := orch.Start(context.Background(), "k3s2", k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	set := newPodSet(t, listPods(t, cs), started)

	for _, c := range set.helper.Spec.Containers {
		if c.Name == k8s.MiniMaxK3SMCPProxyContainerName {
			t.Error("the helper pod runs a K3s MCP proxy without the opt-in")
		}
	}
	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	if _, ok := envValue(mcp, k8s.K3SMCPProxyURLEnvVar); ok {
		t.Error("the session MCP container holds a K3s MCP proxy address without the opt-in")
	}
}
