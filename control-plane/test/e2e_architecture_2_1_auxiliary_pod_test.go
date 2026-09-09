//go:build e2e

// 검증 시나리오: architecture.md#시나리오 2-1
//
// docs/prd/architecture.md AC-A2 (보조 파드 절, AC-F4가 구체화한다), asserted on the
// deployed SUT. 무엇을 단언하는지·무엇이 대조군이고 어느 이웃 파일이 그것을 소유하는지는
// docs/test/e2e.md 의 이 파일 매핑 행에 있다.
package e2e_test

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodRoleWorkload as the deployed pod spec spells it. Written out rather than
// imported for the reason e2e_f1 gives for the other three.
const podRoleWorkload = "workload"

// helperPodsFor cannot serve here: it always narrows to the helper role, and
// what this file compares is the *same* label query at two widths.
func a21PodsBySelector(t *testing.T, cs kubernetes.Interface, ns, selector string) []corev1.Pod {
	t.Helper()
	list, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatalf("list pods in %s with %q: %v", ns, selector, err)
	}
	return list.Items
}

func a21PodNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	return names
}

func a21OwnedPods(t *testing.T, cs kubernetes.Interface, ns, sessionID string) (all, workload []corev1.Pod) {
	t.Helper()
	all = a21PodsBySelector(t, cs, ns, labelSessionID+"="+sessionID)
	workload = a21PodsBySelector(t, cs, ns, labelSessionID+"="+sessionID+","+labelPodRole+"="+podRoleWorkload)
	return all, workload
}

func TestAuxiliaryPod_OneToOneStatementHoldsOnWorkloadPods(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: the auxiliary-pod clause of AC-A2 is a claim about deployed pods")
	}
	ns := sessionNamespace()

	first, second := f4Create(t), f4Create(t)
	if first.ID == second.ID {
		t.Fatal("the two sessions share an id; they cannot be told apart")
	}

	for _, s := range []f4Session{first, second} {
		all, workload := a21OwnedPods(t, cs, ns, s.ID)
		if len(all) != 2 {
			t.Fatalf("session %s owns %d pods %v, want exactly 2 (workload + auxiliary, AC-A2 보조 파드 절)",
				s.ID, len(all), a21PodNames(all))
		}
		if len(workload) != 1 {
			t.Fatalf("session %s selects %d workload pods %v, want exactly 1 — the auxiliary pod broke the 1:1 statement (AC-A2)",
				s.ID, len(workload), a21PodNames(workload))
		}
		if workload[0].Name != s.Pod {
			t.Fatalf("session %s reports pod=%q but the cluster's workload pod is %q (AC-A2)", s.ID, s.Pod, workload[0].Name)
		}
		helpers := helperPodsFor(t, cs, ns, s.ID)
		if len(helpers) != 1 {
			t.Fatalf("session %s selects %d helper pods %v, want exactly 1", s.ID, len(helpers), a21PodNames(helpers))
		}
		if len(s.AuxiliaryPods) != 1 || s.AuxiliaryPods[0] != helpers[0].Name {
			t.Fatalf("session %s reports auxiliaryPods=%v but the cluster's auxiliary pod is %q (AC-A2)",
				s.ID, s.AuxiliaryPods, helpers[0].Name)
		}
	}

	seen := map[string]string{} // pod name -> what claims it
	for _, s := range []f4Session{first, second} {
		for role, name := range map[string]string{"workload": s.Pod, "auxiliary": s.AuxiliaryPods[0]} {
			if prev, dup := seen[name]; dup {
				t.Fatalf("pod %q is both %s of session %s and %s (AC-A2 격리)", name, role, s.ID, prev)
			}
			seen[name] = role + " of session " + s.ID
		}
	}
	if len(seen) != 4 {
		t.Fatalf("two approval-gated sessions account for %d distinct pods %v, want 4", len(seen), seen)
	}

	plain := createSession(t, uniqueName(t))
	plainAll, plainWorkload := a21OwnedPods(t, cs, ns, plain.ID)
	if len(plainAll) != 1 || len(plainWorkload) != 1 {
		t.Fatalf("shell session %s owns %d pods %v (%d workload) — a type without auxiliary pods should give the same answer to both selectors",
			plain.ID, len(plainAll), a21PodNames(plainAll), len(plainWorkload))
	}
	if plainWorkload[0].Name != plain.Pod {
		t.Fatalf("shell session %s reports pod=%q but the cluster's workload pod is %q", plain.ID, plain.Pod, plainWorkload[0].Name)
	}
}

