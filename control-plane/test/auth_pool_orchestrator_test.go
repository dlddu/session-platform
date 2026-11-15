//go:build integration

// AC-M1/AC-M4, AC-E7/AC-E8 and AC-N4/AC-N5 on the real client-go
// orchestrator: the pods a codex-task session and a claude-code session in
// either auth mode are submitted as.
package integration_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func slot(i int) *int { return &i }

func onlyPod(t *testing.T, orch *k8s.ClientOrchestrator, pods func() []corev1.Pod, id string, spec k8s.WorkloadSpec) corev1.Pod {
	t.Helper()
	if _, err := orch.Start(context.Background(), id, spec); err != nil {
		t.Fatalf("start %s: %v", spec.Type, err)
	}
	got := pods()
	if len(got) != 1 {
		t.Fatalf("pods = %d, want exactly the workload pod", len(got))
	}
	return got[0]
}

// assertSlotMount checks AC-N4/L5: the slot PVC is the pod's only claim and
// only the workload container mounts it.
func assertSlotMount(t *testing.T, pod corev1.Pod, claim string) {
	t.Helper()
	var claims []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
			if v.Name != k8s.AuthSlotVolumeName {
				t.Errorf("claim %s mounted as volume %q, want %q", v.PersistentVolumeClaim.ClaimName, v.Name, k8s.AuthSlotVolumeName)
			}
		}
	}
	if len(claims) != 1 || claims[0] != claim {
		t.Fatalf("pod claims = %v, want only %s", claims, claim)
	}
	for _, c := range pod.Spec.Containers {
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == k8s.AuthSlotVolumeName {
				mounted = m.MountPath == k8s.AuthSlotMountPath
			}
		}
		if c.Name == k8s.ContainerName && !mounted {
			t.Errorf("workload container does not mount the slot at %s", k8s.AuthSlotMountPath)
		}
		if c.Name != k8s.ContainerName && mounted {
			t.Errorf("container %s mounts the slot; only the workload container may", c.Name)
		}
	}
	agent := container(t, pod, k8s.ContainerName)
	if v, _ := envValue(agent, k8s.AuthSlotDirEnvVar); v != k8s.AuthSlotMountPath {
		t.Errorf("%s = %q", k8s.AuthSlotDirEnvVar, v)
	}
}

func TestCodexTask_PodShape(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeCodexTask, codexImage),
		k8s.WithCodexTaskCredentialsSecret("codex-task-k3s"),
		k8s.WithCodexTaskConfigConfigMap("codex-task-cfg"),
	)
	pod := onlyPod(t, orch, func() []corev1.Pod { return listPods(t, cs) }, "ct01", k8s.WorkloadSpec{
		Type: session.WorkloadTypeCodexTask, Model: session.PlatformDefaultModel, ReasoningEffort: "high", AuthSlot: slot(1),
	})
	if pod.Spec.AutomountServiceAccountToken == nil || !*pod.Spec.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want true: codex-task runs at claude-code's level (AC-M1)", pod.Spec.AutomountServiceAccountToken)
	}
	if n := len(pod.Spec.Containers); n != 2 {
		t.Fatalf("containers = %d, want agent + k3s-mcp-proxy", n)
	}
	assertSlotMount(t, pod, testNS+"-codex-task-auth-1")
	agent := container(t, pod, k8s.ContainerName)
	for name, want := range map[string]string{
		"DATA_PLANE_WORKLOAD":          "codex-task",
		k8s.CodexReasoningEffortEnvVar: "high",
		k8s.K3SMCPProxyURLEnvVar:       "http://127.0.0.1:8093",
	} {
		if got, _ := envValue(agent, name); got != want {
			t.Errorf("agent %s = %q, want %q", name, got, want)
		}
	}
	for _, e := range agent.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("agent env %s reads a Secret", e.Name)
		}
		if e.Name == k8s.CodexModelEnvVar && (e.ValueFrom == nil || e.ValueFrom.ConfigMapKeyRef == nil || e.ValueFrom.ConfigMapKeyRef.Name != "codex-task-cfg") {
			t.Errorf("platform-default model must come from codex-task-cfg, got %+v", e)
		}
	}
	proxy := container(t, pod, k8s.K3SMCPProxyContainerName)
	if got, _ := envValue(proxy, "PLUGIN_SEED_PLUGINS"); got != "" {
		t.Errorf("proxy plugins = %q, want none: a session that chose no plugins gets none (AC-A5)", got)
	}
	var token string
	for _, e := range proxy.Env {
		if e.Name == k8s.K3SMCPTokenEnvVar && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			token = e.ValueFrom.SecretKeyRef.Name
		}
	}
	if token != "codex-task-k3s" {
		t.Errorf("proxy token Secret = %q, want codex-task's own", token)
	}
}

