//go:build integration

// The approval gate's pod shapes (docs/prd/approval-gate.md AC-F2~F6), checked
// on minimax-code, today the only type behind the gate (session.HasApprovalGate).
// The type-specific half of that type's pods is minimax_code_orchestrator_test.go's.
// Which parts of the F series this file owns and which need the deployed SUT are
// in docs/test/e2e.md.
package integration_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const approvalGatewayURL = "http://pocket-aide.pocket-aide.svc.cluster.local:8080"

// gatedType is the type these tests provision. The contracts here are the
// gate's, so they hold for any type HasApprovalGate admits.
const gatedType = session.WorkloadTypeMiniMaxCode

// gatedTypes is every type behind the gate, for the suites (repo access, drop)
// whose contract is the helper pod's and so holds for each of them.
var gatedTypes = []session.WorkloadType{gatedType}

func newGatedSession(t *testing.T) podSet {
	t.Helper()
	_, set := startMiniMax(t, "f1f2")
	return set
}

type podSet struct {
	workload corev1.Pod
	helper   corev1.Pod
	started  k8s.SessionPods
}

func newPodSet(t *testing.T, pods []corev1.Pod, started k8s.SessionPods) podSet {
	t.Helper()
	set := podSet{started: started}
	var workloads, helpers int
	for _, p := range pods {
		switch p.Labels[k8s.LabelPodRole] {
		case k8s.PodRoleWorkload:
			set.workload, workloads = p, workloads+1
		case k8s.PodRoleHelper:
			set.helper, helpers = p, helpers+1
		default:
			t.Fatalf("pod %s has no %s label", p.Name, k8s.LabelPodRole)
		}
	}
	if workloads != 1 || helpers != 1 {
		t.Fatalf("pod roles = %d workload / %d helper, want 1 / 1 (AC-A2 + AC-F4)", workloads, helpers)
	}
	return set
}

func container(t *testing.T, pod corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("pod %s has no container %q (containers: %s)", pod.Name, name, containerNames(pod))
	return corev1.Container{}
}

func containerNames(pod corev1.Pod) string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}

