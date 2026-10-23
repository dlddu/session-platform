//go:build integration

// AC-G1/AC-G3/AC-G4 on the real client-go orchestrator: the submitted pod spec
// of a minimax-code session. Same fake clientset and helpers as
// workload_type_orchestrator_test.go.
package integration_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const miniMaxCodeImage = "ghcr.io/dlddu/session-platform-minimax-code:dev"

func newMiniMaxOrchestrator(t *testing.T, opts ...k8s.Option) (*k8s.ClientOrchestrator, func() []corev1.Pod) {
	t.Helper()
	base := []k8s.Option{
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
		k8s.WithWorkloadImage(session.WorkloadTypeMiniMaxCode, miniMaxCodeImage),
		k8s.WithClaudeCredentialsSecret("claude-provider"),
		k8s.WithMiniMaxCredentialsSecret("minimax-provider"),
	}
	orch, cs := newReadyOrchestrator(t, append(base, opts...)...)
	return orch, func() []corev1.Pod { return listPods(t, cs) }
}

// AC-G1/AC-G5: the type provisions one workload pod running the agent plus its
// two loopback sidecars (provider proxy, K3s MCP proxy), and nothing else — no
// helper pod.
func TestMiniMaxCode_PodSpec(t *testing.T) {
	orch, pods := newMiniMaxOrchestrator(t)
	set, err := orch.Start(context.Background(), "mm01", k8s.WorkloadSpec{Type: session.WorkloadTypeMiniMaxCode})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(set.Auxiliary) != 0 {
		t.Fatalf("auxiliary pods = %+v, want none", set.Auxiliary)
	}
	all := pods()
	if len(all) != 1 {
		t.Fatalf("pods = %d, want exactly the workload pod", len(all))
	}
	pod := all[0]
	if got := pod.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeMiniMaxCode) {
		t.Errorf("%s label = %q, want %q", k8s.LabelWorkloadType, got, session.WorkloadTypeMiniMaxCode)
	}
	if len(pod.Spec.Containers) != 3 {
		t.Fatalf("containers = %d, want agent + credential proxy + k3s mcp proxy", len(pod.Spec.Containers))
	}
	main, ok := containerNamed(pod, k8s.ContainerName)
	if !ok {
		t.Fatalf("missing main container %q", k8s.ContainerName)
	}
	if main.Image != miniMaxCodeImage {
		t.Errorf("image = %q, want %q", main.Image, miniMaxCodeImage)
	}
	if got, _ := envOf(main, "DATA_PLANE_WORKLOAD"); got != string(session.WorkloadTypeMiniMaxCode) {
		t.Errorf("DATA_PLANE_WORKLOAD = %q, want %q", got, session.WorkloadTypeMiniMaxCode)
	}
	stateDir, ok := envVarOf(main, "CLAUDE_CODE_STATE_DIR")
	if !ok || stateDir.Value != "/session/state" {
		t.Errorf("state dir = %+v (present=%v), want /session/state", stateDir, ok)
	}
	if len(main.VolumeMounts) != 1 || len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].EmptyDir == nil ||
		pod.Spec.Volumes[0].Name != main.VolumeMounts[0].Name {
		t.Errorf("state volume = %+v / %+v, want one matching emptyDir", main.VolumeMounts, pod.Spec.Volumes)
	}
}

// AC-G3: the CLI container holds no provider credential. It sees the loopback
// proxy and the placeholder, and the only Secret it reads is the optional
// model of its own type's Secret. No claude-code bootstrap material leaks in.
func TestMiniMaxCode_MainContainerHoldsNoCredential(t *testing.T) {
	orch, pods := newMiniMaxOrchestrator(t)
	if _, err := orch.Start(context.Background(), "mm02", k8s.WorkloadSpec{Type: session.WorkloadTypeMiniMaxCode}); err != nil {
		t.Fatalf("start: %v", err)
	}
	main, _ := containerNamed(pods()[0], k8s.ContainerName)
	for _, env := range main.Env {
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := env.ValueFrom.SecretKeyRef
		isOptionalModel := env.Name == k8s.MiniMaxCodeModelEnvVar && ref.Name == "minimax-provider" &&
			ref.Key == k8s.ClaudeCodeModelSecretKey && ref.Optional != nil && *ref.Optional
		if !isOptionalModel {
			t.Fatalf("main container has unexpected Secret env %s: %+v", env.Name, env)
		}
	}
	baseURL, _ := envVarOf(main, k8s.AnthropicBaseURLEnvVar)
	if baseURL.Value != k8s.ClaudeCredentialProxyURL || baseURL.ValueFrom != nil {
		t.Errorf("base URL = %+v, want literal %q", baseURL, k8s.ClaudeCredentialProxyURL)
	}
	token, _ := envVarOf(main, k8s.AnthropicAuthTokenEnvVar)
	if token.Value != "session-platform-proxy" || token.ValueFrom != nil {
		t.Errorf("auth token = %+v, want the non-secret placeholder", token)
	}
	for _, name := range []string{k8s.K3SMCPTokenEnvVar, k8s.ClaudeCodeModelEnvVar, k8s.AnthropicCACertEnvVar} {
		if _, ok := envVarOf(main, name); ok {
			t.Errorf("minimax-code main container must not receive %s", name)
		}
	}
}

