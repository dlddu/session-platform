//go:build integration

// approval-gated pod shapes, asserted against the same fake clientset the rest
// of client_orchestrator_test.go uses. AC-F6 states its verification in exactly
// these terms — "inspect the session pod spec" — so the credential split is
// fully checkable here; AC-F1's type contract is checked at the API layer
// (internal/api/workload_type_test.go). What a fake cannot show is the runtime
// behaviour of the two helper containers (AC-F3's approval gate) or that the
// egress policy actually blocks (AC-F2, which needs a policy-enforcing CNI —
// see docs/test/e2e.md). Neither is implemented in this slice.
package integration_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const approvalGatedImage = "ghcr.io/dlddu/session-platform-data-plane:approval-gated"

func newApprovalGatedOrchestrator(t *testing.T) (*k8s.ClientOrchestrator, podSet) {
	t.Helper()
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	started, err := orch.Start(context.Background(), "f1f2",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}
	return orch, newPodSet(t, listPods(t, cs), started)
}

// podSet is the pod pair one approval-gated session owns, resolved by role.
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

// envValue returns a literal env value; secretKey returns the Secret key an env
// var is projected from. Exactly one of them is meaningful per variable, which
// is what makes "is this a secret here?" a decidable question in these tests.
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

// AC-F1/AC-F4: an approval-gated session is provisioned as a workload pod *and*
// a session-scoped helper pod, both bound to the session and returned as one
// set so the lifecycle paths reclaim and restore them together.
func TestApprovalGated_ProvisionsWorkloadAndHelperPod(t *testing.T) {
	_, set := newApprovalGatedOrchestrator(t)

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
		if got := p.Labels[k8s.LabelWorkloadType]; got != string(session.WorkloadTypeApprovalGated) {
			t.Errorf("pod %s: %s = %q, want %q", p.Name, k8s.LabelWorkloadType, got, session.WorkloadTypeApprovalGated)
		}
		if len(p.Name) > 63 {
			t.Errorf("pod name %q is %d chars, over the 63-char DNS label limit", p.Name, len(p.Name))
		}
	}
	if set.workload.Name == set.helper.Name {
		t.Error("workload and helper pod share a name")
	}

	// AC-F4: two containers, the session MCP and the provider proxy.
	if n := len(set.helper.Spec.Containers); n != 2 {
		t.Fatalf("helper containers = %d (%s), want 2", n, containerNames(set.helper))
	}
	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	proxy := container(t, set.helper, k8s.HelperCredentialProxyContainerName)
	if mcp.Image != approvalGatedImage || proxy.Image != approvalGatedImage {
		t.Errorf("helper images = %q/%q, want the configured type image %q", mcp.Image, proxy.Image, approvalGatedImage)
	}
	// The helper pod holds both platform secrets and calls no Kubernetes API,
	// so it gets no cluster identity — unlike the workload pod (AC-A1's
	// read-only view role).
	if set.helper.Spec.ServiceAccountName != "" {
		t.Errorf("helper service account = %q, want none", set.helper.Spec.ServiceAccountName)
	}
	if set.helper.Spec.AutomountServiceAccountToken == nil || *set.helper.Spec.AutomountServiceAccountToken {
		t.Error("helper pod must not mount a service-account token")
	}
}

// AC-F6: the gateway triple reaches the MCP container only, the provider
// credentials reach the proxy container only, and the workload pod gets neither
// — just its helper pod's addresses and a non-secret placeholder.
func TestApprovalGated_CredentialsAreSplitAcrossHelperContainers(t *testing.T) {
	_, set := newApprovalGatedOrchestrator(t)
	mcp := container(t, set.helper, k8s.SessionMCPContainerName)
	proxy := container(t, set.helper, k8s.HelperCredentialProxyContainerName)
	workload := container(t, set.workload, k8s.ContainerName)

	gateway := []struct{ env, key string }{
		{k8s.ApprovalGatewayURLEnvVar, k8s.ApprovalGatewayURLSecretKey},
		{k8s.ApprovalGatewayAPIKeyEnvVar, k8s.ApprovalGatewayAPIKeySecretKey},
		{k8s.ApprovalGatewayUserIDEnvVar, k8s.ApprovalGatewayUserIDSecretKey},
	}
	for _, g := range gateway {
		secret, key, ok := secretKey(mcp, g.env)
		if !ok {
			t.Errorf("MCP container is missing Secret-backed %s (AC-F6)", g.env)
			continue
		}
		if key != g.key {
			t.Errorf("%s reads Secret key %q, want %q", g.env, key, g.key)
		}
		if secret == "" {
			t.Errorf("%s reads from an unnamed Secret", g.env)
		}
		if _, _, ok := secretKey(proxy, g.env); ok {
			t.Errorf("%s is also projected into the proxy container; the gateway key must not be there (AC-F6)", g.env)
		}
		if _, ok := envValue(workload, g.env); ok {
			t.Errorf("%s reached the workload pod; the agent must not be able to address the gateway (AC-F6)", g.env)
		}
	}

	for _, provider := range []struct{ env, key string }{
		{k8s.AnthropicBaseURLEnvVar, k8s.ClaudeCodeBaseURLSecretKey},
		{k8s.AnthropicAuthTokenEnvVar, k8s.ClaudeCodeAuthTokenSecretKey},
	} {
		_, key, ok := secretKey(proxy, provider.env)
		if !ok {
			t.Errorf("proxy container is missing Secret-backed %s (AC-E6 contract, AC-F6 placement)", provider.env)
			continue
		}
		if key != provider.key {
			t.Errorf("%s reads Secret key %q, want %q", provider.env, key, provider.key)
		}
		if _, _, ok := secretKey(mcp, provider.env); ok {
			t.Errorf("%s is also projected into the MCP container; the provider token must not be there (AC-F6)", provider.env)
		}
	}

	// The workload container must carry no Secret projection at all.
	for _, e := range workload.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Key != k8s.ClaudeCodeModelSecretKey {
			t.Errorf("workload container reads Secret key %q through %s; AC-F6 injects no external credential here",
				e.ValueFrom.SecretKeyRef.Key, e.Name)
		}
	}
	// AC-F6's 2026-09-03 decision: no runtime plugin bootstrap for this type, so
	// no K3s MCP token either.
	if _, _, ok := secretKey(workload, k8s.K3SMCPTokenEnvVar); ok {
		t.Error("workload container gets the K3s MCP token; approval-gated has no plugin bootstrap (AC-F6)")
	}

	// What it does get: its own helper pod's addresses and the placeholder.
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

	// The proxy binds the pod network here, not loopback: its client is a pod
	// away (AC-F6). Its claude-code sidecar placement keeps loopback — asserted
	// by TestApprovalGated_ExistingTypesUnchanged below.
	addr, ok := envValue(proxy, "DATA_PLANE_AGENT_ADDR")
	if !ok || strings.HasPrefix(addr, "127.0.0.1") {
		t.Errorf("helper proxy bind address = %q, want a pod-network bind (AC-F6)", addr)
	}
}

