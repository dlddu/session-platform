//go:build e2e

// 검증 AC: AC-F4
//
// The session-dedicated helper pod, asserted on the *deployed* SUT
// (docs/prd/approval-gated-workload.md AC-F4). AC-F4 is a claim about a pod's
// ownership, its lifetime and the boundary between its two containers. Of the
// four things its verification method names, this file buys everything the
// deployed SUT can currently answer:
//
//  1. dedication — two live sessions get two different helper pods and neither
//     is shared,
//  2. reclaim — deleting a session takes *both* of its pods away, so AC-A3's
//     "resources are recovered" holds across the pair rather than across the
//     workload pod alone,
//  3. lifetime coupling — a *refused* freeze reclaims neither pod, which is the
//     same claim read from the other side: the pair goes away when the session
//     ends, not when someone asks for a snapshot (see the next paragraph),
//  4. isolation — the two helper containers share only the network namespace,
//     so neither can read the other's process environments.
//
// What is deliberately missing, and why. AC-F4 also asks for the *snapshot* half
// of reclaim and for the restore round (a fresh pair, the workload pod rewired
// to the helper made for that restore). Neither is observable here: this type
// has no archive strategy registered, so control-plane/internal/service's
// checkpointerFor returns ErrCheckpointDisabled and the public API answers 503.
// That refusal is intentional and owned elsewhere — see the comment on
// TestSnapshotIsRefusedForApprovalGated in
// control-plane/internal/service/workload_type_test.go, and AC-F5, whose
// filesystem archive is the precondition. Rather than skip past the hole, case 3
// asserts the refusal and its ground truth, so this file turns red the moment
// the precondition lifts; its failure message says what to put back. The two
// branches are registered in docs/test/e2e.md § "남은 미검증 분기 (공백은 아님)".
//
// Why an e2e file at all, when a fake clientset already sees pod specs: the
// in-process suite (control-plane/test/approval_gated_orchestrator_test.go,
// build tag `integration`) owns what a submitted spec looks like — that a
// helper pod is provisioned, how the credentials are split across its two
// containers, that a failed workload pod takes the helper with it, and that a
// restore round builds a fresh pair. None of that is re-bought here. What only
// the deployed cluster can answer is whether the real API server and kubelet
// agree: pods that actually disappear, pods that are actually still standing
// after a refusal, and a PID namespace that is actually not shared — a fake
// clientset has no processes to isolate and so cannot buy branch 4 at all, and
// its unit-level sibling counts survivors without being able to say *which* pod
// survived.
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

// checkpointDisabledMessage is what the product says when a workload type has no
// snapshot strategy (control-plane/internal/session's ErrCheckpointDisabled,
// mapped to 503 by the API). Written out rather than imported so that a change
// to the contract fails this assertion instead of agreeing with it silently.
const checkpointDisabledMessage = "checkpoint strategy is disabled"

// restoreTheFreezeBranches is the instruction this file owes whoever registers
// approval-gated's archive strategy, which is the moment the case below stops
// being true.
const restoreTheFreezeBranches = "AC-F5's archive strategy has landed for approval-gated. " +
	"Delete this case and put AC-F4's own freeze/restore branches back in its place: both pods reclaimed by " +
	"POST /snapshot, and a restore that provisions a fresh pair with the workload pod rewired to this restore's " +
	"helper IP. Then drop the two rows this file owns in docs/test/e2e.md § \"남은 미검증 분기 (공백은 아님)\". " +
	"control-plane/internal/service/workload_type_test.go's TestSnapshotIsRefusedForApprovalGated turns red in " +
	"the same moment and wants the same edit"

// A refused freeze reclaims neither pod. AC-F4 couples the helper pod's lifetime
// to the session's, and this is that coupling read from the side the SUT can
// currently answer: the pair goes away when the session ends (the delete case
// below), and stays when it does not. The snapshot path cannot end an
// approval-gated session yet — no archive strategy is registered for the type,
// so the product refuses rather than reclaiming a pod pair behind a checkpoint
// that cannot restore it.
//
// The refusal itself is already unit-tested; what only the deployed cluster can
// add is *which* pod survived. The unit test counts running pods through a stub
// orchestrator, so a run that reclaimed the helper and kept the workload pod
// would satisfy it. Here the helper is named — selected by its own role label
// and cross-checked against the session's auxiliaryPods.
func TestApprovalGatedHelperPod_RefusedFreezeReclaimsNeitherPod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: what a refused freeze leaves standing is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	getPodEventually(t, cs, ns, s.Pod) // both halves exist before the attempt

	// Not snapshotSession: that helper treats any non-200 as a fatal harness
	// error, and here the non-200 is the contract under test.
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/snapshot", nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("snapshot on an approval-gated session succeeded (body=%s). %s", body, restoreTheFreezeBranches)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("snapshot status=%d body=%s, want %d — the documented refusal for a type with no archive strategy",
			resp.StatusCode, body, http.StatusServiceUnavailable)
	}
	var refusal struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode snapshot refusal: %v body=%s", err, body)
	}
	if refusal.Error != checkpointDisabledMessage {
		t.Fatalf("refusal = %q, want %q — a 503 for some other reason would not tell us the pods are safe",
			refusal.Error, checkpointDisabledMessage)
	}

	// The public answer: nothing moved.
	got := f4Get(t, s.ID)
	if got.State != "active" {
		t.Fatalf("state after a refused freeze = %q, want it unchanged at active", got.State)
	}
	if got.Pod != s.Pod {
		t.Fatalf("workload pod after a refused freeze = %q, want it unchanged at %q", got.Pod, s.Pod)
	}
	stillNamed := false
	for _, name := range got.AuxiliaryPods {
		if name == helper.Name {
			stillNamed = true
		}
	}
	if !stillNamed {
		t.Fatalf("auxiliaryPods after a refused freeze = %v, want it to still name helper pod %q (AC-F4)",
			got.AuxiliaryPods, helper.Name)
	}

	// Then the cluster's, which is the one that costs resources. A refusal that
	// reclaimed the helper anyway would leave the session active and unable to
	// reach its MCP — the exact half-torn state checkpointerFor exists to avoid.
	getPodEventually(t, cs, ns, s.Pod)
	helpers := helperPodsFor(t, cs, ns, s.ID)
	if len(helpers) != 1 {
		t.Fatalf("session %s selects %d helper pods after a refused freeze, want exactly the one it had (AC-F4)",
			s.ID, len(helpers))
	}
	survivor := helpers[0]
	if survivor.Name != helper.Name {
		t.Fatalf("helper pod after a refused freeze = %q, want the original %q — the refusal replaced it (AC-F4)",
			survivor.Name, helper.Name)
	}
	for _, name := range []string{sessionMCPContainer, helperCredProxyContainer} {
		found, ready := containerReady(&survivor, name)
		if !found {
			t.Fatalf("helper pod %s publishes no status for container %q after a refused freeze", survivor.Name, name)
		}
		if !ready {
			t.Fatalf("helper pod %s container %q is not Ready after a refused freeze — the refusal disturbed the pair (AC-F4)",
				survivor.Name, name)
		}
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