// Exactly one of these two is meaningful per variable, which is what makes "is
// this a secret here?" a decidable question in these tests.
func envValue(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func secretKey(c corev1.Container, name string) (secret, key string, ok bool) {
	for _, e := range c.Env {
		if e.Name != name {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			return "", "", false
		}
		return e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key, true
	}
	return "", "", false
}

func configMapKey(c corev1.Container, name string) (configMap, key string, ok bool) {
	for _, e := range c.Env {
		if e.Name != name {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.ConfigMapKeyRef == nil {
			return "", "", false
		}
		return e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Key, true
	}
	return "", "", false
}

// AC-F4.
func TestApprovalGate_ProvisionsWorkloadAndHelperPod(t *testing.T) {
	set := newGatedSession(t)

	if n := len(set.started.Auxiliary); n != 1 {
		t.Fatalf("auxiliary pods = %d, want 1 (the session helper pod, AC-F4)", n)
	}
	if set.started.Auxiliary[0].Name != set.helper.Name {
		t.Fatalf("auxiliary ref = %q, want the helper pod %q", set.started.Auxiliary[0].Name, set.helper.Name)
	}
	if set.started.Workload.Name != set.workload.Name {
		t.Fatalf("workload ref = %q, want %q", set.started.Workload.Name, set.workload.Name)
	}
	for _, p := range []corev1.Pod{set.workload, set.helper} {
		if got := p.Labels[k8s.LabelSessionID]; got != "f1f2" {
			t.Errorf("pod %s: %s = %q, want the session id (AC-F4: the helper pod is session-exclusive)",
				p.Name, k8s.LabelSessionID, got)
		}
		if got := p.Labels[k8s.LabelWorkloadType]; got != string(gatedType) {
			t.Errorf("pod %s: %s = %q, want %q", p.Name, k8s.LabelWorkloadType, got, gatedType)
		}
		if len(p.Name) > 63 {
			t.Errorf("pod name %q is %d chars, over the 63-char DNS label limit", p.Name, len(p.Name))
		}
	}
	if set.workload.Name == set.helper.Name {
		t.Error("workload and helper pod share a name")
	}

	// The session MCP and the provider proxy are the gate's; anything beside
	// them is the type's own (minimax-code's K3s MCP proxy, AC-G5).
	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	proxy := container(t, set.helper, k8s.HelperCredentialProxyContainerName)
	if mcp.Image != miniMaxCodeImage || proxy.Image != miniMaxCodeImage {
		t.Errorf("helper images = %q/%q, want the configured type image %q", mcp.Image, proxy.Image, miniMaxCodeImage)
	}
	// The helper pod calls no Kubernetes API: its identity is the gateway
	// caller's, never the workload pod's view role, and no apiserver token is
	// mounted.
	if set.helper.Spec.ServiceAccountName != k8s.SessionMCPServiceAccountName {
		t.Errorf("helper service account = %q, want %q", set.helper.Spec.ServiceAccountName, k8s.SessionMCPServiceAccountName)
	}
	if set.helper.Spec.AutomountServiceAccountToken == nil || *set.helper.Spec.AutomountServiceAccountToken {
		t.Error("helper pod must not mount an apiserver service-account token")
	}
}

// AC-F6.
func TestApprovalGate_CredentialsAreSplitAcrossHelperContainers(t *testing.T) {
	set := newGatedSession(t)
	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	proxy := container(t, set.helper, k8s.HelperCredentialProxyContainerName)
	workload := container(t, set.workload, k8s.ContainerName)

	if got, _ := envValue(mcp, k8s.ApprovalGatewayURLEnvVar); got != approvalGatewayURL {
		t.Errorf("MCP container %s = %q, want the configured gateway %q", k8s.ApprovalGatewayURLEnvVar, got, approvalGatewayURL)
	}
	tokenFile, ok := envValue(mcp, k8s.ApprovalGatewayTokenFileEnvVar)
	if !ok || tokenFile == "" {
		t.Fatalf("MCP container is missing %s (AC-F6)", k8s.ApprovalGatewayTokenFileEnvVar)
	}
	for _, name := range []string{k8s.ApprovalGatewayURLEnvVar, k8s.ApprovalGatewayTokenFileEnvVar} {
		if _, ok := envValue(proxy, name); ok {
			t.Errorf("%s is also in the proxy container (AC-F6)", name)
		}
		if _, ok := envValue(workload, name); ok {
			t.Errorf("%s reached the workload pod; the agent must not be able to address the gateway (AC-F6)", name)
		}
	}
	assertGatewayTokenOnlyInMCP(t, set.helper, tokenFile)

	if _, key, ok := secretKey(proxy, k8s.AnthropicAuthTokenEnvVar); !ok || key != k8s.ClaudeCodeAuthTokenSecretKey {
		t.Errorf("proxy %s reads Secret key %q (present=%v), want %q (AC-E6 contract, AC-F6 placement)",
			k8s.AnthropicAuthTokenEnvVar, key, ok, k8s.ClaudeCodeAuthTokenSecretKey)
	}
	if _, key, ok := configMapKey(proxy, k8s.AnthropicBaseURLEnvVar); !ok || key != k8s.ClaudeCodeBaseURLConfigKey {
		t.Errorf("proxy %s reads ConfigMap key %q (present=%v), want %q (AC-E6 contract, AC-F6 placement)",
			k8s.AnthropicBaseURLEnvVar, key, ok, k8s.ClaudeCodeBaseURLConfigKey)
	}
	for _, name := range []string{k8s.AnthropicBaseURLEnvVar, k8s.AnthropicAuthTokenEnvVar} {
		if _, ok := envValue(mcp, name); ok {
			t.Errorf("%s is also projected into the MCP container; the provider token must not be there (AC-F6)", name)
		}
	}

	for _, e := range workload.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			t.Errorf("workload container reads Secret key %q through %s; AC-F6 injects no external credential here",
				e.ValueFrom.SecretKeyRef.Key, e.Name)
		}
		if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil && e.ValueFrom.ConfigMapKeyRef.Key != k8s.ClaudeCodeModelConfigKey {
			t.Errorf("workload container reads ConfigMap key %q through %s; only the optional model belongs here",
				e.ValueFrom.ConfigMapKeyRef.Key, e.Name)
		}
	}
	// AC-F6's ✅ 2026-09-03 decision: the K3s MCP token stays in the helper pod.
	if _, _, ok := secretKey(workload, k8s.K3SMCPTokenEnvVar); ok {
		t.Error("workload container gets the K3s MCP token; it belongs to the helper pod only (AC-F6, AC-G5)")
	}

	helperIP := set.started.Auxiliary[0].IP
	if helperIP == "" {
		t.Fatal("helper pod ref has no IP, so the workload pod cannot be pointed at it")
	}
	for _, want := range []struct{ env, contains string }{
		{k8s.AnthropicBaseURLEnvVar, helperIP},
		{k8s.SessionMCPURLEnvVar, helperIP},
	} {
		got, ok := envValue(workload, want.env)
		if !ok {
			t.Errorf("workload container is missing %s", want.env)
			continue
		}
		if !strings.Contains(got, want.contains) {
			t.Errorf("%s = %q, want it to address the session helper pod %q", want.env, got, want.contains)
		}
	}
	if token, ok := envValue(workload, k8s.AnthropicAuthTokenEnvVar); !ok || token == "" {
		t.Error("workload container is missing the non-secret proxy placeholder token")
	}

	addr, ok := envValue(proxy, "DATA_PLANE_AGENT_ADDR")
	if !ok || strings.HasPrefix(addr, "127.0.0.1") {
		t.Errorf("helper proxy bind address = %q, want a pod-network bind (AC-F6)", addr)
	}
}

