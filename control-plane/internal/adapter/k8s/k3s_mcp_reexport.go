package k8s

import (
	corev1 "k8s.io/api/core/v1"
)

// K3s MCP re-export (docs/session-mcp-tool-surface.md, 등재 2번): the session
// MCP delegates a read-only slice of the K3s MCP server's tools behind the
// approval gate.
//
// The credential placement is what this file decides. The K3s MCP token stays
// in the helper pod's K3s MCP proxy container (AC-F6 C1) and the session MCP
// gets nothing but the loopback address of that proxy (C2). The proxy's port
// is not admitted to the workload pod (AC-F2), so the workload reaches the K3s
// MCP only through the gate.
const (
	// k3sMCPReexportProxyURL is the session MCP's address of the proxy in its
	// own pod. It is loopback on purpose: the session MCP has no other route
	// to the K3s MCP, and the workload pod has none at all.
	k3sMCPReexportProxyURL = "http://127.0.0.1:8093"
)

// WithK3sMCPReexport turns the K3s MCP re-export on for approval-gated
// sessions. Off leaves the tool surface as it was: no proxy container in the
// helper pod, and no tools on the session MCP.
func WithK3sMCPReexport(on bool) Option {
	return func(o *ClientOrchestrator) {
		o.k3sMCPReexport = on
	}
}

func (o *ClientOrchestrator) k3sMCPReexportEnabled() bool { return o.k3sMCPReexport }

// addK3sMCPReexport tells the session MCP where its K3s MCP proxy is. It adds
// nothing else: the token is not here, and neither is a route to the server.
func (o *ClientOrchestrator) addK3sMCPReexport(mcp *corev1.Container) {
	if !o.k3sMCPReexportEnabled() {
		return
	}
	// With repo access on, addRepoAccess already named the same proxy.
	if o.repoAccessEnabled() {
		return
	}
	mcp.Env = append(mcp.Env, corev1.EnvVar{Name: K3SMCPProxyURLEnvVar, Value: k3sMCPReexportProxyURL})
}
