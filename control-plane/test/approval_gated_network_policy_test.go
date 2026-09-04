//go:build integration

// AC-F2 as far as a fake clientset can carry it: the two policy objects an
// approval-gated session gets, their selectors and ports, and the fact that
// they are reclaimed with the session rather than left behind. What is *not*
// here — and cannot be, on any fake or on kindnet — is whether a cluster
// enforces them. That half needs a policy-enforcing CNI and is recorded as
// open in docs/doc-tracker.md.
package integration_test

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func listNetworkPolicies(t *testing.T, cs *fake.Clientset) map[string]networkingv1.NetworkPolicy {
	t.Helper()
	list, err := cs.NetworkingV1().NetworkPolicies(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list network policies: %v", err)
	}
	byName := make(map[string]networkingv1.NetworkPolicy, len(list.Items))
	for _, policy := range list.Items {
		byName[policy.Name] = policy
	}
	return byName
}

// startApprovalGated brings up one approval-gated session and hands back its
// pods plus the clientset, so a test can look at what else was created.
func startApprovalGated(t *testing.T, sessionID string) (*k8s.ClientOrchestrator, *fake.Clientset, podSet) {
	t.Helper()
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	started, err := orch.Start(context.Background(), sessionID,
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}
	return orch, cs, newPodSet(t, listPods(t, cs), started)
}

func TestApprovalGatedSessionGetsEgressAllowlistAndHelperIngressPolicy(t *testing.T) {
	_, cs, pods := startApprovalGated(t, "f2f2")
	policies := listNetworkPolicies(t, cs)
	if len(policies) != 2 {
		t.Fatalf("network policies = %d, want 2 (workload egress + helper ingress)", len(policies))
	}

	egress, ok := policies[pods.helper.Name+"-workload-egress"]
	if !ok {
		t.Fatalf("no workload egress policy; have %v", policyNames(policies))
	}
	if got := egress.Spec.PodSelector.MatchLabels; got[k8s.LabelSessionID] != "f2f2" || got[k8s.LabelPodRole] != k8s.PodRoleWorkload {
		t.Fatalf("egress podSelector = %v, want this session's workload pod", got)
	}
	if len(egress.Spec.PolicyTypes) != 1 || egress.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("egress policyTypes = %v, want [Egress]", egress.Spec.PolicyTypes)
	}
	if len(egress.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2 (kube-dns, own helper pod)", len(egress.Spec.Egress))
	}

	dns := egress.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].NamespaceSelector == nil || dns.To[0].PodSelector == nil {
		t.Fatalf("dns rule peer = %+v, want a kube-system namespace + kube-dns pod selector", dns.To)
	}
	if got := dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "kube-system" {
		t.Fatalf("dns namespace selector = %q, want kube-system", got)
	}
	if len(dns.Ports) != 2 {
		t.Fatalf("dns ports = %d, want udp+tcp 53", len(dns.Ports))
	}

	helper := egress.Spec.Egress[1]
	if len(helper.To) != 1 || helper.To[0].PodSelector == nil {
		t.Fatalf("helper rule peer = %+v, want a pod selector", helper.To)
	}
	// No namespace selector: the rule must not reach out of this namespace, and
	// the session id in the selector is what keeps it off another session's
	// helper pod inside it (AC-F2).
	if helper.To[0].NamespaceSelector != nil {
		t.Fatal("helper egress rule carries a namespace selector; it must stay in this namespace")
	}
	if got := helper.To[0].PodSelector.MatchLabels; got[k8s.LabelSessionID] != "f2f2" || got[k8s.LabelPodRole] != k8s.PodRoleHelper {
		t.Fatalf("helper egress peer selector = %v, want this session's helper pod", got)
	}
	assertPorts(t, "helper egress", helper.Ports, 8091, 8092)

	ingress, ok := policies[pods.helper.Name+"-helper-ingress"]
	if !ok {
		t.Fatalf("no helper ingress policy; have %v", policyNames(policies))
	}
	if got := ingress.Spec.PodSelector.MatchLabels; got[k8s.LabelSessionID] != "f2f2" || got[k8s.LabelPodRole] != k8s.PodRoleHelper {
		t.Fatalf("ingress podSelector = %v, want this session's helper pod", got)
	}
	if len(ingress.Spec.PolicyTypes) != 1 || ingress.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("ingress policyTypes = %v, want [Ingress]", ingress.Spec.PolicyTypes)
	}
	if len(ingress.Spec.Ingress) != 1 || len(ingress.Spec.Ingress[0].From) != 1 ||
		ingress.Spec.Ingress[0].From[0].PodSelector == nil {
		t.Fatalf("ingress rules = %+v, want one workload-pod peer", ingress.Spec.Ingress)
	}
	if got := ingress.Spec.Ingress[0].From[0].PodSelector.MatchLabels; got[k8s.LabelSessionID] != "f2f2" ||
		got[k8s.LabelPodRole] != k8s.PodRoleWorkload {
		t.Fatalf("ingress peer selector = %v, want this session's workload pod", got)
	}
	assertPorts(t, "helper ingress", ingress.Spec.Ingress[0].Ports, 8091, 8092)
}

