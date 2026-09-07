//go:build e2e

// 검증 AC: AC-F4
//
// The session-dedicated helper pod, asserted on the *deployed* SUT
// (docs/prd/approval-gated-workload.md AC-F4). AC-F4 is a claim about a pod's
// ownership, its lifetime and the boundary between its two containers, and its
// verification method names four things this file buys in that order:
//
//  1. dedication — two live sessions get two different helper pods and neither
//     is shared,
//  2. reclaim — freezing or deleting a session takes *both* of its pods away,
//     so AC-A3's "resources are recovered" holds across the pair rather than
//     across the workload pod alone,
//  3. restore — a restored session comes back as a fresh pair, and its workload
//     pod is wired to the helper pod made for *that* restore rather than to the
//     address of the one the freeze threw away,
//  4. isolation — the two helper containers share only the network namespace,
//     so neither can read the other's process environments.
//
// Why an e2e file at all, when a fake clientset already sees pod specs: the
// in-process suite (control-plane/test/approval_gated_orchestrator_test.go,
// build tag `integration`) owns what a submitted spec looks like — that a
// helper pod is provisioned, how the credentials are split across its two
// containers, that a failed workload pod takes the helper with it, and that a
// restore round builds a fresh pair. None of that is re-bought here. What only
// the deployed cluster can answer is whether the real API server and kubelet
// agree: pods that actually disappear, an address that actually points at the
// new pod, and a PID namespace that is actually not shared — a fake clientset
// has no processes to isolate and so cannot buy branch 4 at all.
//
// Neighbours this file also does not re-buy: AC-F1 owns the type axis and the
// shape of the set it provisions (one helper pod, two containers, both Ready);
// AC-F2 owns the network boundary; AC-F6 owns which credential lands in which
// container. This file takes the helper pod as given and asks who it belongs
// to, how long it lives, and what its two halves can see of each other.
package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// sessionMCPURLEnvVar is how the workload pod learns where its session MCP is.
// Written out rather than imported for the same reason e2e_f1's labels are: the
// assertion must fail when the deployment stops injecting it, not agree with a
// rename by construction.
const sessionMCPURLEnvVar = "SESSION_MCP_URL"

// f4Session is the wire view this file needs. `auxiliaryPods` is the public
// name for a session's session-scoped pods, and it is what makes the pair
// observable from outside the cluster — the cluster assertions below check that
// the API's answer and the API server's answer are the same answer.
type f4Session struct {
	ID            string   `json:"id"`
	State         string   `json:"state"`
	Pod           string   `json:"pod"`
	WorkloadType  string   `json:"workloadType"`
	AuxiliaryPods []string `json:"auxiliaryPods"`
}

func f4Create(t *testing.T) f4Session {
	t.Helper()
	resp, raw := do(t, http.MethodPost, "/api/v1/sessions", map[string]any{
		"name": uniqueName(t), "workloadType": "approval-gated",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create approval-gated session: status=%d body=%s", resp.StatusCode, raw)
	}
	var s f4Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode created session: %v body=%s", err, raw)
	}
	if s.WorkloadType != "approval-gated" {
		t.Fatalf("workloadType=%q want approval-gated", s.WorkloadType)
	}
	if s.Pod == "" {
		t.Fatal("created session has no workload pod (AC-A2)")
	}
	return s
}

func f4Get(t *testing.T, id string) f4Session {
	t.Helper()
	resp, raw := do(t, http.MethodGet, "/api/v1/sessions/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status=%d body=%s", id, resp.StatusCode, raw)
	}
	var s f4Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode session %s: %v body=%s", id, err, raw)
	}
	return s
}

