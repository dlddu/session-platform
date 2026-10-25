//go:build integration

// AC-G1/AC-G3~G6 on the real client-go orchestrator: the submitted pod specs
// and network policies of a minimax-code session. Same fake clientset and
// helpers as workload_type_orchestrator_test.go and the approval-gated suite.
package integration_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const miniMaxCodeImage = "ghcr.io/dlddu/session-platform-minimax-code:dev"

func newMiniMaxOrchestrator(t *testing.T, opts ...k8s.Option) (*k8s.ClientOrchestrator, *fake.Clientset) {
	t.Helper()
	base := []k8s.Option{
		k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:dev"),
		k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage),
		k8s.WithWorkloadImage(session.WorkloadTypeMiniMaxCode, miniMaxCodeImage),
		k8s.WithClaudeCredentialsSecret("claude-provider"),
		k8s.WithMiniMaxCredentialsSecret("minimax-provider"),
		k8s.WithApprovalGatewayURL(approvalGatewayURL),
	}
	return newReadyOrchestrator(t, append(base, opts...)...)
}

func startMiniMax(t *testing.T, sessionID string, opts ...k8s.Option) (*fake.Clientset, podSet) {
	t.Helper()
	orch, cs := newMiniMaxOrchestrator(t, opts...)
	started, err := orch.Start(context.Background(), sessionID, k8s.WorkloadSpec{Type: session.WorkloadTypeMiniMaxCode})
	if err != nil {
		t.Fatalf("start minimax-code session: %v", err)
	}
	return cs, newPodSet(t, listPods(t, cs), started)
}

// AC-G1/AC-G6: a workload pod running only the agent, plus the session's
// helper pod with the session MCP and both proxies.
func TestMiniMaxCode_ProvisionsWorkloadAndHelperPod(t *testing.T) {
	_, set := startMiniMax(t, "mm01")
	if n := len(set.started.Auxiliary); n != 1 || set.started.Auxiliary[0].Name != set.helper.Name {
		t.Fatalf("auxiliary pods = %+v, want the helper pod %q", set.started.Auxiliary, set.helper.Name)
	}
	for _, p := range []corev1.Pod{set.workload, set.helper} {
		if got := p.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeMiniMaxCode) {
			t.Errorf("pod %s: %s = %q, want %q", p.Name, k8s.LabelWorkloadType, got, session.WorkloadTypeMiniMaxCode)
		}
	}
	if n := len(set.workload.Spec.Containers); n != 1 {
		t.Fatalf("workload containers = %d (%s), want the agent alone — no sidecar may hold a credential or egress",
			n, containerNames(set.workload))
	}
	main := container(t, set.workload, k8s.ContainerName)
	if main.Image != miniMaxCodeImage {
		t.Errorf("image = %q, want %q", main.Image, miniMaxCodeImage)
	}
	if got, _ := envOf(main, "DATA_PLANE_WORKLOAD"); got != string(session.WorkloadTypeMiniMaxCode) {
		t.Errorf("DATA_PLANE_WORKLOAD = %q, want %q", got, session.WorkloadTypeMiniMaxCode)
	}
	if stateDir, ok := envVarOf(main, "CLAUDE_CODE_STATE_DIR"); !ok || stateDir.Value != "/session/state" {
		t.Errorf("state dir = %+v (present=%v), want /session/state", stateDir, ok)
	}
	if len(main.VolumeMounts) != 1 || len(set.workload.Spec.Volumes) != 1 || set.workload.Spec.Volumes[0].EmptyDir == nil {
		t.Errorf("state volume = %+v / %+v, want one emptyDir", main.VolumeMounts, set.workload.Spec.Volumes)
	}

	if n := len(set.helper.Spec.Containers); n != 3 {
		t.Fatalf("helper containers = %d (%s), want session MCP + credential proxy + k3s mcp proxy",
			n, containerNames(set.helper))
	}
	for _, name := range []string{k8s.SessionMCPContainerName, k8s.HelperCredentialProxyContainerName, k8s.MiniMaxK3SMCPProxyContainerName} {
		if c := container(t, set.helper, name); c.Image != miniMaxCodeImage {
			t.Errorf("helper container %s image = %q, want %q", name, c.Image, miniMaxCodeImage)
		}
	}
	if set.helper.Spec.AutomountServiceAccountToken == nil || *set.helper.Spec.AutomountServiceAccountToken {
		t.Error("helper pod must not mount a service-account token")
	}
}