// AC-F3's external request identifier needs the session half, and only the
// control plane can hand it to the container that talks to the gateway.
func TestApprovalGate_MCPContainerKnowsItsSession(t *testing.T) {
	set := newGatedSession(t)
	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	proxy := container(t, set.helper, k8s.HelperCredentialProxyContainerName)

	sessionID := set.helper.Labels[k8s.LabelSessionID]
	if sessionID == "" {
		t.Fatal("helper pod has no session label to compare the injected id against")
	}
	got, ok := envValue(mcp, k8s.SessionIDEnvVar)
	if !ok {
		t.Fatalf("MCP container is missing %s; AC-F3 cannot build {sessionID}:{requestID} without it", k8s.SessionIDEnvVar)
	}
	if got != sessionID {
		t.Errorf("%s = %q, want the session this helper pod serves (%q)", k8s.SessionIDEnvVar, got, sessionID)
	}
	if _, _, ok := secretKey(mcp, k8s.SessionIDEnvVar); ok {
		t.Errorf("%s is projected from a Secret; the session id is not a credential", k8s.SessionIDEnvVar)
	}
	if _, ok := envValue(proxy, k8s.SessionIDEnvVar); ok {
		t.Errorf("%s reached the proxy container, which never addresses the gateway (AC-F6)", k8s.SessionIDEnvVar)
	}
}

func TestApprovalGate_WorkloadPodHasNoCredentialSidecar(t *testing.T) {
	set := newGatedSession(t)
	if n := len(set.workload.Spec.Containers); n != 1 {
		t.Fatalf("workload containers = %d (%s), want 1 — the proxy belongs to the helper pod (AC-F2/F4)",
			n, containerNames(set.workload))
	}
	if set.workload.Spec.Containers[0].Name != k8s.ContainerName {
		t.Errorf("workload container = %q, want %q", set.workload.Spec.Containers[0].Name, k8s.ContainerName)
	}
}

// An unconfigured image makes a pod spec fail to build, which is the cheapest
// way to reach the failure paths AC-A3 hygiene covers.
func TestApprovalGate_HelperPodIsReclaimedWhenTheWorkloadPodFails(t *testing.T) {
	// No WithWorkloadImage for the type: the helper pod spec fails first, so
	// nothing is created at all.
	orch, cs := newReadyOrchestrator(t)
	if _, err := orch.Start(context.Background(), "f4f4",
		k8s.WorkloadSpec{Type: gatedType}); err == nil {
		t.Fatal("start with no configured image: want an error, got nil")
	}
	if pods := listPods(t, cs); len(pods) != 0 {
		t.Fatalf("pods after a refused start = %d, want 0", len(pods))
	}

	// Now the workload pod fails *after* the helper pod is Ready: an invalid
	// model is rejected when its spec is built.
	orch, cs = newMiniMaxOrchestrator(t)
	_, err := orch.Start(context.Background(), "f5f5", k8s.WorkloadSpec{
		Type:  gatedType,
		Model: "not a valid model name",
	})
	if err == nil {
		t.Fatal("start with an invalid model: want an error, got nil")
	}
	if pods := listPods(t, cs); len(pods) != 0 {
		t.Fatalf("pods left behind after the workload pod failed = %d, want 0 (AC-A3)", len(pods))
	}
}