// f4TheHelperPod is the session's one helper pod, cross-checked against the
// public API. Two independent sources naming the same pod is what rules out a
// helper the control plane forgot to record — or a record naming a pod that was
// never created.
func f4TheHelperPod(t *testing.T, cs kubernetes.Interface, ns string, s f4Session) corev1.Pod {
	t.Helper()
	helpers := helperPodsFor(t, cs, ns, s.ID)
	if len(helpers) != 1 {
		t.Fatalf("session %s has %d helper pods, want exactly 1 (AC-F4)", s.ID, len(helpers))
	}
	if len(s.AuxiliaryPods) != 1 || s.AuxiliaryPods[0] != helpers[0].Name {
		t.Fatalf("session %s reports auxiliaryPods=%v but the cluster's helper pod is %q (AC-F4)",
			s.ID, s.AuxiliaryPods, helpers[0].Name)
	}
	return helpers[0]
}

// f4AwaitReclaimed blocks until the pod is gone or accepted for deletion. The
// window clears the default 30s termination grace, same as AC-A3's file.
func f4AwaitReclaimed(t *testing.T, cs kubernetes.Interface, ns, name, why string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get pod %s/%s: %v", ns, name, err)
		}
		if pod.DeletionTimestamp != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s/%s is still present with no deletion after %s — the pair was not reclaimed together (AC-F4)",
				ns, name, why)
		}
		time.Sleep(time.Second)
	}
}

// f4Sh runs a /bin/sh script in one container of a pod, passing values as
// positional arguments rather than splicing them into the command line.
func f4Sh(t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod, container, script string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := append([]string{"/bin/sh", "-c", script, "sh"}, args...)
	stdout, stderr, err := execInContainer(ctx, cs, cfg, ns, pod, container, command)
	if err != nil {
		t.Fatalf("exec in %s/%s [%s]: %v (stdout=%q stderr=%q)", ns, pod, container, err, stdout, stderr)
	}
	return stdout
}

// f4NeighbourProbe counts the process environments in this container that carry
// its own workload marker and those that carry the neighbour container's. The
// own count is the control: it must hit, or a zero neighbour count would say
// nothing more than "/proc is unreadable here".
//
// $1 is this container's marker and $2 the neighbour's.
const f4NeighbourProbe = `printf 'own=%s\n' "$(grep -lF "$1" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"
printf 'neighbour=%s\n' "$(grep -lF "$2" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"`

// f4WorkloadMarker reads the container's own DATA_PLANE_WORKLOAD value. Read
// from the running container rather than written here so the probe below keeps
// meaning what it says if the deployment renames a workload.
func f4WorkloadMarker(t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod, container string) string {
	t.Helper()
	got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, pod, container, `printf %s "$DATA_PLANE_WORKLOAD"`))
	if got == "" {
		t.Fatalf("container %q of %s/%s has no DATA_PLANE_WORKLOAD; the isolation probe has no marker to look for", container, ns, pod)
	}
	return "DATA_PLANE_WORKLOAD=" + got
}

// Two live sessions get two different helper pods, and each helper carries only
// its own session's identifier. A helper shared between sessions would show up
// under both selectors; one mislabelled would show up under neither.
func TestApprovalGatedHelperPod_IsDedicatedToOneSession(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: helper pod dedication is a claim about deployed pods")
	}
	ns := sessionNamespace()

	first, second := f4Create(t), f4Create(t)
	if first.ID == second.ID {
		t.Fatal("the two sessions share an id; they cannot be told apart")
	}

	helperOne := f4TheHelperPod(t, cs, ns, first)
	helperTwo := f4TheHelperPod(t, cs, ns, second)

	if helperOne.Name == helperTwo.Name {
		t.Fatalf("both sessions are served by helper pod %q — the helper is shared, not session-dedicated (AC-F4)", helperOne.Name)
	}
	for _, tc := range []struct {
		helper  corev1.Pod
		own     f4Session
		foreign f4Session
	}{
		{helperOne, first, second},
		{helperTwo, second, first},
	} {
		if got := tc.helper.Labels[labelSessionID]; got != tc.own.ID {
			t.Fatalf("helper pod %s carries %s=%q, want its own session %q (AC-F4)",
				tc.helper.Name, labelSessionID, got, tc.own.ID)
		}
		if tc.helper.Name == tc.foreign.Pod || tc.helper.Name == tc.own.Pod {
			t.Fatalf("helper pod %s is also a session's workload pod; the helper runs no session workload (AC-F4)", tc.helper.Name)
		}
	}

	// The selector really discriminates rather than matching everything: asking
	// for one session's helper must not return the other's.
	for _, s := range []f4Session{first, second} {
		helpers := helperPodsFor(t, cs, ns, s.ID)
		if len(helpers) != 1 {
			t.Fatalf("session %s selects %d helper pods, want exactly its own", s.ID, len(helpers))
		}
	}
}