func TestCodexTask_NeedsSlotAndValidEffort(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithWorkloadImage(session.WorkloadTypeCodexTask, codexImage))
	if _, err := orch.Start(context.Background(), "ct02", k8s.WorkloadSpec{Type: session.WorkloadTypeCodexTask}); err == nil {
		t.Fatal("codex-task started without an auth slot")
	}
	if _, err := orch.Start(context.Background(), "ct03", k8s.WorkloadSpec{Type: session.WorkloadTypeCodexTask, ReasoningEffort: "max", AuthSlot: slot(0)}); err == nil {
		t.Fatal("codex-task accepted claude's max effort")
	}
	if n := len(listPods(t, cs)); n != 0 {
		t.Fatalf("pods = %d after refused starts, want none", n)
	}
}

func TestClaudeCode_SubscriptionPodShape(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
		k8s.WithClaudeCodeAuthMode(k8s.ClaudeCodeAuthSubscription),
	)
	pod := onlyPod(t, orch, func() []corev1.Pod { return listPods(t, cs) }, "cc01", k8s.WorkloadSpec{
		Type: session.WorkloadTypeClaudeCode, Model: "opus", ReasoningEffort: "max",
	})
	if got := containerNames(pod); got != k8s.ContainerName+","+k8s.K3SMCPProxyContainerName {
		t.Fatalf("containers = %s, want the workload and K3s MCP proxy, no credential proxy (AC-E7)", got)
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("subscription pod mounts claim %s; the OAuth token replaced the slot (AC-E7)", v.PersistentVolumeClaim.ClaimName)
		}
	}
	agent := container(t, pod, k8s.ContainerName)
	if _, ok := envValue(agent, k8s.AuthSlotDirEnvVar); ok {
		t.Errorf("subscription agent receives %s", k8s.AuthSlotDirEnvVar)
	}
	for _, name := range []string{k8s.AnthropicBaseURLEnvVar, k8s.AnthropicAuthTokenEnvVar} {
		if _, ok := envValue(agent, name); ok {
			t.Errorf("subscription agent receives %s", name)
		}
	}
	for name, want := range map[string]string{
		k8s.ClaudeCodeAuthModeEnvVar: "subscription",
		k8s.ClaudeCodeEffortEnvVar:   "max",
		k8s.ClaudeCodeModelEnvVar:    "opus",
	} {
		if got, _ := envValue(agent, name); got != want {
			t.Errorf("agent %s = %q, want %q", name, got, want)
		}
	}
	var oauth *corev1.SecretKeySelector
	for _, e := range agent.Env {
		if e.Name == k8s.K3SMCPTokenEnvVar {
			t.Error("subscription agent receives the K3s MCP token; the sidecar holds it")
		}
		if e.Name == k8s.ClaudeCodeOAuthTokenEnvVar {
			if e.Value != "" || e.ValueFrom == nil {
				t.Fatalf("%s is not a Secret reference: %+v", k8s.ClaudeCodeOAuthTokenEnvVar, e)
			}
			oauth = e.ValueFrom.SecretKeyRef
		}
	}
	if oauth == nil || oauth.Name != "claude-code-credentials" || oauth.Key != k8s.ClaudeCodeOAuthTokenSecretKey ||
		(oauth.Optional != nil && *oauth.Optional) {
		t.Errorf("%s ref = %+v, want required key %q of claude-code-credentials", k8s.ClaudeCodeOAuthTokenEnvVar, oauth, k8s.ClaudeCodeOAuthTokenSecretKey)
	}
	if _, err := orch.Start(context.Background(), "cc02", k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode, AuthSlot: slot(0)}); err == nil {
		t.Fatal("subscription claude-code accepted an auth slot it would never use")
	}
}

