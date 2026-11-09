//go:build e2e

// 검증 시나리오: approval-gate.md#시나리오 7
//
// docs/prd/approval-gate.md AC-F4, asserted on the deployed SUT with a session of
// the gated type (harness_approval_gate_test.go). The freeze and restore
// branches need the deploy/overlays/e2e/ overlay's MINIMAX_CODE_ARCHIVE_ENABLED, which
// registers that type's archive strategy.
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

// `auxiliaryPods` is the public name for a session's session-scoped pods, and it
// is what makes the pair observable from outside the cluster.
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
		"name": uniqueName(t), "workloadType": gatedWorkloadType,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s session: status=%d body=%s", gatedWorkloadType, resp.StatusCode, raw)
	}
	var s f4Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode created session: %v body=%s", err, raw)
	}
	if s.WorkloadType != gatedWorkloadType {
		t.Fatalf("workloadType=%q want %s", s.WorkloadType, gatedWorkloadType)
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

// Two independent sources naming the same pod is what rules out a helper the
// control plane forgot to record — or a record naming a pod that was never
// created.
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

// The window clears the k8s default 30s termination grace, same as AC-A3's file.
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

// Values go in as positional arguments rather than being spliced into the
// command line.
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

// The own-marker count is the control: it must hit, or a zero neighbour count
// would say nothing more than "/proc is unreadable here".
//
// $1 is this container's marker and $2 the neighbour's.
const f4NeighbourProbe = `printf 'own=%s\n' "$(grep -lF "$1" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"
printf 'neighbour=%s\n' "$(grep -lF "$2" /proc/[0-9]*/environ 2>/dev/null | wc -l | tr -d ' ')"`

// Read from the running container rather than written here, so the probe below
// keeps meaning what it says if the deployment renames a workload.
func f4WorkloadMarker(t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod, container string) string {
	t.Helper()
	got := strings.TrimSpace(f4Sh(t, cs, cfg, ns, pod, container, `printf %s "$DATA_PLANE_WORKLOAD"`))
	if got == "" {
		t.Fatalf("container %q of %s/%s has no DATA_PLANE_WORKLOAD; the isolation probe has no marker to look for", container, ns, pod)
	}
	return "DATA_PLANE_WORKLOAD=" + got
}

// A helper shared between sessions would show up under both selectors; one
// mislabelled would show up under neither.
func TestApprovalGateHelperPod_IsDedicatedToOneSession(t *testing.T) {
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

// A freeze reclaims the pair together, and the restore provisions a fresh pair
// whose workload pod reaches this round's helper rather than the reclaimed one.
func TestApprovalGateHelperPod_FreezeReclaimsBothPodsAndRestoreRewiresAFreshPair(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: reclaim and rewiring are claims about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	getPodEventually(t, cs, ns, s.Pod)

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the freeze branch is not exercisable here")
	}
	frozen := f4Get(t, s.ID)
	if frozen.State != "snapshot" || frozen.Pod != "" || len(frozen.AuxiliaryPods) != 0 {
		t.Fatalf("after freeze = %+v, want state=snapshot with no pods recorded (AC-F4)", frozen)
	}
	f4AwaitReclaimed(t, cs, ns, s.Pod, "the freeze")
	f4AwaitReclaimed(t, cs, ns, helper.Name, "the freeze")

	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+s.ID+"/read", map[string]int64{"offset": 0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restoring read: status=%d body=%s", resp.StatusCode, body)
	}
	restored := f4Get(t, s.ID)
	if restored.State != "active" || restored.Pod == "" || restored.Pod == s.Pod {
		t.Fatalf("after restore = %+v, want active on a new workload pod (old %q) (AC-F4)", restored, s.Pod)
	}
	// The reclaimed helper may still be terminating, so only live pods count.
	var live []corev1.Pod
	for _, p := range helperPodsFor(t, cs, ns, s.ID) {
		if p.DeletionTimestamp == nil {
			live = append(live, p)
		}
	}
	if len(live) != 1 || len(restored.AuxiliaryPods) != 1 || restored.AuxiliaryPods[0] != live[0].Name {
		t.Fatalf("after restore: live helper pods %d, auxiliaryPods=%v — want exactly one, named by the session (AC-F4)",
			len(live), restored.AuxiliaryPods)
	}
	newHelper := live[0]
	if newHelper.Name == helper.Name {
		t.Fatalf("restore reused helper pod %q; a restore provisions a fresh pair (AC-F4)", helper.Name)
	}
	workload := getPodEventually(t, cs, ns, restored.Pod)
	c := containerSpec(workload, workloadContainer)
	if c == nil {
		t.Fatalf("restored workload pod %s has no %q container", workload.Name, workloadContainer)
	}
	var mcpURL string
	for _, e := range c.Env {
		if e.Name == "SESSION_MCP_URL" {
			mcpURL = e.Value
		}
	}
	if newHelper.Status.PodIP == "" || !strings.Contains(mcpURL, newHelper.Status.PodIP) {
		t.Fatalf("restored workload SESSION_MCP_URL=%q, want this round's helper IP %q (AC-F4)",
			mcpURL, newHelper.Status.PodIP)
	}
}

// The freeze path leaves a snapshot behind and the delete path does not, so they
// reach reclamation differently; the helper pod has to go on both.
func TestApprovalGateHelperPod_DeleteReclaimsBothPods(t *testing.T) {
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

// This is the property AC-F4 leans on when it puts both of the platform's
// external secrets in one pod: the isolation that keeps them apart is the
// container boundary, not the pod boundary.
func TestApprovalGateHelperPod_ContainersDoNotSharePIDNamespace(t *testing.T) {
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

// Create only returns after every pod in the set reports Ready, so a passing
// create already proves the helper pod got its gateway token and came up; what
// this test adds is the shape of the set (AC-F4).
func TestApprovalGateHelperPod_SessionStandsUpWithItsHelperPod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: the helper pod's shape is a claim about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	if s.State != "active" {
		t.Fatalf("state=%q want active", s.State)
	}
	getPodEventually(t, cs, ns, s.Pod) // the workload half of the set

	helper := f4TheHelperPod(t, cs, ns, s)
	if helper.Name == s.Pod {
		t.Fatalf("helper pod and workload pod are the same pod %q — the proxy did not leave the workload pod (AC-F2)", helper.Name)
	}
	for _, name := range gatedHelperContainers {
		if _, found := containerByName(&helper, name); !found {
			t.Fatalf("helper pod %s has no %q container: %v", helper.Name, name, containerNames(&helper))
		}
		found, ready := containerReady(&helper, name)
		if !found {
			t.Fatalf("helper pod %s publishes no status for container %q", helper.Name, name)
		}
		if !ready {
			t.Fatalf("helper pod %s container %q is not Ready — create returned before the pod set was up", helper.Name, name)
		}
	}
	if n := len(helper.Spec.Containers); n != len(gatedHelperContainers) {
		t.Fatalf("helper pod %s has %d containers %v, want exactly %v", helper.Name, n, containerNames(&helper), gatedHelperContainers)
	}
}

// Without this, the case above would pass just as well against a control plane
// that gave *every* session a helper pod: the helper pod belongs to the gate,
// not to sessions in general.
func TestApprovalGateHelperPod_UngatedTypesGetNoHelperPod(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: the helper-pod-absence assertion needs the deployed cluster")
	}
	ns := sessionNamespace()

	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{name: "omitted", body: map[string]any{}, want: "shell"},
		{name: "explicit shell", body: map[string]any{"workloadType": "shell"}, want: "shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"name": uniqueName(t)}
			for k, v := range tc.body {
				body[k] = v
			}
			status, s := createTyped(t, body)
			if status != http.StatusCreated {
				t.Fatalf("create: status=%d", status)
			}
			if s.WorkloadType != tc.want {
				t.Fatalf("workloadType=%q want %q", s.WorkloadType, tc.want)
			}
			if helpers := helperPodsFor(t, cs, ns, s.ID); len(helpers) != 0 {
				t.Fatalf("%s session %s got %d helper pods, want 0 — the helper pod is not tied to the gate", tc.want, s.ID, len(helpers))
			}
		})
	}
}
