//go:build e2e

// 검증 시나리오: architecture.md#시나리오 2-1
//
// docs/prd/architecture.md AC-A2 (보조 파드 절, AC-F4가 구체화한다), asserted on the
// deployed SUT.
//
// 이 파일이 배타적으로 사는 것은 **격리 진술의 불변**이다. AC-A2는 "세션 1개 ↔ 워크로드
// 파드 1개"인데 보조 파드를 갖는 타입이 생기면서 한 세션의 파드가 둘이 됐다 — 그래도 그
// 진술이 깨지지 않는다는 것이 시나리오 2-1이다. 무엇을 단언하는지는 docs/test/e2e.md 의
// 매핑 행에 있고, 여기 적어 둘 것은 그 행이 담지 못하는 **경계**다.
//
// approval-gated-workload.md#시나리오 7(e2e_f4_helper_pod_test.go)과의 경계: 7은 헬퍼
// 파드의 **귀속·수명·PID 경계**를 산다. 그래서 아래에서 "지워진 세션의 파드 둘이
// 사라진다"는 이 파일의 산출이 **아니라 대조군**이다 — 아무것도 지우지 않는 delete 라면
// "다른 세션이 남았다"는 관찰이 공허해진다. 산출은 그 옆의 **범위**다. 헬퍼 파드의 존재와
// 컨테이너 구성(AC-F1)·자격 증명 배치(AC-F6)도 각자의 파일이 소유한다.
//
// 하네스는 같은 패키지의 것을 재사용한다(f4Create/f4Get/f4AwaitReclaimed·helperPodsFor) —
// e2e_f4가 helperPodsFor를 e2e_f1에서, execInContainer를 e2e_e1에서 가져다 쓰는 것과 같은
// 관례다.
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
// imported for the reason e2e_f1 gives for the other three: a rename in the
// control plane has to fail this file, not travel into it.
const podRoleWorkload = "workload"

// a21PodsBySelector is the one primitive this file needs that the suite does not
// already have: the *same* label query at two widths. helperPodsFor always adds
// the helper role; here the whole point is to compare "everything this session
// owns" against "the workload pod this session owns".
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

// a21OwnedPods returns (all pods carrying the session id, the subset labelled as
// its workload pod).
func a21OwnedPods(t *testing.T, cs kubernetes.Interface, ns, sessionID string) (all, workload []corev1.Pod) {
	t.Helper()
	all = a21PodsBySelector(t, cs, ns, labelSessionID+"="+sessionID)
	workload = a21PodsBySelector(t, cs, ns, labelSessionID+"="+sessionID+","+labelPodRole+"="+podRoleWorkload)
	return all, workload
}

// The 1:1 statement AC-A2 makes is about the *workload* pod. This asserts it on
// a type that has an auxiliary pod (where a session-id-only selector returns
// two) and, as the contrast that gives that number meaning, on a type that has
// none (where the two selectors agree).
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
		// The remaining pod is the auxiliary one, and the API says the same.
		helpers := helperPodsFor(t, cs, ns, s.ID)
		if len(helpers) != 1 {
			t.Fatalf("session %s selects %d helper pods %v, want exactly 1", s.ID, len(helpers), a21PodNames(helpers))
		}
		if len(s.AuxiliaryPods) != 1 || s.AuxiliaryPods[0] != helpers[0].Name {
			t.Fatalf("session %s reports auxiliaryPods=%v but the cluster's auxiliary pod is %q (AC-A2)",
				s.ID, s.AuxiliaryPods, helpers[0].Name)
		}
	}

	// ① the auxiliary pods are not shared, and neither are the workload pods:
	// four sessions' worth of pod would collapse to fewer names if either were.
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

	// The contrast: for a type with no auxiliary pod the two selectors agree, so
	// the "want exactly 1 workload pod" above is not a number every session has
	// by construction — it survives the auxiliary pod, which is the claim.
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

// ③ Deleting a session is scoped to that session. The reclaim of the deleted
// session's own pair is approval-gated-workload.md#시나리오 7's property and is
// only the control here — without it, "the other session is still standing"
// would also be true of a delete that did nothing at all.
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

	// The property: it stopped there. The API record first…
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

	// …then the cluster, which is the half that costs resources. A pod already
	// marked for deletion still answers Get, so the deletion timestamp is part of
	// "untouched".
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