func TestAuxiliaryPod_DeletingOneSessionLeavesTheOtherPairStanding(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: what a delete leaves standing is a claim about deployed pods")
	}
	ns := sessionNamespace()

	doomed, survivor := f4Create(t), f4Create(t)
	doomedHelper := f4TheHelperPod(t, cs, ns, doomed)
	survivorHelper := f4TheHelperPod(t, cs, ns, survivor)
	getPodEventually(t, cs, ns, doomed.Pod)
	getPodEventually(t, cs, ns, survivor.Pod)

	resp, body := do(t, http.MethodDelete, "/api/v1/sessions/"+doomed.ID, nil)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("delete session %s: status=%d body=%s", doomed.ID, resp.StatusCode, body)
	}

	// Control: the delete really did reclaim something (AC-F4 owns this claim).
	f4AwaitReclaimed(t, cs, ns, doomed.Pod, "the delete")
	f4AwaitReclaimed(t, cs, ns, doomedHelper.Name, "the delete")

	got := f4Get(t, survivor.ID)
	if got.State != "active" {
		t.Fatalf("surviving session %s state = %q after a neighbour was deleted, want active (AC-A2 격리)", survivor.ID, got.State)
	}
	if got.Pod != survivor.Pod {
		t.Fatalf("surviving session %s workload pod = %q, want the unchanged %q (AC-A2 격리)", survivor.ID, got.Pod, survivor.Pod)
	}
	if len(got.AuxiliaryPods) != 1 || got.AuxiliaryPods[0] != survivorHelper.Name {
		t.Fatalf("surviving session %s auxiliaryPods = %v, want the unchanged [%s] (AC-A2 격리)",
			survivor.ID, got.AuxiliaryPods, survivorHelper.Name)
	}

	// A pod already marked for deletion still answers Get, so the deletion
	// timestamp is part of "untouched".
	all, workload := a21OwnedPods(t, cs, ns, survivor.ID)
	if len(all) != 2 || len(workload) != 1 {
		t.Fatalf("surviving session %s owns %d pods %v (%d workload) after a neighbour was deleted, want 2 (1 workload)",
			survivor.ID, len(all), a21PodNames(all), len(workload))
	}
	for _, name := range []string{survivor.Pod, survivorHelper.Name} {
		pod := getPodEventually(t, cs, ns, name)
		if pod.DeletionTimestamp != nil {
			t.Fatalf("pod %s of the surviving session is terminating after a *different* session was deleted (AC-A2 격리)", name)
		}
		if pod.Status.Phase != corev1.PodRunning {
			t.Fatalf("pod %s of the surviving session is in phase %q after a different session was deleted, want Running",
				name, pod.Status.Phase)
		}
	}
	for _, container := range []string{sessionMCPContainer, helperCredProxyContainer} {
		found, ready := containerReady(&survivorHelper, container)
		if !found {
			t.Fatalf("auxiliary pod %s publishes no status for container %q", survivorHelper.Name, container)
		}
		if !ready {
			t.Fatalf("auxiliary pod %s container %q was not Ready to begin with; its survival proves nothing", survivorHelper.Name, container)
		}
	}
	stillReady := getPodEventually(t, cs, ns, survivorHelper.Name)
	for _, container := range []string{sessionMCPContainer, helperCredProxyContainer} {
		found, ready := containerReady(stillReady, container)
		if !found || !ready {
			t.Fatalf("auxiliary pod %s container %q is no longer Ready after a different session was deleted (AC-A2 격리)",
				survivorHelper.Name, container)
		}
	}
}