// AC-G3/AC-G6: the CLI container holds no credential. It knows its helper's
// three addresses and the placeholder, and the only Secret it reads is the
// optional model of its own type's Secret.
func TestMiniMaxCode_MainContainerHoldsNoCredential(t *testing.T) {
	_, set := startMiniMax(t, "mm02")
	main := container(t, set.workload, k8s.ContainerName)
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
	for name, port := range map[string]string{
		k8s.AnthropicBaseURLEnvVar: "8091",
		k8s.SessionMCPURLEnvVar:    "8092",
		k8s.K3SMCPProxyURLEnvVar:   "8093",
	} {
		want := "http://" + testPodIP + ":" + port
		if got, ok := envValue(main, name); !ok || got != want {
			t.Errorf("%s = %q (present=%v), want the helper address %q", name, got, ok, want)
		}
	}
	if got, _ := envValue(main, k8s.AnthropicAuthTokenEnvVar); got != "session-platform-proxy" {
		t.Errorf("auth token = %q, want the non-secret placeholder", got)
	}
	for _, name := range []string{k8s.K3SMCPTokenEnvVar, k8s.ClaudeCodeModelEnvVar, k8s.AnthropicCACertEnvVar} {
		if _, ok := envVarOf(main, name); ok {
			t.Errorf("minimax-code main container must not receive %s", name)
		}
	}
}