// The policies exist for the helper pod's sake, so they must not outlive it:
// freeze, delete and a failed provisioning rollback all reclaim the pod, and an
// owner reference is what makes every one of those paths reclaim the policies
// too without the reclaim code having to know they exist.
func TestApprovalGatedBoundaryPoliciesAreOwnedByTheirHelperPod(t *testing.T) {
	_, cs, pods := startApprovalGated(t, "f2f3")
	if pods.helper.UID == "" {
		t.Fatal("helper pod has no UID; the ownership assertion below would be vacuous")
	}
	for name, policy := range listNetworkPolicies(t, cs) {
		if len(policy.OwnerReferences) != 1 {
			t.Fatalf("policy %s has %d owner references, want 1", name, len(policy.OwnerReferences))
		}
		owner := policy.OwnerReferences[0]
		if owner.Kind != "Pod" || owner.Name != pods.helper.Name || owner.UID != pods.helper.UID {
			t.Fatalf("policy %s owner = %s/%s (%s), want Pod/%s (%s)",
				name, owner.Kind, owner.Name, owner.UID, pods.helper.Name, pods.helper.UID)
		}
		if owner.Controller == nil || !*owner.Controller {
			t.Fatalf("policy %s owner reference is not the controller reference", name)
		}
	}
}

// Only this type moves a credential holder onto the pod network, so only this
// type needs the boundary. A policy on a shell or claude-code session would be
// a silent behaviour change for workloads whose egress was never restricted.
func TestOtherWorkloadTypesGetNoNetworkPolicies(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec k8s.WorkloadSpec
		opts []k8s.Option
	}{
		{"shell", k8s.WorkloadSpec{Type: session.WorkloadTypeShell}, nil},
		{"claude-code", k8s.WorkloadSpec{Type: session.WorkloadTypeClaudeCode},
			[]k8s.Option{k8s.WithWorkloadImage(session.WorkloadTypeClaudeCode, claudeCodeImage)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, cs := newReadyOrchestrator(t, tc.opts...)
			if _, err := orch.Start(context.Background(), "f2f4", tc.spec); err != nil {
				t.Fatalf("start %s session: %v", tc.name, err)
			}
			if policies := listNetworkPolicies(t, cs); len(policies) != 0 {
				t.Fatalf("%s session created network policies %v, want none", tc.name, policyNames(policies))
			}
		})
	}
}

// A restore builds a second pod pair before the first is reclaimed. Its
// policies must be distinct objects owned by the *new* helper pod, or the
// incoming round would inherit a boundary that disappears when the outgoing
// pod does.
func TestRestoreRoundGetsItsOwnBoundaryPolicies(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	if _, err := orch.Start(context.Background(), "f2f5",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated}); err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}
	restored, err := orch.RestoreInto(context.Background(), "f2f5", "archive-ref",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("restore approval-gated session: %v", err)
	}
	if len(restored.Auxiliary) != 1 {
		t.Fatalf("restored auxiliary pods = %d, want 1", len(restored.Auxiliary))
	}
	policies := listNetworkPolicies(t, cs)
	if len(policies) != 4 {
		t.Fatalf("network policies = %d, want 4 (two rounds x two policies)", len(policies))
	}
	newHelper := restored.Auxiliary[0].Name
	var owned int
	for _, policy := range policies {
		if policy.OwnerReferences[0].Name == newHelper {
			owned++
		}
	}
	if owned != 2 {
		t.Fatalf("policies owned by the restore round's helper = %d, want 2", owned)
	}
}

func assertPorts(t *testing.T, what string, ports []networkingv1.NetworkPolicyPort, want ...int32) {
	t.Helper()
	if len(ports) != len(want) {
		t.Fatalf("%s ports = %d, want %d", what, len(ports), len(want))
	}
	for i, port := range ports {
		if port.Port == nil || port.Port.IntVal != want[i] {
			t.Fatalf("%s port[%d] = %v, want %d", what, i, port.Port, want[i])
		}
	}
}

func policyNames(policies map[string]networkingv1.NetworkPolicy) []string {
	names := make([]string, 0, len(policies))
	for name := range policies {
		names = append(names, name)
	}
	return names
}
