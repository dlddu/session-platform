//go:build e2e

// AC-F5's shared volume, asserted against the deployed SUT.
//
// This file is deliberately not named e2e_*_test.go. That glob is the matching
// unit of the 1:1 declaration gate (scripts/e2e/), and AC-F5's dedicated e2e
// file and its registry row belong to a different consistency model; taking the
// name here would claim a mapping this change does not own. What it asserts is
// the half the integration suite cannot: a fake clientset agrees the pod specs
// name one claim, but only a real cluster can say the claim binds, both pods
// come up holding it, and the bytes one writes are the bytes the other reads.
//
// The path this exercises exists here because the kind overlay deploys a
// ReadWriteMany class and names it to the control plane
// (deploy/shared-volume-provisioner.yaml, deploy/kustomization.yaml). Remove
// either and these tests fail at the first claim, which is the intent — a
// silently unconfigured SUT would otherwise let AC-F5 pass unobserved.
package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// sharedClaimFor resolves the round's claim through the session label rather
// than by rebuilding the name from the helper pod, so a rename in the control
// plane surfaces here as "no claim" instead of quietly passing.
func sharedClaimFor(t *testing.T, cs kubernetes.Interface, ns, sessionID string) corev1.PersistentVolumeClaim {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		list, err := cs.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{
			LabelSelector: labelSessionID + "=" + sessionID,
		})
		if err != nil {
			t.Fatalf("list claims for session %s in %s: %v", sessionID, ns, err)
		}
		if len(list.Items) > 1 {
			t.Fatalf("session %s has %d claims, want exactly 1 per provisioning round (AC-F5)", sessionID, len(list.Items))
		}
		if len(list.Items) == 1 && list.Items[0].Status.Phase == corev1.ClaimBound {
			return list.Items[0]
		}
		if time.Now().After(deadline) {
			if len(list.Items) == 0 {
				t.Fatalf("session %s has no shared volume claim — is SESSION_SHARED_VOLUME_STORAGE_CLASS set on the SUT? (AC-F5)", sessionID)
			}
			t.Fatalf("claim %s for session %s is %s, never Bound — does the class serve ReadWriteMany? (AC-F5)",
				list.Items[0].Name, sessionID, list.Items[0].Status.Phase)
		}
		time.Sleep(time.Second)
	}
}

// mountsOfClaim answers "which containers hold this claim, and where", going
// claim -> volume -> mount. Comparing paths that were each resolved from the
// claim name is what makes "the same path" a property of the pods rather than
// of two literals that happen to agree.
func mountsOfClaim(pod *corev1.Pod, claimName string) map[string]string {
	volumes := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
			volumes[v.Name] = true
		}
	}
	out := map[string]string{}
	for _, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if volumes[m.Name] {
				out[c.Name] = m.MountPath
			}
		}
	}
	return out
}

