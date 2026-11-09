//go:build integration

// AC-J1/AC-J3/AC-J6 on the real client-go orchestrator: the pod a codex
// session is submitted as. Same fake clientset helpers as the other suites.
package integration_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const codexImage = "ghcr.io/dlddu/session-platform-codex:dev"

func startCodex(t *testing.T, sessionID, model string) (corev1.Pod, k8s.SessionPods) {
	t.Helper()
	orch, cs := newReadyOrchestrator(t,
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeCodexChat, codexImage),
		k8s.WithCodexCredentialsSecret("codex-k3s"),
		k8s.WithCodexConfigConfigMap("codex-cfg"),
		k8s.WithCodexPlugins("session-platform,extra"),
	)
	slot := 0
	started, err := orch.Start(context.Background(), sessionID, k8s.WorkloadSpec{Type: session.WorkloadTypeCodexChat, Model: model, AuthSlot: &slot})
	if err != nil {
		t.Fatalf("start codex session: %v", err)
	}
	pods := listPods(t, cs)
	if len(pods) != 1 {
		t.Fatalf("pods = %d, want exactly the workload pod (no helper pod)", len(pods))
	}
	return pods[0], started
}

func TestCodex_OnePodWithLoopbackK3SProxy(t *testing.T) {
	pod, started := startCodex(t, "cx01", session.PlatformDefaultModel)
	if len(started.Auxiliary) != 0 {
		t.Fatalf("auxiliary pods = %+v, want none", started.Auxiliary)
	}
	if got := pod.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeCodexChat) {
		t.Errorf("workload label = %q", got)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false: no codex tool can use an API credential", pod.Spec.AutomountServiceAccountToken)
	}
	if n := len(pod.Spec.Containers); n != 2 {
		t.Fatalf("containers = %d, want agent + k3s-mcp-proxy", n)
	}
	agent := container(t, pod, k8s.ContainerName)
	if v, _ := envValue(agent, "DATA_PLANE_WORKLOAD"); v != "codex-chat" {
		t.Errorf("agent workload = %q", v)
	}
	if v, _ := envValue(agent, k8s.K3SMCPProxyURLEnvVar); v != "http://127.0.0.1:8093" {
		t.Errorf("agent %s = %q, want the loopback sidecar", k8s.K3SMCPProxyURLEnvVar, v)
	}
	for _, e := range agent.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("agent env %s reads Secret %s: the workload container must hold no platform credential (AC-J6)", e.Name, e.ValueFrom.SecretKeyRef.Name)
		}
		if e.Name == k8s.CodexModelEnvVar && (e.ValueFrom == nil || e.ValueFrom.ConfigMapKeyRef == nil ||
			e.ValueFrom.ConfigMapKeyRef.Name != "codex-cfg" || e.ValueFrom.ConfigMapKeyRef.Optional == nil || !*e.ValueFrom.ConfigMapKeyRef.Optional) {
			t.Errorf("platform-default model must come from codex-cfg's optional key, got %+v", e)
		}
	}
	proxy := container(t, pod, k8s.CodexK3SMCPProxyContainerName)
	want := map[string]string{
		"DATA_PLANE_WORKLOAD":        "k3s-mcp-proxy",
		"DATA_PLANE_AGENT_ADDR":      "127.0.0.1:8093",
		"DATA_PLANE_PROXY_PLACEMENT": "sidecar",
		"SESSION_ID":                 "cx01",
		"MINIMAX_CODE_PLUGINS":       "session-platform,extra",
		"K3S_MCP_TOOL_POLICY":        "allow-all",
	}
	for name, value := range want {
		if got, _ := envValue(proxy, name); got != value {
			t.Errorf("proxy %s = %q, want %q", name, got, value)
		}
	}
	var token bool
	for _, e := range proxy.Env {
		if e.Name == k8s.K3SMCPTokenEnvVar {
			token = e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name == "codex-k3s" &&
				e.ValueFrom.SecretKeyRef.Optional != nil && *e.ValueFrom.SecretKeyRef.Optional
		}
	}
	if !token {
		t.Errorf("proxy must read the optional k3s-mcp-token from codex-k3s")
	}
	if proxy.SecurityContext == nil || proxy.SecurityContext.ReadOnlyRootFilesystem == nil || !*proxy.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("proxy must run hardened with a read-only root filesystem")
	}
}

