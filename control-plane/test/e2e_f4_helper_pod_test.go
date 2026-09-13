//go:build e2e

// 검증 시나리오: approval-gated-workload.md#시나리오 7
//
// docs/prd/approval-gated-workload.md AC-F4, asserted on the deployed SUT.
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

const f4ReadSessionMCPURL = `printf %s "${SESSION_MCP_URL-}"`

func f4Switch(t *testing.T, id string) f4Session {
	t.Helper()
	resp, raw := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/switch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch %s: status=%d body=%s", id, resp.StatusCode, raw)
	}
	var s f4Session
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode switch %s: %v body=%s", id, err, raw)
	}
	return s
}

// A helper shared between sessions would show up under both selectors; one
// mislabelled would show up under neither.
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

func TestApprovalGatedHelperPod_FreezeReclaimsBothPodsAndRestoreRewiresTheNewPair(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster: what a freeze reclaims and what a restore provisions are claims about deployed pods")
	}
	ns := sessionNamespace()

	s := f4Create(t)
	helper := f4TheHelperPod(t, cs, ns, s)
	getPodEventually(t, cs, ns, s.Pod) // both halves exist before the freeze
	// Read before the freeze so that comparing it after the restore makes
	// "rewired" a measured fact rather than an assumption about pod recreation.
	mcpBefore := strings.TrimSpace(f4Sh(t, cs, cfg, ns, s.Pod, workloadContainer, f4ReadSessionMCPURL))
	if mcpBefore == "" {
		t.Fatal("workload container has no SESSION_MCP_URL before the freeze; there is nothing to see rewired (AC-F4)")
	}

	frozen, found := snapshotSession(t, s.ID)
	if !found {
		t.Fatal("snapshot endpoint reported the session missing")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after a freeze = %q, want snapshot (AC-B1)", frozen.State)
	}

	// AC-A3's reclaim is only whole when both pods are gone.
	afterFreeze := f4Get(t, s.ID)
	if afterFreeze.Pod != "" {
		t.Fatalf("workload pod after a freeze = %q, want it released (AC-A3)", afterFreeze.Pod)
	}
	if len(afterFreeze.AuxiliaryPods) != 0 {
		t.Fatalf("auxiliaryPods after a freeze = %v, want none — the helper is reclaimed with the workload (AC-F4)",
			afterFreeze.AuxiliaryPods)
	}
	f4AwaitReclaimed(t, cs, ns, s.Pod, "the freeze")
	f4AwaitReclaimed(t, cs, ns, helper.Name, "the freeze")
	if helpers := helperPodsFor(t, cs, ns, s.ID); len(helpers) != 0 {
		for _, h := range helpers {
			if h.DeletionTimestamp == nil {
				t.Fatalf("helper pod %s is still standing after the freeze — the pair was not reclaimed together (AC-F4)", h.Name)
			}
		}
	}

	restored := f4Switch(t, s.ID)
	if restored.State != "active" {
		t.Fatalf("state after accessing a frozen session = %q, want active (AC-B2)", restored.State)
	}
	if restored.Pod == "" || restored.Pod == s.Pod {
		t.Fatalf("restored workload pod = %q, want a new one (the freeze reclaimed %q) (AC-B2)", restored.Pod, s.Pod)
	}
	newHelper := f4TheHelperPod(t, cs, ns, restored)
	if newHelper.Name == helper.Name {
		t.Fatalf("restored onto helper pod %q, the one the freeze reclaimed — the restore round gets its own (AC-F4)", helper.Name)
	}
	if newHelper.Status.PodIP == "" {
		t.Fatalf("restored helper pod %s has no pod IP; the workload cannot have been given one (AC-F4)", newHelper.Name)
	}
	for _, name := range []string{sessionMCPContainer, helperCredProxyContainer} {
		found, ready := containerReady(&newHelper, name)
		if !found || !ready {
			t.Fatalf("restored helper pod %s container %q found=%v ready=%v — the restore round came up half-formed (AC-F4)",
				newHelper.Name, name, found, ready)
		}
	}

	// A workload still holding the reclaimed helper's address would satisfy
	// every assertion above and reach nothing.
	mcpAfter := strings.TrimSpace(f4Sh(t, cs, cfg, ns, restored.Pod, workloadContainer, f4ReadSessionMCPURL))
	if !strings.Contains(mcpAfter, newHelper.Status.PodIP) {
		t.Fatalf("restored workload was given SESSION_MCP_URL=%q, which does not name this round's helper IP %q (AC-F4)",
			mcpAfter, newHelper.Status.PodIP)
	}
	if mcpAfter == mcpBefore {
		t.Fatalf("restored workload still holds the pre-freeze SESSION_MCP_URL %q — it is pointed at a reclaimed pod (AC-F4)", mcpBefore)
	}
}

// The freeze path leaves a snapshot behind and the delete path does not, so they
// reach reclamation differently; the helper pod has to go on both.
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

// This is the property AC-F4 leans on when it puts both of the platform's
// external secrets in one pod: the isolation that keeps them apart is the
// container boundary, not the pod boundary.
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