// AC-B2 for a gated type, with AC-F4's helper pod.
func TestApprovalGate_RestoreProvisionsAFreshPair(t *testing.T) {
	orch, cs := newMiniMaxOrchestrator(t)
	restored, err := orch.RestoreInto(context.Background(), "f6f6", "s3://bucket/archive.tar",
		k8s.WorkloadSpec{Type: gatedType})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	set := newPodSet(t, listPods(t, cs), restored)
	if set.workload.Name == "sess-f6f6" {
		t.Error("restore reused the deterministic pod name; it must provision a new pod")
	}
	if len(set.started.Auxiliary) != 1 {
		t.Fatalf("restored auxiliary pods = %d, want 1 (AC-F4: a new helper pod comes up with it)",
			len(set.started.Auxiliary))
	}
	if got := set.workload.Annotations[k8s.AnnotationRestoreArchive]; got != "s3://bucket/archive.tar" {
		t.Errorf("workload restore annotation = %q, want the archive ref", got)
	}
	if len(set.helper.Annotations) != 0 {
		t.Errorf("helper pod carries restore annotations %v; it holds no state to restore (AC-F4)", set.helper.Annotations)
	}
	if len(set.workload.Name) > 63 || len(set.helper.Name) > 63 {
		t.Errorf("restore pod names exceed the DNS label limit: %q (%d), %q (%d)",
			set.workload.Name, len(set.workload.Name), set.helper.Name, len(set.helper.Name))
	}
	wSuffix := strings.TrimPrefix(set.workload.Name, "sess-f6f6-r")
	hSuffix := strings.TrimPrefix(set.helper.Name, "sess-f6f6-h")
	if wSuffix == set.workload.Name || hSuffix == set.helper.Name || wSuffix != hSuffix {
		t.Errorf("restore pods are not one round: %q / %q", set.workload.Name, set.helper.Name)
	}
}

// The types outside the gate keep their own pod shapes.
func TestApprovalGate_UngatedTypesUnchanged(t *testing.T) {
	const claudeImage = "ghcr.io/dlddu/session-platform-data-plane:claude"
	for _, tc := range []struct {
		workload   session.WorkloadType
		containers []string
	}{
		{session.WorkloadTypeShell, []string{k8s.ContainerName}},
		{session.WorkloadTypeClaudeCode, []string{k8s.ContainerName, k8s.ClaudeCredentialsContainerName, k8s.K3SMCPProxyContainerName}},
	} {
		t.Run(string(tc.workload), func(t *testing.T) {
			orch, cs := newReadyOrchestrator(t,
				k8s.WithImage("ghcr.io/dlddu/session-platform-data-plane:shell"),
				k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeImage))
			started, err := orch.Start(context.Background(), "e1e1", k8s.WorkloadSpec{Type: tc.workload})
			if err != nil {
				t.Fatalf("start %s: %v", tc.workload, err)
			}
			if n := len(started.Auxiliary); n != 0 {
				t.Fatalf("%s auxiliary pods = %d, want 0", tc.workload, n)
			}
			pods := listPods(t, cs)
			if len(pods) != 1 {
				t.Fatalf("%s pods = %d, want 1", tc.workload, len(pods))
			}
			if got := containerNames(pods[0]); got != strings.Join(tc.containers, ",") {
				t.Fatalf("%s containers = %q, want %q", tc.workload, got, strings.Join(tc.containers, ","))
			}
			if tc.workload != session.WorkloadTypeClaudeCode {
				return
			}
			proxy := container(t, pods[0], k8s.ClaudeCredentialsContainerName)
			addr, ok := envValue(proxy, "DATA_PLANE_AGENT_ADDR")
			if !ok || !strings.HasPrefix(addr, "127.0.0.1") {
				t.Errorf("claude-code proxy bind = %q, want loopback (AC-E6 keeps the sidecar placement)", addr)
			}
			// The data plane refuses a bind its declared placement does not
			// sanction, so opening the pod-network bind for the helper cannot
			// reach this one by accident.
			if placement, ok := envValue(proxy, k8s.CredentialProxyPlacementEnvVar); !ok || placement != "sidecar" {
				t.Errorf("claude-code proxy placement = %q, want sidecar", placement)
			}
			if _, _, ok := secretKey(container(t, pods[0], k8s.K3SMCPProxyContainerName), k8s.K3SMCPTokenEnvVar); !ok {
				t.Error("claude-code lost its K3s MCP token projection")
			}
			if _, _, ok := secretKey(container(t, pods[0], k8s.ContainerName), k8s.K3SMCPTokenEnvVar); ok {
				t.Error("claude-code's CLI container holds the K3s MCP token; the sidecar must")
			}
		})
	}
}