func TestCodex_ConcreteModelIsLiteral(t *testing.T) {
	pod, _ := startCodex(t, "cx02", "gpt-6-sol")
	agent := container(t, pod, k8s.ContainerName)
	if v, ok := envValue(agent, k8s.CodexModelEnvVar); !ok || v != "gpt-6-sol" {
		t.Errorf("%s = %q, want the literal session model", k8s.CodexModelEnvVar, v)
	}
}

// AC-N6: the codex-chat slot is split by subPath. The workload container
// mounts only the login directory, the sidecar only the database directory,
// and the root slot-init container that prepares both is the one place the
// whole slot is mounted.
func TestCodexChat_SlotSplitsLoginAndDatabase(t *testing.T) {
	pod, _ := startCodex(t, "cx03", session.PlatformDefaultModel)
	claim := k8s.AuthSlotClaimName(pod.Namespace, k8s.AuthPoolCodexChat, 0)
	var claims []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(claims) != 1 || claims[0] != claim {
		t.Fatalf("pod claims = %v, want only %s", claims, claim)
	}
	slotMount := func(c corev1.Container) (corev1.VolumeMount, bool) {
		for _, m := range c.VolumeMounts {
			if m.Name == k8s.AuthSlotVolumeName {
				return m, true
			}
		}
		return corev1.VolumeMount{}, false
	}

	agent := container(t, pod, k8s.ContainerName)
	if m, ok := slotMount(agent); !ok || m.MountPath != k8s.AuthSlotMountPath || m.SubPath != "auth" {
		t.Errorf("agent slot mount = %+v, want %s from subPath auth", m, k8s.AuthSlotMountPath)
	}
	if v, _ := envValue(agent, k8s.AuthSlotDirEnvVar); v != k8s.AuthSlotMountPath {
		t.Errorf("agent %s = %q", k8s.AuthSlotDirEnvVar, v)
	}
	if v, _ := envValue(agent, k8s.SessionDBMCPURLEnvVar); v != "http://127.0.0.1:8093/session-db/mcp" {
		t.Errorf("agent %s = %q, want the sidecar's database MCP", k8s.SessionDBMCPURLEnvVar, v)
	}
	if _, ok := envValue(agent, k8s.SessionDBDirEnvVar); ok {
		t.Errorf("agent has %s: the workload container must not see the database", k8s.SessionDBDirEnvVar)
	}

	proxy := container(t, pod, k8s.CodexK3SMCPProxyContainerName)
	if m, ok := slotMount(proxy); !ok || m.MountPath != k8s.SessionDBMountPath || m.SubPath != "db/cx03" {
		t.Errorf("proxy slot mount = %+v, want %s from subPath db/cx03", m, k8s.SessionDBMountPath)
	}
	if v, _ := envValue(proxy, k8s.SessionDBDirEnvVar); v != k8s.SessionDBMountPath {
		t.Errorf("proxy %s = %q", k8s.SessionDBDirEnvVar, v)
	}

	if n := len(pod.Spec.InitContainers); n != 1 {
		t.Fatalf("init containers = %d, want slot-init", n)
	}
	init := pod.Spec.InitContainers[0]
	if m, ok := slotMount(init); !ok || m.SubPath != "" {
		t.Errorf("slot-init mount = %+v, want the whole slot", m)
	}
	if sc := init.SecurityContext; sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 {
		t.Errorf("slot-init security context = %+v, want no escalation, read-only root and ALL dropped", sc)
	}
}

// codex-chat now needs a slot like codex-task (AC-N6): a spec without one is
// refused before any pod exists.
func TestCodexChat_RefusesWithoutSlot(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeCodexChat, codexImage),
	)
	if _, err := orch.Start(context.Background(), "cx04", k8s.WorkloadSpec{Type: session.WorkloadTypeCodexChat}); err == nil {
		t.Fatal("codex-chat started without an auth slot")
	}
	if n := len(listPods(t, cs)); n != 0 {
		t.Fatalf("pods = %d, want none", n)
	}
}
