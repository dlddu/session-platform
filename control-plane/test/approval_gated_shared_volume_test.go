//go:build integration

// AC-F5's volume topology and lifetime, against the same fake clientset as the
// rest of the approval-gated pod-shape suite. What a fake cannot show — a file
// the MCP container wrote being read by the workload, and the volume surviving
// a freeze/restore round — is not implemented yet either; docs/doc-tracker.md
// carries the remainder.
//
// Deliberately not named e2e_*_test.go: that glob is the AC ↔ e2e mapping's
// matching unit, and a dedicated e2e file for AC-F5 comes with a registry row
// in docs/test/e2e.md, which this suite does not own.
package integration_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func listClaims(t *testing.T, cs *fake.Clientset) []corev1.PersistentVolumeClaim {
	t.Helper()
	list, err := cs.CoreV1().PersistentVolumeClaims(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list persistent volume claims: %v", err)
	}
	return list.Items
}

// mountPath resolves the pod's volume name through to its claim, so what is
// asserted is the claim the container actually sees rather than a volume name
// two specs happen to share.
func mountPath(t *testing.T, pod corev1.Pod, containerName, claimName string) (string, bool) {
	t.Helper()
	volumeName := ""
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
			volumeName = v.Name
		}
	}
	if volumeName == "" {
		return "", false
	}
	for _, m := range container(t, pod, containerName).VolumeMounts {
		if m.Name == volumeName {
			return m.MountPath, true
		}
	}
	return "", false
}

func onlyClaim(t *testing.T, cs *fake.Clientset) corev1.PersistentVolumeClaim {
	t.Helper()
	claims := listClaims(t, cs)
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want exactly 1 for the session (AC-F5)", len(claims))
	}
	return claims[0]
}

// ReadWriteMany is the property itself — it is what lets two pods hold the
// volume at once — so it is asserted rather than assumed.
func TestApprovalGated_SessionGetsItsOwnReadWriteManyClaim(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	if _, err := orch.Start(context.Background(), "f5a1",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated}); err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}

	claim := onlyClaim(t, cs)
	if got := claim.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteMany {
		t.Fatalf("claim %s access modes = %v, want [ReadWriteMany] (AC-F5)", claim.Name, got)
	}
	if claim.Labels[k8s.LabelSessionID] != "f5a1" {
		t.Fatalf("claim %s session label = %q, want the session's own id",
			claim.Name, claim.Labels[k8s.LabelSessionID])
	}
	// Unset, not empty: the empty string means "bind only to a statically
	// provisioned volume", which would leave the claim Pending forever on a
	// cluster that provisions dynamically.
	if claim.Spec.StorageClassName != nil {
		t.Fatalf("claim %s storage class = %q, want unset so the cluster default is used",
			claim.Name, *claim.Spec.StorageClassName)
	}
	if claim.Spec.Resources.Requests.Storage().IsZero() {
		t.Fatalf("claim %s requests no storage", claim.Name)
	}
}

// The cluster that has a ReadWriteMany class rarely has it under the default
// name, so the configured one must reach the claim.
func TestApprovalGated_ClaimUsesTheConfiguredStorageClass(t *testing.T) {
	const class = "nfs-rwx"
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage),
		k8s.WithSharedVolume(class, resource.MustParse("4Gi")))
	if _, err := orch.Start(context.Background(), "f5a2",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated}); err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}

	claim := onlyClaim(t, cs)
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != class {
		t.Fatalf("claim %s storage class = %v, want %q", claim.Name, claim.Spec.StorageClassName, class)
	}
	if got := claim.Spec.Resources.Requests.Storage().String(); got != "4Gi" {
		t.Fatalf("claim %s size = %s, want 4Gi", claim.Name, got)
	}
}