func TestClaudeCode_GatewayPodUnchanged(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage))
	pod := onlyPod(t, orch, func() []corev1.Pod { return listPods(t, cs) }, "cc03", k8s.WorkloadSpec{
		Type: session.WorkloadTypeClaudeCode, ReasoningEffort: "low",
	})
	if n := len(pod.Spec.Containers); n != 3 {
		t.Fatalf("containers = %s, want agent + credential proxy + K3s MCP proxy (AC-E6)", containerNames(pod))
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("gateway pod mounts claim %s", v.PersistentVolumeClaim.ClaimName)
		}
	}
	agent := container(t, pod, k8s.ContainerName)
	if v, _ := envValue(agent, k8s.AnthropicBaseURLEnvVar); v == "" {
		t.Error("gateway agent lost its proxy base URL")
	}
	if _, ok := envValue(agent, k8s.ClaudeCodeAuthModeEnvVar); ok {
		t.Error("gateway agent receives the subscription marker")
	}
	for _, e := range agent.Env {
		if e.Name == k8s.ClaudeCodeOAuthTokenEnvVar {
			t.Error("gateway agent receives the subscription OAuth token")
		}
	}
	if v, _ := envValue(agent, k8s.ClaudeCodeEffortEnvVar); v != "low" {
		t.Errorf("effort = %q, want low", v)
	}
	if _, err := orch.Start(context.Background(), "cc04", k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode, AuthSlot: slot(0)}); err == nil {
		t.Fatal("gateway claude-code accepted an auth slot it would never use")
	}
}

func TestMiniMaxCode_RefusesEffort(t *testing.T) {
	orch, _ := newReadyOrchestrator(t, k8s.WithWorkloadImage(session.WorkloadTypeMiniMaxCode, miniMaxCodeImage))
	if _, err := orch.Start(context.Background(), "mm01", k8s.WorkloadSpec{Type: session.WorkloadTypeMiniMaxCode, ReasoningEffort: "high"}); err == nil {
		t.Fatal("minimax-code accepted a reasoning effort (AC-E8)")
	}
}

// A platform-default subscription session reads the subscription key, not the
// gateway's model id, which a Claude subscription would refuse (AC-E7).
func TestClaudeCode_SubscriptionPlatformDefaultReadsSubscriptionKey(t *testing.T) {
	for _, tc := range []struct {
		mode k8s.ClaudeCodeAuthMode
		slot *int
		key  string
	}{
		{k8s.ClaudeCodeAuthSubscription, nil, k8s.ClaudeCodeSubscriptionModelConfigKey},
		{k8s.ClaudeCodeAuthGateway, nil, k8s.ClaudeCodeModelConfigKey},
	} {
		orch, cs := newReadyOrchestrator(t,
			k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
			k8s.WithClaudeCodeAuthMode(tc.mode),
		)
		pod := onlyPod(t, orch, func() []corev1.Pod { return listPods(t, cs) }, "cd01", k8s.WorkloadSpec{
			Type: session.WorkloadTypeClaudeCode, Model: session.PlatformDefaultModel, AuthSlot: tc.slot,
		})
		agent := container(t, pod, k8s.ContainerName)
		var ref *corev1.ConfigMapKeySelector
		for _, e := range agent.Env {
			if e.Name == k8s.ClaudeCodeModelEnvVar && e.ValueFrom != nil {
				ref = e.ValueFrom.ConfigMapKeyRef
			}
		}
		if ref == nil || ref.Key != tc.key || ref.Optional == nil || !*ref.Optional {
			t.Errorf("%s: %s ref = %+v, want optional key %q", tc.mode, k8s.ClaudeCodeModelEnvVar, ref, tc.key)
		}
	}
}
