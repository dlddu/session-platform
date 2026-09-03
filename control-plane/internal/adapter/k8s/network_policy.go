// AC-F2's network boundary for an approval-gated session, as Kubernetes
// objects. The PRD states the property in terms of what the *workload* pod may
// reach: kube-dns and its own session's helper pod, and nothing else — not the
// provider API origin, not another session's helper, not the approval gateway.
// Two policies express that, and both are created with the session:
//
//   - a workload egress allowlist, which is the property itself, and
//   - a helper ingress restriction, which is what makes the helper pod's
//     credential proxy safe to bind to the pod network at all (AC-F6): without
//     it, any pod in the namespace could borrow the platform's provider token
//     by dialling the helper's proxy port directly.
//
// The two ship together for that reason — splitting them would leave the
// second half of the boundary open while the first half looked done.
//
// NOT verified here: whether the cluster *enforces* any of this. Enforcement is
// the CNI's job and the e2e cluster's kindnet does not implement NetworkPolicy,
// so "the object exists, selects the right pods, and is reclaimed with them" is
// what this code and its tests establish. docs/doc-tracker.md carries the
// remainder as an open item.
package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// kubeDNSNamespaceLabel is the immutable label kube-apiserver puts on every
	// namespace, which is how a policy names kube-system without the cluster
	// having had to label it by hand.
	kubeDNSNamespaceLabel = "kubernetes.io/metadata.name"
	kubeSystemNamespace   = "kube-system"
	kubeDNSAppLabel       = "k8s-app"
	kubeDNSAppValue       = "kube-dns"
	dnsPort               = 53

	// networkPolicyEgressSuffix and networkPolicyIngressSuffix are appended to
	// the round's helper pod name. Deriving both names from that pod keeps them
	// unique per provisioning round exactly as the pods are, so a restore's new
	// pair never collides with the pair it replaces.
	networkPolicyEgressSuffix  = "-workload-egress"
	networkPolicyIngressSuffix = "-helper-ingress"
)

// sessionNetworkPolicies renders the pair for one session. The selectors are
// session-scoped (not round-scoped) because the property is about the session:
// during a restore both rounds' pods carry the same session id and both must be
// inside the boundary. NetworkPolicies are additive, so the outgoing round's
// pair and the incoming one's overlap harmlessly until the old pods go.
//
// owner is that round's helper pod. Both policies are owned by it, so Kubernetes
// reclaims them exactly when the pod they exist for is reclaimed — on freeze, on
// delete, and on a failed provisioning rollback (AC-A3, AC-F4).
func sessionNetworkPolicies(namespace, sessionID string, owner *corev1.Pod) []*networkingv1.NetworkPolicy {
	workloadSelector := metav1.LabelSelector{MatchLabels: map[string]string{
		LabelSessionID: sessionID,
		LabelPodRole:   PodRoleWorkload,
	}}
	helperSelector := metav1.LabelSelector{MatchLabels: map[string]string{
		LabelSessionID: sessionID,
		LabelPodRole:   PodRoleHelper,
	}}
	helperPorts := []networkingv1.NetworkPolicyPort{
		tcpPort(credentialProxyPort),
		tcpPort(SessionMCPPort),
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelSessionID: sessionID,
				labelManagedBy: managedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{ownerReferenceTo(owner)},
		}
	}

	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	dns := intstr.FromInt32(dnsPort)

	return []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: meta(owner.Name + networkPolicyEgressSuffix),
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: workloadSelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						// (a) name resolution only — the agent may look a name
						// up, but the allowlist below decides what it can then
						// connect to.
						To: []networkingv1.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
								kubeDNSNamespaceLabel: kubeSystemNamespace,
							}},
							PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
								kubeDNSAppLabel: kubeDNSAppValue,
							}},
						}},
						Ports: []networkingv1.NetworkPolicyPort{
							{Protocol: &udp, Port: &dns},
							{Protocol: &tcp, Port: &dns},
						},
					},
					{
						// (b) this session's own helper pod, and only its two
						// ports. The peer carries no namespace selector, so it
						// cannot match a pod outside this namespace; the session
						// id in the selector is what excludes another session's
						// helper inside it.
						To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &helperSelector}},
						Ports: helperPorts,
					},
				},
			},
		},
		{
			ObjectMeta: meta(owner.Name + networkPolicyIngressSuffix),
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: helperSelector,
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &workloadSelector}},
					Ports: helperPorts,
				}},
			},
		},
	}
}

// applySessionNetworkPolicies creates the pair. It runs after the helper pod is
// created (the owner reference needs its UID) but before it is waited on and
// before the workload pod exists, so the boundary is in place for the whole
// life of the pods it governs. An existing object is left alone: names are
// per-round, so the only way to meet one is a retry of this same round.
func (o *ClientOrchestrator) applySessionNetworkPolicies(ctx context.Context, sessionID string, owner *corev1.Pod) error {
	for _, policy := range sessionNetworkPolicies(o.namespace, sessionID, owner) {
		_, err := o.client.NetworkingV1().NetworkPolicies(o.namespace).Create(ctx, policy, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create network policy %s/%s: %w", o.namespace, policy.Name, err)
		}
	}
	return nil
}

// ownerReferenceTo makes obj's lifetime the owner pod's. BlockOwnerDeletion is
// left off deliberately: setting it would require the control plane to hold
// update rights on pods/finalizers, which is a wider grant than reclaiming a
// policy is worth.
func ownerReferenceTo(owner *corev1.Pod) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       owner.Name,
		UID:        owner.UID,
		Controller: &controller,
	}
}

func tcpPort(port int) networkingv1.NetworkPolicyPort {
	protocol := corev1.ProtocolTCP
	value := intstr.FromInt32(int32(port))
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &value}
}
