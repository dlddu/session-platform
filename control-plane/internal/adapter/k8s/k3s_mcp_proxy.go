package k8s

import (
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// The K3s MCP proxy in an approval-gated helper pod — roadmap entry 2
// ("K3s MCP 재노출", docs/session-mcp-tool-surface.md). Repo access already
// needs this container (AC-H5); the re-export needs it on its own, so a
// deployment can offer the K3s MCP's read tools without offering repo access.
//
// The token stays in this container. The session MCP container receives only
// the loopback address of it (AC-F6 C1) and reaches it over the pod's own
// network namespace, so the helper's ingress boundary stays at two ports and
// the workload pod has no route to the K3s MCP at all (AC-F6 C2, AC-F2). The
// session MCP is the only thing the agent can call, and every call it makes
// passes the approval gate first.

// WithK3SMCPProxy turns the K3s MCP proxy container on for approval-gated
// helper pods that do not have repo access.
func WithK3SMCPProxy(enabled bool) Option {
	return func(o *ClientOrchestrator) {
		o.k3sMCPProxyEnabled = enabled
	}
}

// k3sMCPProxyAvailable reports whether the helper pod runs the K3s MCP proxy
// container: repo access needs it, and the re-export asks for it on its own.
func (o *ClientOrchestrator) k3sMCPProxyAvailable() bool {
	return o.repoAccessEnabled() || o.k3sMCPProxyEnabled
}

// addK3SMCPProxy gives the session MCP container the loopback address of the
// K3s MCP proxy in its own pod. The address is not a credential and the token
// never comes with it — the proxy installs its own bearer (AC-F6 C1, C4).
//
// Repo access already hands over the same address, so this only adds it when
// repo access is off and the re-export asked for the proxy on its own.
func (o *ClientOrchestrator) addK3SMCPProxy(mcp *corev1.Container) {
	if o.repoAccessEnabled() || !o.k3sMCPProxyEnabled {
		return
	}
	mcp.Env = append(mcp.Env, corev1.EnvVar{
		Name:  K3SMCPProxyURLEnvVar,
		Value: "http://127.0.0.1:" + strconv.Itoa(k3sMCPProxyPort),
	})
}