func TestSharedVolume_BothPodsHoldOneReadWriteManyClaimAtTheSamePath(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: a bound claim and a mounted path are claims about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	workload := getPodEventually(t, cs, ns, s.Pod)
	claim := sharedClaimFor(t, cs, ns, s.ID)

	wantMode := false
	for _, m := range claim.Spec.AccessModes {
		if m == corev1.ReadWriteMany {
			wantMode = true
		}
	}
	if !wantMode {
		t.Fatalf("claim %s has accessModes=%v, want ReadWriteMany (AC-F5)", claim.Name, claim.Spec.AccessModes)
	}

	workloadMounts := mountsOfClaim(workload, claim.Name)
	helperMounts := mountsOfClaim(&helper, claim.Name)
	wPath, wOK := workloadMounts[workloadContainer]
	if !wOK {
		t.Fatalf("workload pod %s does not mount claim %s in container %q (mounted: %v) (AC-F5)",
			workload.Name, claim.Name, workloadContainer, workloadMounts)
	}
	hPath, hOK := helperMounts[sessionMCPContainer]
	if !hOK {
		t.Fatalf("helper pod %s does not mount claim %s in container %q (mounted: %v) (AC-F5)",
			helper.Name, claim.Name, sessionMCPContainer, helperMounts)
	}
	if wPath != hPath {
		t.Fatalf("the pair mounts claim %s at different paths — workload %q, helper %q (AC-F5)", claim.Name, wPath, hPath)
	}
	if _, mounted := helperMounts[helperCredProxyContainer]; mounted {
		t.Fatalf("helper pod %s mounts claim %s into %q, which does not handle files (AC-F5)",
			helper.Name, claim.Name, helperCredProxyContainer)
	}

	// The pod specs agreeing is not the same fact as the two containers seeing
	// one filesystem: a class that binds per node would satisfy every assertion
	// above and fail here.
	marker := fmt.Sprintf("f5-%s", s.ID)
	f4Sh(t, cs, cfg, ns, helper.Name, sessionMCPContainer,
		`set -eu; printf '%s' "$1" > "$2/probe"`, marker, hPath)
	got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, workload.Name, workloadContainer,
		`set -eu; cat "$1/probe"`, wPath))
	if got != marker {
		t.Fatalf("workload read %q at %s but the helper wrote %q — the two mounts are not one filesystem (AC-F5)", got, wPath, marker)
	}
}

// "그 세션 전용" is the half of AC-F5 a shared backing directory could break
// without any pod spec looking wrong.
func TestSharedVolume_EachSessionGetsItsOwnAndSeesNoOther(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: volume privacy is a claim about deployed pods")
	}
	ns := sessionNamespace()

	first, second := f4Create(t), f4Create(t)
	claimOne := sharedClaimFor(t, cs, ns, first.ID)
	claimTwo := sharedClaimFor(t, cs, ns, second.ID)
	if claimOne.Name == claimTwo.Name {
		t.Fatalf("both sessions were given claim %q — the volume is shared between sessions (AC-F5, AC-A2)", claimOne.Name)
	}

	helperOne := f4TheHelperPod(t, cs, ns, first)
	helperTwo := f4TheHelperPod(t, cs, ns, second)
	pathOne := mountsOfClaim(&helperOne, claimOne.Name)[sessionMCPContainer]
	pathTwo := mountsOfClaim(&helperTwo, claimTwo.Name)[sessionMCPContainer]
	if pathOne == "" || pathTwo == "" {
		t.Fatalf("a helper does not mount its own claim: %q / %q (AC-F5)", pathOne, pathTwo)
	}

	f4Sh(t, cs, cfg, ns, helperOne.Name, sessionMCPContainer,
		`set -eu; printf 'first' > "$1/private"`, pathOne)
	got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, helperTwo.Name, sessionMCPContainer,
		`if [ -e "$1/private" ]; then cat "$1/private"; else printf absent; fi`, pathTwo))
	if got != "absent" {
		t.Fatalf("the second session reads %q from its own mount — it is looking at the first session's volume (AC-F5, AC-A2)", got)
	}
}

// The claim is owned by the round's helper pod, so it is garbage collected with
// the pair rather than by a delete call that could be forgotten.
func TestSharedVolume_ClaimIsReclaimedWithTheSession(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: reclaim is a claim about deployed objects")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	claim := sharedClaimFor(t, cs, ns, s.ID)
	if len(claim.OwnerReferences) == 0 {
		t.Fatalf("claim %s has no owner reference; nothing reclaims it with the pod pair (AC-F5)", claim.Name)
	}

	if resp, body := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil); resp.StatusCode >= 400 {
		t.Fatalf("delete session %s: status=%d body=%s", s.ID, resp.StatusCode, body)
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), claim.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get claim %s/%s: %v", ns, claim.Name, err)
		}
		if got.DeletionTimestamp != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim %s is still present after the session was deleted — it outlives its pods (AC-F5)", claim.Name)
		}
		time.Sleep(time.Second)
	}
}