// AC-G3: the sidecar is claude-code's proxy (same placement, probe and
// hardening) reading this type's Secret — never the claude-code one.
func TestMiniMaxCode_ProxyReadsItsOwnSecret(t *testing.T) {
	orch, pods := newMiniMaxOrchestrator(t)
	if _, err := orch.Start(context.Background(), "mm03", k8s.WorkloadSpec{Type: session.WorkloadTypeMiniMaxCode}); err != nil {
		t.Fatalf("start: %v", err)
	}
	proxy, ok := containerNamed(pods()[0], k8s.MiniMaxCredentialsContainerName)
	if !ok {
		t.Fatalf("missing credential container %q", k8s.MiniMaxCredentialsContainerName)
	}
	if got, _ := envOf(proxy, "DATA_PLANE_WORKLOAD"); got != "credential-proxy" {
		t.Errorf("proxy workload = %q, want credential-proxy", got)
	}
	if got, _ := envOf(proxy, "DATA_PLANE_AGENT_ADDR"); got != "127.0.0.1:8091" {
		t.Errorf("proxy listen address = %q, want loopback", got)
	}
	for _, tc := range []struct {
		env, key string
		optional bool
	}{
		{k8s.AnthropicBaseURLEnvVar, k8s.ClaudeCodeBaseURLSecretKey, false},
		{k8s.AnthropicAuthTokenEnvVar, k8s.ClaudeCodeAuthTokenSecretKey, false},
		{k8s.AnthropicCACertEnvVar, k8s.ClaudeCodeCACertSecretKey, true},
	} {
		env, ok := envVarOf(proxy, tc.env)
		if !ok || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("proxy env %s is not Secret-backed: %+v", tc.env, env)
		}
		ref := env.ValueFrom.SecretKeyRef
		optional := ref.Optional != nil && *ref.Optional
		if ref.Name != "minimax-provider" || ref.Key != tc.key || optional != tc.optional {
			t.Errorf("proxy env %s selector = %+v, want minimax-provider/%s optional=%v", tc.env, ref, tc.key, tc.optional)
		}
	}
	for _, c := range pods()[0].Spec.Containers {
		for _, env := range c.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == "claude-provider" {
				t.Errorf("container %s reads the claude-code Secret via %s", c.Name, env.Name)
			}
		}
	}
	if proxy.ReadinessProbe == nil || proxy.ReadinessProbe.Exec == nil {
		t.Error("loopback credential proxy needs an in-container readiness probe")
	}
	if s := proxy.SecurityContext; s == nil || s.ReadOnlyRootFilesystem == nil || !*s.ReadOnlyRootFilesystem {
		t.Errorf("credential proxy security context is not hardened: %+v", s)
	}
}

// AC-G5: the K3s MCP token lives only in its own loopback sidecar, read from
// this type's Secret as an optional key so a deployment without it still runs.
func TestMiniMaxCode_K3SMCPProxyHoldsTheToken(t *testing.T) {
	orch, pods := newMiniMaxOrchestrator(t)
	if _, err := orch.Start(context.Background(), "mm07", k8s.WorkloadSpec{Type: session.WorkloadTypeMiniMaxCode}); err != nil {
		t.Fatalf("start: %v", err)
	}
	pod := pods()[0]
	proxy, ok := containerNamed(pod, k8s.MiniMaxK3SMCPProxyContainerName)
	if !ok {
		t.Fatalf("missing container %q", k8s.MiniMaxK3SMCPProxyContainerName)
	}
	if got, _ := envOf(proxy, "DATA_PLANE_WORKLOAD"); got != "k3s-mcp-proxy" {
		t.Errorf("workload = %q, want k3s-mcp-proxy", got)
	}
	if got, _ := envOf(proxy, "DATA_PLANE_AGENT_ADDR"); got != "127.0.0.1:8093" {
		t.Errorf("listen address = %q, want loopback 8093", got)
	}
	token, ok := envVarOf(proxy, k8s.K3SMCPTokenEnvVar)
	if !ok || token.ValueFrom == nil || token.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s is not Secret-backed: %+v", k8s.K3SMCPTokenEnvVar, token)
	}
	ref := token.ValueFrom.SecretKeyRef
	if ref.Name != "minimax-provider" || ref.Key != "k3s-mcp-token" || ref.Optional == nil || !*ref.Optional {
		t.Errorf("token selector = %+v, want optional minimax-provider/k3s-mcp-token", ref)
	}
	for _, name := range []string{k8s.AnthropicBaseURLEnvVar, k8s.AnthropicAuthTokenEnvVar, k8s.AnthropicCACertEnvVar} {
		if _, ok := envVarOf(proxy, name); ok {
			t.Errorf("k3s mcp proxy must not receive the provider's %s", name)
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == k8s.MiniMaxK3SMCPProxyContainerName {
			continue
		}
		if _, ok := envVarOf(c, k8s.K3SMCPTokenEnvVar); ok {
			t.Errorf("container %s receives the K3s MCP token", c.Name)
		}
	}
	probe := proxy.ReadinessProbe
	if probe == nil || probe.Exec == nil || !strings.Contains(strings.Join(probe.Exec.Command, " "), "/127.0.0.1/8093") {
		t.Errorf("readiness probe = %+v, want an in-container check of 127.0.0.1:8093", probe)
	}
	if s := proxy.SecurityContext; s == nil || s.ReadOnlyRootFilesystem == nil || !*s.ReadOnlyRootFilesystem {
		t.Errorf("k3s mcp proxy security context is not hardened: %+v", s)
	}
}