func TestApprovalGateHelperContainersAreStartableUnderTheirOwnBoundary(t *testing.T) {
	pods := newGatedSession(t)

	proxy := container(t, pods.helper, k8s.HelperCredentialProxyContainerName)
	if placement, ok := envValue(proxy, k8s.CredentialProxyPlacementEnvVar); !ok || placement != "helper" {
		t.Errorf("helper proxy placement = %q, want helper (AC-F6 moves it off loopback)", placement)
	}
	addr, ok := envValue(proxy, "DATA_PLANE_AGENT_ADDR")
	if !ok || strings.HasPrefix(addr, "127.0.0.1") {
		t.Errorf("helper proxy bind = %q, want the pod network — its client is a pod away", addr)
	}

	mcp := container(t, pods.helper, k8s.SessionMCPContainerName)
	if mcp.ReadinessProbe == nil || mcp.ReadinessProbe.Exec == nil {
		t.Fatalf("session MCP readiness probe = %+v, want an exec probe", mcp.ReadinessProbe)
	}
	if mcp.ReadinessProbe.HTTPGet != nil {
		// An HTTP probe is dialled by the kubelet from the node, which is
		// exactly the caller AC-F2's ingress policy is written to reject.
		t.Error("session MCP uses an HTTP readiness probe, which its own ingress policy would block")
	}
	if got := strings.Join(mcp.ReadinessProbe.Exec.Command, " "); !strings.Contains(got, "8092") {
		t.Errorf("session MCP probe = %q, want it to check the MCP port", got)
	}
}

// The caller token is a projected ServiceAccount token for the gateway's
// audience, mounted into the MCP container and nowhere else in the pod.
func assertGatewayTokenOnlyInMCP(t *testing.T, helper corev1.Pod, tokenFile string) {
	t.Helper()
	volumeName := ""
	for _, v := range helper.Spec.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			if src.ServiceAccountToken == nil {
				continue
			}
			volumeName = v.Name
			if src.ServiceAccountToken.Audience != k8s.ApprovalGatewayTokenAudience {
				t.Errorf("projected token audience = %q, want %q", src.ServiceAccountToken.Audience, k8s.ApprovalGatewayTokenAudience)
			}
			if !strings.HasSuffix(tokenFile, "/"+src.ServiceAccountToken.Path) {
				t.Errorf("token file %q does not end in the projected path %q", tokenFile, src.ServiceAccountToken.Path)
			}
		}
	}
	if volumeName == "" {
		t.Fatal("helper pod projects no service-account token for the gateway")
	}
	for _, c := range helper.Spec.Containers {
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name != volumeName {
				continue
			}
			mounted = true
			if !m.ReadOnly {
				t.Errorf("%s mounts the gateway token read-write", c.Name)
			}
			if !strings.HasPrefix(tokenFile, m.MountPath+"/") {
				t.Errorf("token file %q is outside the mount %q", tokenFile, m.MountPath)
			}
		}
		if mounted != (c.Name == k8s.SessionMCPContainerName) {
			t.Errorf("container %s: gateway token mounted = %v", c.Name, mounted)
		}
	}
}