// AC-F5's central property, and the one a wrong refactor is likeliest to break:
// same claim, same path, and not in the proxy container beside it.
func TestApprovalGated_WorkloadAndMCPShareTheClaimAtOnePath(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	started, err := orch.Start(context.Background(), "f5b1",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}
	set := newPodSet(t, listPods(t, cs), started)
	claim := onlyClaim(t, cs)

	workloadPath, ok := mountPath(t, set.workload, "session", claim.Name)
	if !ok {
		t.Fatalf("workload container does not mount claim %s (AC-F5)", claim.Name)
	}
	mcpPath, ok := mountPath(t, set.helper, k8s.SessionMCPContainerName, claim.Name)
	if !ok {
		t.Fatalf("helper %s container does not mount claim %s (AC-F5)",
			k8s.SessionMCPContainerName, claim.Name)
	}
	if workloadPath != mcpPath {
		t.Fatalf("shared claim is mounted at %q in the workload and %q in the MCP container; "+
			"AC-F5 requires one path", workloadPath, mcpPath)
	}
	if workloadPath != k8s.SharedVolumeMountPath {
		t.Fatalf("shared claim mount path = %q, want %q", workloadPath, k8s.SharedVolumeMountPath)
	}

	// Mounts are per container: the proxy sits in the same pod and still must
	// not see the path.
	if path, ok := mountPath(t, set.helper, k8s.HelperCredentialProxyContainerName, claim.Name); ok {
		t.Fatalf("helper %s container mounts the shared claim at %q; AC-F5 excludes it",
			k8s.HelperCredentialProxyContainerName, path)
	}
	for _, m := range container(t, set.helper, k8s.HelperCredentialProxyContainerName).VolumeMounts {
		if m.MountPath == k8s.SharedVolumeMountPath {
			t.Fatalf("helper %s container has something else mounted at the shared path %q",
				k8s.HelperCredentialProxyContainerName, m.MountPath)
		}
	}
}

// Reclamation is garbage collection following this reference, which is why
// k8s/rbac.yaml grants no delete right on claims. Asserting the reference is
// the only place that dependency is checkable.
func TestApprovalGated_ClaimIsOwnedByItsHelperPod(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	started, err := orch.Start(context.Background(), "f5c1",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}
	set := newPodSet(t, listPods(t, cs), started)
	if set.helper.UID == "" {
		t.Fatal("helper pod has no UID; the ownership assertion below would be vacuous")
	}

	claim := onlyClaim(t, cs)
	if len(claim.OwnerReferences) != 1 {
		t.Fatalf("claim %s has %d owner references, want 1", claim.Name, len(claim.OwnerReferences))
	}
	owner := claim.OwnerReferences[0]
	if owner.Kind != "Pod" || owner.Name != set.helper.Name || owner.UID != set.helper.UID {
		t.Fatalf("claim %s owner = %s/%s (%s), want Pod/%s (%s)",
			claim.Name, owner.Kind, owner.Name, owner.UID, set.helper.Name, set.helper.UID)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Fatalf("claim %s owner reference is not the controller reference", claim.Name)
	}
}

// Reusing the outgoing round's claim would tie the incoming pods' volume to a
// pod that is being reclaimed.
func TestApprovalGated_RestoreRoundGetsItsOwnClaim(t *testing.T) {
	orch, cs := newReadyOrchestrator(t,
		k8s.WithWorkloadImage(session.WorkloadTypeApprovalGated, approvalGatedImage))
	if _, err := orch.Start(context.Background(), "f5d1",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated}); err != nil {
		t.Fatalf("start approval-gated session: %v", err)
	}
	first := onlyClaim(t, cs).Name

	restored, err := orch.RestoreInto(context.Background(), "f5d1", "archive-ref",
		k8s.WorkloadSpec{Type: session.WorkloadTypeApprovalGated})
	if err != nil {
		t.Fatalf("restore approval-gated session: %v", err)
	}
	if len(restored.Auxiliary) != 1 {
		t.Fatalf("restore produced %d auxiliary pods, want 1", len(restored.Auxiliary))
	}
	newHelper := restored.Auxiliary[0].Name

	claims := listClaims(t, cs)
	if len(claims) != 2 {
		t.Fatalf("claims after restore = %d, want 2 (one per round)", len(claims))
	}
	var owned int
	for _, claim := range claims {
		if claim.Name != first && claim.OwnerReferences[0].Name == newHelper {
			owned++
		}
	}
	if owned != 1 {
		t.Fatalf("claims owned by the restore round's helper = %d, want 1", owned)
	}
}

// Only this type has two pods to share a volume between; a claim for the others
// would make cluster storage a dependency of types that never needed one.
func TestOtherWorkloadTypesGetNoSharedClaim(t *testing.T) {
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
			if _, err := orch.Start(context.Background(), "f5e1", tc.spec); err != nil {
				t.Fatalf("start %s session: %v", tc.name, err)
			}
			if claims := listClaims(t, cs); len(claims) != 0 {
				t.Fatalf("%s session created %d claims, want 0", tc.name, len(claims))
			}
			pods := listPods(t, cs)
			for _, pod := range pods {
				for _, v := range pod.Spec.Volumes {
					if v.PersistentVolumeClaim != nil {
						t.Fatalf("%s pod %s declares a claim-backed volume %q",
							tc.name, pod.Name, v.Name)
					}
				}
			}
		})
	}
}