// AC-G3/AC-G6: the provider proxy is approval-gated's helper proxy (same
// placement, probe and hardening) reading this type's Secret — never the
// claude-code one, in any container of either pod.
func TestMiniMaxCode_ProviderProxyReadsItsOwnSecret(t *testing.T) {
	_, set := startMiniMax(t, "mm03")
	proxy := container(t, set.helper, k8s.HelperCredentialProxyContainerName)
	if got, _ := envOf(proxy, "DATA_PLANE_WORKLOAD"); got != "credential-proxy" {
		t.Errorf("proxy workload = %q, want credential-proxy", got)
	}
	if got, _ := envValue(proxy, k8s.CredentialProxyPlacementEnvVar); got != "helper" {
		t.Errorf("proxy placement = %q, want helper", got)
	}
	if addr, _ := envOf(proxy, "DATA_PLANE_AGENT_ADDR"); strings.HasPrefix(addr, "127.") {
		t.Errorf("proxy bind = %q, want the pod network", addr)
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
	for _, pod := range []corev1.Pod{set.workload, set.helper} {
		for _, c := range pod.Spec.Containers {
			for _, env := range c.Env {
				if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == "claude-provider" {
					t.Errorf("container %s/%s reads the claude-code Secret via %s", pod.Name, c.Name, env.Name)
				}
			}
		}
	}
	if s := proxy.SecurityContext; s == nil || s.ReadOnlyRootFilesystem == nil || !*s.ReadOnlyRootFilesystem {
		t.Errorf("credential proxy security context is not hardened: %+v", s)
	}
}

// AC-G5: the K3s MCP token lives only in the helper pod's K3s MCP proxy, read
// from this type's Secret as an optional key so a deployment without it still
// runs.
func TestMiniMaxCode_K3SMCPProxyHoldsTheToken(t *testing.T) {
	_, set := startMiniMax(t, "mm07")
	proxy := container(t, set.helper, k8s.MiniMaxK3SMCPProxyContainerName)
	if got, _ := envOf(proxy, "DATA_PLANE_WORKLOAD"); got != "k3s-mcp-proxy" {
		t.Errorf("workload = %q, want k3s-mcp-proxy", got)
	}
	if got, _ := envOf(proxy, "DATA_PLANE_AGENT_ADDR"); got != ":8093" {
		t.Errorf("listen address = %q, want the pod network on 8093", got)
	}
	if got, _ := envValue(proxy, k8s.CredentialProxyPlacementEnvVar); got != "helper" {
		t.Errorf("placement = %q, want helper", got)
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
	for _, pod := range []corev1.Pod{set.workload, set.helper} {
		for _, c := range pod.Spec.Containers {
			if pod.Name == set.helper.Name && c.Name == k8s.MiniMaxK3SMCPProxyContainerName {
				continue
			}
			if _, ok := envVarOf(c, k8s.K3SMCPTokenEnvVar); ok {
				t.Errorf("container %s/%s receives the K3s MCP token", pod.Name, c.Name)
			}
		}
	}
	// AC-G5: the marketplace clone needs writable scratch under a read-only
	// root; only this container mounts it, and it is an emptyDir of the pod.
	dir, _ := envValue(proxy, k8s.MiniMaxMarketplaceDirEnvVar)
	if len(proxy.VolumeMounts) != 1 || proxy.VolumeMounts[0].MountPath != dir || dir == "" {
		t.Errorf("k3s mcp proxy mounts = %+v, %s = %q; want one scratch mount at that path", proxy.VolumeMounts, k8s.MiniMaxMarketplaceDirEnvVar, dir)
	} else {
		var scratch *corev1.Volume
		for i := range set.helper.Spec.Volumes {
			if set.helper.Spec.Volumes[i].Name == proxy.VolumeMounts[0].Name {
				scratch = &set.helper.Spec.Volumes[i]
			}
		}
		if scratch == nil || scratch.EmptyDir == nil {
			t.Errorf("marketplace scratch volume = %+v, want an emptyDir", scratch)
		}
	}
	for _, name := range []string{k8s.SessionMCPContainerName, k8s.HelperCredentialProxyContainerName} {
		for _, m := range container(t, set.helper, name).VolumeMounts {
			if m.MountPath == dir {
				t.Errorf("container %s mounts the marketplace scratch", name)
			}
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

// AC-G6: the session MCP is approval-gated's, gateway address and caller token
// included.
func TestMiniMaxCode_SessionMCPHoldsTheGateway(t *testing.T) {
	_, set := startMiniMax(t, "mm08")
	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	if got, _ := envValue(mcp, k8s.SessionIDEnvVar); got != "mm08" {
		t.Errorf("session MCP session id = %q, want mm08", got)
	}
	if set.helper.Spec.ServiceAccountName != k8s.SessionMCPServiceAccountName {
		t.Errorf("helper service account = %q, want %q", set.helper.Spec.ServiceAccountName, k8s.SessionMCPServiceAccountName)
	}
	tokenFile, _ := envValue(mcp, k8s.ApprovalGatewayTokenFileEnvVar)
	assertGatewayTokenOnlyInMCP(t, set.helper, tokenFile)
	for _, name := range []string{k8s.ApprovalGatewayURLEnvVar, k8s.ApprovalGatewayTokenFileEnvVar} {
		if v, ok := envValue(mcp, name); !ok || v == "" {
			t.Errorf("session MCP %s is unset", name)
		}
		for _, pod := range []corev1.Pod{set.workload, set.helper} {
			for _, c := range pod.Spec.Containers {
				if pod.Name == set.helper.Name && c.Name == k8s.SessionMCPContainerName {
					continue
				}
				if _, ok := envVarOf(c, name); ok {
					t.Errorf("container %s/%s receives %s", pod.Name, c.Name, name)
				}
			}
		}
	}
}

// AC-G6: approval-gated's boundary, widened by exactly the K3s MCP proxy port.
func TestMiniMaxCode_BoundaryAdmitsTheHelperPortsOnly(t *testing.T) {
	cs, set := startMiniMax(t, "mm09")
	policies := listNetworkPolicies(t, cs)
	if len(policies) != 2 {
		t.Fatalf("network policies = %v, want workload egress + helper ingress", policyNames(policies))
	}
	egress, ok := policies[set.helper.Name+"-workload-egress"]
	if !ok {
		t.Fatalf("no workload egress policy; have %v", policyNames(policies))
	}
	if len(egress.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want DNS + this session's helper pod", len(egress.Spec.Egress))
	}
	assertPorts(t, "workload egress to helper", egress.Spec.Egress[1].Ports, 8091, 8092, 8093)
	ingress, ok := policies[set.helper.Name+"-helper-ingress"]
	if !ok {
		t.Fatalf("no helper ingress policy; have %v", policyNames(policies))
	}
	assertPorts(t, "helper ingress", ingress.Spec.Ingress[0].Ports, 8091, 8092, 8093)
}

// AC-G6 (AC-F5): with a configured class the workload pod mounts the helper
// round's shared claim, as the session MCP does.
func TestMiniMaxCode_SharedVolumeIsMountedByWorkloadAndMCP(t *testing.T) {
	cs, set := startMiniMax(t, "mm10", withSharedVolume())
	claim := onlyClaim(t, cs)
	for _, tc := range []struct {
		pod       corev1.Pod
		container string
	}{
		{set.workload, k8s.ContainerName},
		{set.helper, k8s.SessionMCPContainerName},
	} {
		if path, ok := mountPath(t, tc.pod, tc.container, claim.Name); !ok || path != k8s.SharedVolumeMountPath {
			t.Errorf("%s/%s mount of %s = %q (present=%v), want %q",
				tc.pod.Name, tc.container, claim.Name, path, ok, k8s.SharedVolumeMountPath)
		}
	}
	if got, _ := envValue(container(t, set.workload, k8s.ContainerName), k8s.SharedVolumeDirEnvVar); got != k8s.SharedVolumeMountPath {
		t.Errorf("workload %s = %q, want %q", k8s.SharedVolumeDirEnvVar, got, k8s.SharedVolumeMountPath)
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
			orch, cs := newMiniMaxOrchestrator(t)
			started, err := orch.Start(context.Background(), "mm04", k8s.WorkloadSpec{
				Type: session.WorkloadTypeMiniMaxCode, Model: tc.model,
			})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			set := newPodSet(t, listPods(t, cs), started)
			main := container(t, set.workload, k8s.ContainerName)
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

// AC-G4/AC-G6: a restore builds a fresh pod pair. The workload half is shaped
// like claude-code's restore target — the filesystem archive annotation, never
// the CRIU one — and keeps the type and model.
func TestMiniMaxCode_RestoreTargetUsesArchiveAnnotation(t *testing.T) {
	orch, cs := newMiniMaxOrchestrator(t)
	set, err := orch.RestoreInto(context.Background(), "mm06", "s3://bucket/mm06.tar", k8s.WorkloadSpec{
		Type: session.WorkloadTypeMiniMaxCode, Model: "MiniMax-M3",
	})
	if err != nil {
		t.Fatalf("restore into: %v", err)
	}
	if len(set.Auxiliary) != 1 {
		t.Fatalf("restore auxiliary pods = %+v, want a fresh helper pod", set.Auxiliary)
	}
	pods := newPodSet(t, listPods(t, cs), set)
	restored := pods.workload
	if restored.Name != set.Workload.Name {
		t.Fatalf("restore pod = %q, want %q", restored.Name, set.Workload.Name)
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

// AC-G5: the K3s MCP proxy names the session it serves on every upstream call,
// so the control plane hands it the id as a plain value — on a fresh set and on
// a restore's fresh helper pod alike.
func TestMiniMaxCode_K3SMCPProxyKnowsItsSession(t *testing.T) {
	_, set := startMiniMax(t, "mm09")
	proxy := container(t, set.helper, k8s.MiniMaxK3SMCPProxyContainerName)
	if got, ok := envValue(proxy, k8s.SessionIDEnvVar); !ok || got != "mm09" {
		t.Errorf("k3s mcp proxy %s = %q (present=%v), want mm09", k8s.SessionIDEnvVar, got, ok)
	}
	if _, _, ok := secretKey(proxy, k8s.SessionIDEnvVar); ok {
		t.Errorf("%s is projected from a Secret; the session id is not a credential", k8s.SessionIDEnvVar)
	}
	if _, ok := envValue(container(t, set.helper, k8s.HelperCredentialProxyContainerName), k8s.SessionIDEnvVar); ok {
		t.Errorf("%s reached the provider proxy, which never names the session", k8s.SessionIDEnvVar)
	}

	orch, cs := newMiniMaxOrchestrator(t)
	restored, err := orch.RestoreInto(context.Background(), "mm10", "s3://bucket/mm10.tar", k8s.WorkloadSpec{
		Type: session.WorkloadTypeMiniMaxCode,
	})
	if err != nil {
		t.Fatalf("restore into: %v", err)
	}
	pods := newPodSet(t, listPods(t, cs), restored)
	proxy = container(t, pods.helper, k8s.MiniMaxK3SMCPProxyContainerName)
	if got, _ := envValue(proxy, k8s.SessionIDEnvVar); got != "mm10" {
		t.Errorf("restore helper %s k3s mcp proxy %s = %q, want mm10", pods.helper.Name, k8s.SessionIDEnvVar, got)
	}
}
