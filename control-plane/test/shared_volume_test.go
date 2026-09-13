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

// The path is only half the wiring: the data plane has to be *told* it. The MCP
// container spills large approved responses into this directory and the
// workload's agent reads them back from it (AC-F5), and neither can do that
// from a pod spec it cannot see. Reading the value out of the running
// containers rather than out of the spec is what makes this an assertion about
// the deployed SUT — an image that ignored the variable would still pass a spec
// check.
func TestSharedVolume_BothContainersAreToldWhereItIs(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: what a running container was told is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	workload := getPodEventually(t, cs, ns, s.Pod)
	claim := sharedClaimFor(t, cs, ns, s.ID)

	const readEnv = `set -eu; printf %s "${SESSION_SHARED_DIR-}"`
	helperDir := strings.TrimSpace(f4Sh(t, cs, cfg, ns, helper.Name, sessionMCPContainer, readEnv))
	workloadDir := strings.TrimSpace(f4Sh(t, cs, cfg, ns, workload.Name, workloadContainer, readEnv))
	if helperDir == "" || workloadDir == "" {
		t.Fatalf("SESSION_SHARED_DIR is %q in the MCP container and %q in the workload container; "+
			"a mounted volume nothing was told about is one nothing writes to (AC-F5)", helperDir, workloadDir)
	}
	if helperDir != workloadDir {
		t.Fatalf("the pair was told different directories — MCP %q, workload %q (AC-F5)", helperDir, workloadDir)
	}
	// And it is the mount, not merely a directory both agree on.
	if got := mountsOfClaim(&helper, claim.Name)[sessionMCPContainer]; got != helperDir {
		t.Fatalf("MCP container mounts claim %s at %q but was told %q (AC-F5)", claim.Name, got, helperDir)
	}
	if got := mountsOfClaim(workload, claim.Name)[workloadContainer]; got != workloadDir {
		t.Fatalf("workload container mounts claim %s at %q but was told %q (AC-F5)", claim.Name, got, workloadDir)
	}

	// The spill subtree is created by the MCP on first use, so what is checked
	// here is that a directory made under that path by one container is the
	// directory the other one reads — the property a per-node class would break
	// one level below the probe file the case above writes.
	marker := fmt.Sprintf("f5-env-%s", s.ID)
	f4Sh(t, cs, cfg, ns, helper.Name, sessionMCPContainer,
		`set -eu; mkdir -p "$1/web_fetch_get"; printf '%s' "$2" > "$1/web_fetch_get/probe.body"`, helperDir, marker)
	got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, workload.Name, workloadContainer,
		`set -eu; cat "$1/web_fetch_get/probe.body"`, workloadDir))
	if got != marker {
		t.Fatalf("workload read %q under %s but the MCP wrote %q — a spilled file would not arrive (AC-F5)",
			got, workloadDir, marker)
	}
}

// The half AC-F5 states in the present tense — "동결·복원을 건너서도 이전 실행이
// 만든 파일이 남아 있고" — and the only one no pod spec can be read for. The
// claim is owned by the round's helper pod, so the freeze garbage collects it:
// nothing in the cluster survives to be re-read, and the bytes can only come
// back through the filesystem archive.
func TestSharedVolume_SurvivesFreezeAndRestore(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: what a freeze carries is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	workload := getPodEventually(t, cs, ns, s.Pod)
	claim := sharedClaimFor(t, cs, ns, s.ID)
	mountPath := mountsOfClaim(workload, claim.Name)[workloadContainer]
	if mountPath == "" {
		t.Fatalf("workload pod %s does not mount claim %s; there is no volume to carry (AC-F5)", workload.Name, claim.Name)
	}

	// Written through the MCP container, which is the container that actually
	// spills approved results, and read back before the freeze so a match after
	// it is a positive result rather than two failures agreeing.
	marker := fmt.Sprintf("f5-roundtrip-%s", s.ID)
	f4Sh(t, cs, cfg, ns, helper.Name, sessionMCPContainer,
		`set -eu; mkdir -p "$1/web_fetch_get"; printf '%s' "$2" > "$1/web_fetch_get/approved.body"`,
		mountsOfClaim(&helper, claim.Name)[sessionMCPContainer], marker)
	if got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, workload.Name, workloadContainer,
		`set -eu; cat "$1/web_fetch_get/approved.body"`, mountPath)); got != marker {
		t.Fatalf("workload read %q before the freeze, want %q — the probe does not work, so surviving one would prove nothing",
			got, marker)
	}

	if _, found := snapshotSession(t, s.ID); !found {
		t.Fatal("snapshot endpoint reported the session missing")
	}
	// The old claim goes with the old helper, which is exactly why the archive
	// has to be carrying these bytes.
	awaitClaimGone(t, cs, ns, claim.Name)

	restored := f4Switch(t, s.ID)
	newHelper := f4TheHelperPod(t, cs, ns, restored)
	newWorkload := getPodEventually(t, cs, ns, restored.Pod)
	newClaim := sharedClaimFor(t, cs, ns, restored.ID)
	if newClaim.Name == claim.Name {
		t.Fatalf("restored onto claim %q, the one the freeze reclaimed — the round would be reusing a deleted volume (AC-F5)", claim.Name)
	}
	newPath := mountsOfClaim(newWorkload, newClaim.Name)[workloadContainer]
	if newPath == "" {
		t.Fatalf("restored workload pod %s does not mount claim %s (AC-F5)", newWorkload.Name, newClaim.Name)
	}
	if got := mountsOfClaim(&newHelper, newClaim.Name)[sessionMCPContainer]; got != newPath {
		t.Fatalf("the restored pair mounts claim %s at different paths — workload %q, helper %q (AC-F5)",
			newClaim.Name, newPath, got)
	}

	got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, newWorkload.Name, workloadContainer,
		`set -eu; cat "$1/web_fetch_get/approved.body"`, newPath))
	if got != marker {
		t.Fatalf("after a freeze and restore the workload reads %q at %s, want %q — the approved result did not cross (AC-F5)",
			got, newPath, marker)
	}
}

// awaitClaimGone waits out the garbage collection the owner reference drives,
// which is asynchronous rather than part of the delete the freeze issues.
func awaitClaimGone(t *testing.T, cs kubernetes.Interface, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		claim, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get claim %s/%s: %v", ns, name, err)
		}
		if claim.DeletionTimestamp != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim %s is still present after the freeze — it outlives the helper pod that owns it (AC-F5)", name)
		}
		time.Sleep(time.Second)
	}
}
