//go:build e2e

// 검증 AC: AC-F1
//
// docs/prd/approval-gated-workload.md AC-F1, asserted against the kind cluster.
// Why the F series is a 공백 rather than an 예외 is docs/test/e2e.md.
package e2e_test

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Written out rather than imported: this suite asserts the deployed ground
// truth, so it must fail when the control plane's labels drift away from what
// the ledger and the PRD describe — importing the constants would make the
// assertion agree with any rename by construction.
const (
	labelSessionID = "session-id"
	labelPodRole   = "session-platform.dev/pod-role"
	podRoleHelper  = "helper"

	// Named as the deployed pod spec names them, for the same reason.
	sessionMCPContainer      = "session-mcp"
	helperCredProxyContainer = "credential-proxy"
)

// Selecting on the session id *and* the pod role is what makes "the helper pod
// is this session's own" observable — a helper shared across sessions would show
// up here for both.
func helperPodsFor(t *testing.T, cs kubernetes.Interface, ns, sessionID string) []corev1.Pod {
	t.Helper()
	list, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSessionID + "=" + sessionID + "," + labelPodRole + "=" + podRoleHelper,
	})
	if err != nil {
		t.Fatalf("list helper pods for session %s in %s: %v", sessionID, ns, err)
	}
	return list.Items
}

func containerReady(pod *corev1.Pod, name string) (found, ready bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return true, cs.Ready
		}
	}
	return false, false
}

// Create only returns after every pod in the set reports Ready, so a passing
// create already proves the helper pod got its gateway Secret and came up; what
// this test adds is the shape of the set.
func TestApprovalGatedWorkloadType_SessionStandsUpWithItsHelperPod(t *testing.T) {
	status, s := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "approval-gated",
	})
	if status != http.StatusCreated {
		t.Fatalf("create approval-gated session: status=%d", status)
	}
	if s.State != "active" {
		t.Fatalf("state=%q want active", s.State)
	}
	if s.WorkloadType != "approval-gated" {
		t.Fatalf("workloadType=%q want approval-gated", s.WorkloadType)
	}
	if s.Pod == "" {
		t.Fatal("approval-gated session has no dedicated workload pod (AC-A2)")
	}
	if got := getTyped(t, s.ID); got.WorkloadType != "approval-gated" {
		t.Fatalf("workloadType on read back = %q, want approval-gated", got.WorkloadType)
	}

	cs, _, ok := kubeClient(t)
	if !ok {
		return // wire contract asserted; the cluster half needs the deployed cluster
	}
	ns := sessionNamespace()
	getPodEventually(t, cs, ns, s.Pod) // the workload half of the set

	helpers := helperPodsFor(t, cs, ns, s.ID)
	if len(helpers) != 1 {
		names := make([]string, 0, len(helpers))
		for _, p := range helpers {
			names = append(names, p.Name)
		}
		t.Fatalf("session %s has %d helper pods %v, want exactly 1 (AC-F1/AC-F4)", s.ID, len(helpers), names)
	}
	helper := helpers[0]
	if helper.Name == s.Pod {
		t.Fatalf("helper pod and workload pod are the same pod %q — the proxy did not leave the workload pod (AC-F2)", helper.Name)
	}

	for _, name := range []string{sessionMCPContainer, helperCredProxyContainer} {
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
	if n := len(helper.Spec.Containers); n != 2 {
		t.Fatalf("helper pod %s has %d containers %v, want exactly 2 (AC-F4)", helper.Name, n, containerNames(&helper))
	}
}

// Without this, the case above would pass just as well against a control plane
// that gave *every* session a helper pod.
func TestApprovalGatedWorkloadType_OtherTypesGetNoHelperPod(t *testing.T) {
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
				t.Fatalf("%s session %s got %d helper pods, want 0 — the helper pod is not tied to the type", tc.want, s.ID, len(helpers))
			}
		})
	}
}

// The near misses matter here in a way they do not for a settled type:
// `approval-gated` is the newest member of the allow list, so a lenient
// normalisation would show up as one of these passing.
func TestApprovalGatedWorkloadType_NearMissValuesRejectedBeforeProvisioning(t *testing.T) {
	cs, _, hasCluster := kubeClient(t)
	ns := sessionNamespace()
	before := podCount(t, cs, ns, hasCluster)

	for _, bad := range []any{
		"approval_gated", " approval-gated", "approval-gated ", "APPROVAL-GATED", "Approval-Gated",
		"approvalgated", "approval-gate", "", nil, 42, true,
	} {
		status, _ := createTyped(t, map[string]any{"name": uniqueName(t), "workloadType": bad})
		if status != http.StatusBadRequest {
			t.Fatalf("workloadType=%#v status=%d, want 400", bad, status)
		}
	}

	if after := podCount(t, cs, ns, hasCluster); hasCluster && after != before {
		t.Fatalf("session pod count moved %d -> %d across rejected creates — a pod was provisioned", before, after)
	}
}

func TestApprovalGatedWorkloadType_ImmutableAfterCreate(t *testing.T) {
	const model = "approval-gated-e2e-model"
	status, created := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "approval-gated", "model": model,
	})
	if status != http.StatusCreated {
		t.Fatalf("create: status=%d", status)
	}

	for _, path := range []string{"/read", "/write", "/switch"} {
		resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+created.ID+path, map[string]any{
			"workloadType": "shell", "model": "platform-default", "payload": "x",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("post %s status=%d want 400 body=%s", path, resp.StatusCode, body)
		}
	}

	got := getTyped(t, created.ID)
	if got.WorkloadType != "approval-gated" {
		t.Fatalf("workloadType after mutating calls = %q, want approval-gated", got.WorkloadType)
	}
	if got.Model != model {
		t.Fatalf("model after mutating calls = %q, want %q", got.Model, model)
	}
}