// The workload pod of an approval-gated session must not carry the credential
// proxy sidecar: moving it out is what lets AC-F2 leave no external destination
// on that pod's egress allowlist.
func TestApprovalGated_WorkloadPodHasNoCredentialSidecar(t *testing.T) {
	_, set := newApprovalGatedOrchestrator(t)
	if n := len(set.workload.Spec.Containers); n != 1 {
		t.Fatalf("workload containers = %d (%s), want 1 — the proxy belongs to the helper pod (AC-F2/F4)",
			n, containerNames(set.workload))
	}
	if set.workload.Spec.Containers[0].Name != k8s.ContainerName {
		t.Errorf("workload container = %q, want %q", set.workload.Spec.Containers[0].Name, k8s.ContainerName)
	}
}

// A failure after the helper pod is up must not leave it behind (AC-A3
// hygiene): an unconfigured image makes the workload pod spec fail to build,
// which is the cheapest way to reach that path.
func TestApprovalGated_HelperPodIsReclaimedWhenTheWorkloadPodFails(t *testing.T) {
	// No WithWorkloadImage for the type: the helper pod spec fails first, so
	// nothing is created at all.
	orch, cs := newReadyOrchestrator(t)
	if _, err := orch.Start(context.Background(), "f4f4",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated}); err == nil {
		t.Fatal("start with no configured image: want an error, got nil")
	}
	if pods := listPods(t, cs); len(pods) != 0 {
		t.Fatalf("pods after a refused start = %d, want 0", len(pods))
	}

	// Now the workload pod fails *after* the helper pod is Ready: an invalid
	// model is rejected when its spec is built.
	orch, cs = newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	_, err := orch.Start(context.Background(), "f5f5", k8s.WorkloadSpec{
		Type:  session.WorkloadTypeApprovalGated,
		Model: "not a valid model name",
	})
	if err == nil {
		t.Fatal("start with an invalid model: want an error, got nil")
	}
	if pods := listPods(t, cs); len(pods) != 0 {
		t.Fatalf("pods left behind after the workload pod failed = %d, want 0 (AC-A3)", len(pods))
	}
}

// AC-B2 for this type: a restore provisions a *new* pair under fresh names that
// share one round suffix, and the workload pod is pointed at the helper pod
// created for that restore (AC-F4).
func TestApprovalGated_RestoreProvisionsAFreshPair(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	restored, err := orch.RestoreInto(context.Background(), "f6f6", "s3://bucket/archive.tar",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
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
	// Same provisioning round: the names differ only in the role letter.
	wSuffix := strings.TrimPrefix(set.workload.Name, "sess-f6f6-r")
	hSuffix := strings.TrimPrefix(set.helper.Name, "sess-f6f6-h")
	if wSuffix == set.workload.Name || hSuffix == set.helper.Name || wSuffix != hSuffix {
		t.Errorf("restore pods are not one round: %q / %q", set.workload.Name, set.helper.Name)
	}
}

// Regression guard for the two existing types: adding the third one must not
// change the pods they get. Their contract is one pod, and for claude-code a
// loopback-bound credential sidecar inside it.
func TestApprovalGated_ExistingTypesUnchanged(t *testing.T) {
	const claudeImage = "ghcr.io/dlddu/session-platform-data-plane:claude"
	for _, tc := range []struct {
		workload   session.WorkloadType
		containers []string
	}{
		{session.WorkloadTypeShell, []string{k8s.ContainerName}},
		{session.WorkloadTypeClaudeCode, []string{k8s.ContainerName, k8s.ClaudeCredentialsContainerName}},
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
			if _, _, ok := secretKey(container(t, pods[0], k8s.ContainerName), k8s.K3SMCPTokenEnvVar); !ok {
				t.Error("claude-code lost its K3s MCP token projection")
			}
		})
	}
}

// The helper pod's two containers must be startable by the data plane as
// configured — the type's whole failure mode until now was a pod spec whose
// containers the agent refused. Both halves are asserted here: the proxy's
// declared placement (which is what lets it bind the pod network at all) and a
// readiness probe that AC-F2's ingress policy cannot lock out.
func TestApprovalGatedHelperContainersAreStartableUnderTheirOwnBoundary(t *testing.T) {
	_, pods := newApprovalGatedOrchestrator(t)

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