// Freezing a session reclaims the pair. AC-A3's file already owns the workload
// half; what AC-F4 adds is that the helper pod goes with it — a helper left
// running would keep the session's real occupancy above zero while the API
// reported it frozen.
func TestApprovalGatedHelperPod_FreezeReclaimsBothPods(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: reclaim is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	getPodEventually(t, cs, ns, s.Pod) // both halves exist before the freeze

	frozen, ok := snapshotSession(t, s.ID)
	if !ok {
		t.Skip("SUT predates the product snapshot endpoint — the freeze path is not exercisable here")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after snapshot = %q, want snapshot", frozen.State)
	}

	// The public answer first: a frozen session names neither pod.
	got := f4Get(t, s.ID)
	if got.Pod != "" {
		t.Fatalf("pod after freeze = %q, want it reclaimed (AC-A3)", got.Pod)
	}
	if len(got.AuxiliaryPods) != 0 {
		t.Fatalf("auxiliaryPods after freeze = %v, want none — the helper pod is not snapshot state (AC-F4)", got.AuxiliaryPods)
	}

	// Then the cluster's, which is the one that costs resources.
	f4AwaitReclaimed(t, cs, ns, s.Pod, "the freeze")
	f4AwaitReclaimed(t, cs, ns, helper.Name, "the freeze")
	if helpers := helperPodsFor(t, cs, ns, s.ID); len(helpers) != 0 {
		names := make([]string, 0, len(helpers))
		for _, p := range helpers {
			names = append(names, p.Name)
		}
		t.Fatalf("session %s still selects helper pods %v after the freeze (AC-F4)", s.ID, names)
	}
}

// Deleting a session reclaims the same pair. The freeze path leaves a snapshot
// behind and the delete path does not, so they reach reclamation differently;
// AC-F4 requires the helper pod to go on both.
func TestApprovalGatedHelperPod_DeleteReclaimsBothPods(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: reclaim is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	getPodEventually(t, cs, ns, s.Pod)

	resp, body := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("delete session %s: status=%d body=%s", s.ID, resp.StatusCode, body)
	}

	f4AwaitReclaimed(t, cs, ns, s.Pod, "the delete")
	f4AwaitReclaimed(t, cs, ns, helper.Name, "the delete")
}