// AC-G1 (model contract of AC-E6): an explicit model is a literal that wins
// over the Secret; the alias resolves from the Secret's optional key.
func TestMiniMaxCode_ModelEnv(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		wantLiteral string
	}{
		{name: "explicit", model: "MiniMax-M3", wantLiteral: "MiniMax-M3"},
		{name: "platform-default", model: session.PlatformDefaultModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, pods := newMiniMaxOrchestrator(t)
			if _, err := orch.Start(context.Background(), "mm04", k8s.WorkloadSpec{
				Type: session.WorkloadTypeMiniMaxCode, Model: tc.model,
			}); err != nil {
				t.Fatalf("start: %v", err)
			}
			main, _ := containerNamed(pods()[0], k8s.ContainerName)
			env, ok := envVarOf(main, k8s.MiniMaxCodeModelEnvVar)
			if !ok {
				t.Fatalf("missing %s", k8s.MiniMaxCodeModelEnvVar)
			}
			if tc.wantLiteral != "" {
				if env.Value != tc.wantLiteral || env.ValueFrom != nil {
					t.Fatalf("%s = %+v, want literal %q", k8s.MiniMaxCodeModelEnvVar, env, tc.wantLiteral)
				}
				return
			}
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil || env.ValueFrom.SecretKeyRef.Name != "minimax-provider" {
				t.Fatalf("%s = %+v, want the minimax Secret's optional model", k8s.MiniMaxCodeModelEnvVar, env)
			}
		})
	}
}

// AC-G1: same fail-loud rule as AC-E1 when the deployment has no image for it.
func TestMiniMaxCode_UnconfiguredImageIsRefused(t *testing.T) {
	orch, cs := newReadyOrchestrator(t, k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"))
	_, err := orch.Start(context.Background(), "mm05", k8s.WorkloadSpec{Type: session.WorkloadTypeMiniMaxCode})
	if err == nil || !strings.Contains(err.Error(), string(session.WorkloadTypeMiniMaxCode)) {
		t.Fatalf("start error = %v, want a refusal naming %s", err, session.WorkloadTypeMiniMaxCode)
	}
	if got := listPods(t, cs); len(got) != 0 {
		t.Errorf("created %d pods for a refused workload type, want 0", len(got))
	}
}

// AC-G4: a restore target is shaped like claude-code's — the filesystem archive
// annotation, never the CRIU one — and keeps the type and model.
func TestMiniMaxCode_RestoreTargetUsesArchiveAnnotation(t *testing.T) {
	orch, pods := newMiniMaxOrchestrator(t)
	set, err := orch.RestoreInto(context.Background(), "mm06", "s3://bucket/mm06.tar", k8s.WorkloadSpec{
		Type: session.WorkloadTypeMiniMaxCode, Model: "MiniMax-M3",
	})
	if err != nil {
		t.Fatalf("restore into: %v", err)
	}
	var restored *corev1.Pod
	all := pods()
	for i := range all {
		if all[i].Name == set.Workload.Name {
			restored = &all[i]
		}
	}
	if restored == nil {
		t.Fatalf("restore pod %q not found", set.Workload.Name)
	}
	if _, ok := restored.Annotations[k8s.AnnotationRestoreArchive]; !ok {
		t.Errorf("restore pod is missing %s", k8s.AnnotationRestoreArchive)
	}
	if _, ok := restored.Annotations[k8s.AnnotationRestoreCheckpoint]; ok {
		t.Errorf("restore pod unexpectedly carries CRIU annotation %s", k8s.AnnotationRestoreCheckpoint)
	}
	if got := restored.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeMiniMaxCode) {
		t.Errorf("%s label = %q, want %q", k8s.LabelWorkloadType, got, session.WorkloadTypeMiniMaxCode)
	}
	if got, _ := envOf(restored.Spec.Containers[0], k8s.MiniMaxCodeModelEnvVar); got != "MiniMax-M3" {
		t.Errorf("restore %s = %q, want MiniMax-M3", k8s.MiniMaxCodeModelEnvVar, got)
	}
}