// A restored session comes back as a fresh pair, and its new workload pod is
// told where the *new* helper pod is. Carrying the pre-freeze address forward
// would leave the agent pointed at a pod that no longer exists, which no
// assertion about pod names alone would catch.
func TestApprovalGatedHelperPod_RestoreProvisionsAFreshPairAndRewiresTheWorkloadPod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: the restored pair is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helperBefore := f4TheHelperPod(t, cs, ns, s)
	ipBefore := helperBefore.Status.PodIP
	if ipBefore == "" {
		t.Fatalf("helper pod %s reports no pod IP; the workload pod could not have been wired to it", helperBefore.Name)
	}

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the restore path is not exercisable here")
	}

	// Access = switch, the same door AC-B2's file uses.
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/switch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch on a snapshot session: status=%d body=%s", resp.StatusCode, body)
	}
	restored := f4Get(t, s.ID)
	if restored.State != "active" {
		t.Fatalf("state after restore = %q, want active (AC-B2)", restored.State)
	}
	if restored.Pod == "" || restored.Pod == s.Pod {
		t.Fatalf("restored workload pod = %q, want a new one (was %q)", restored.Pod, s.Pod)
	}

	helperAfter := f4TheHelperPod(t, cs, ns, restored)
	if helperAfter.Name == helperBefore.Name {
		t.Fatalf("restore reused helper pod %q; the freeze reclaimed it, so restore must provision a new one (AC-F4)", helperBefore.Name)
	}
	ipAfter := helperAfter.Status.PodIP
	if ipAfter == "" {
		t.Fatalf("restored helper pod %s reports no pod IP", helperAfter.Name)
	}

	// The wiring, read off the pod the API server admitted.
	workload := getPodEventually(t, cs, ns, restored.Pod)
	main, found := containerByName(workload, workloadContainer)
	if !found {
		t.Fatalf("restored workload pod %s has no %q container: %v", workload.Name, workloadContainer, containerNames(workload))
	}
	var mcpURL string
	for _, env := range main.Env {
		if env.Name == sessionMCPURLEnvVar {
			mcpURL = env.Value
		}
	}
	if mcpURL == "" {
		t.Fatalf("restored workload pod %s has no %s; it cannot reach the helper pod made for this restore (AC-F4)",
			workload.Name, sessionMCPURLEnvVar)
	}
	if !strings.Contains(mcpURL, ipAfter) {
		t.Fatalf("%s = %q on the restored workload pod, but this restore's helper pod is at %s (AC-F4)",
			sessionMCPURLEnvVar, mcpURL, ipAfter)
	}
	if ipAfter != ipBefore && strings.Contains(mcpURL, ipBefore) {
		t.Fatalf("%s = %q still points at the pre-freeze helper pod (%s), which the freeze reclaimed (AC-F4)",
			sessionMCPURLEnvVar, mcpURL, ipBefore)
	}
}

// The two helper containers do not share a PID namespace, so neither can read
// the other's process environments. This is the property AC-F4 leans on when it
// puts both of the platform's external secrets in one pod: the isolation that
// keeps them apart is the container boundary, not the pod boundary.
//
// The probe is symmetric and self-validating — each container must find its own
// marker (or its zero for the neighbour would only mean /proc was unreadable)
// and must not find the other's.
func TestApprovalGatedHelperPod_ContainersDoNotSharePIDNamespace(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: namespace isolation needs real processes")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)

	// Opting the pod into a shared PID namespace would defeat the boundary
	// before any probe ran, so say so in those terms.
	if helper.Spec.ShareProcessNamespace != nil && *helper.Spec.ShareProcessNamespace {
		t.Fatalf("helper pod %s shares its PID namespace; each container's environment would be readable from the other (AC-F4)", helper.Name)
	}

	mcpMarker := f4WorkloadMarker(t, cs, cfg, ns, helper.Name, sessionMCPContainer)
	proxyMarker := f4WorkloadMarker(t, cs, cfg, ns, helper.Name, helperCredProxyContainer)
	if mcpMarker == proxyMarker {
		t.Fatalf("both helper containers report %s; the probe cannot tell them apart and would pass vacuously", mcpMarker)
	}

	for _, tc := range []struct {
		container string
		own       string
		neighbour string
	}{
		{sessionMCPContainer, mcpMarker, proxyMarker},
		{helperCredProxyContainer, proxyMarker, mcpMarker},
	} {
		probe := e6ProbeFields(t, f4Sh(t, cs, cfg, ns, helper.Name, tc.container, f4NeighbourProbe, tc.own, tc.neighbour))
		if probe["own"] == "" || probe["own"] == "0" {
			t.Fatalf("container %q found its own marker %q in no process environment, so it cannot read /proc here and its neighbour=%q result proves nothing",
				tc.container, tc.own, probe["neighbour"])
		}
		if probe["neighbour"] != "0" {
			t.Fatalf("container %q can read %s process environments carrying %q — the two helper containers are not PID-isolated (AC-F4)",
				tc.container, probe["neighbour"], tc.neighbour)
		}
	}
}
