//go:build e2e

// 검증 AC: AC-A1
//
// docs/prd/architecture.md, docs/test/architecture.md scenario 1.
//
// The control-plane half asserts an absence, so it needs a control: the same exec
// succeeds against a session pod, which e2e_d1_pty_shell_test.go proves.
package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPlaneSeparation_SessionWorkloadRunsOutsideControlPlane(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — plane separation needs the deployed cluster")
	}
	ns := sessionNamespace()

	s := createSession(t, uniqueName(t))
	if s.Pod == "" {
		t.Fatal("API returned an empty pod name for a created session (AC-A1)")
	}
	pod := getPodEventually(t, cs, ns, s.Pod)

	cpPods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=control-plane",
	})
	if err != nil {
		t.Fatalf("list control-plane pods: %v", err)
	}
	if len(cpPods.Items) == 0 {
		t.Fatalf("no control-plane pods labelled app=control-plane in %q", ns)
	}
	for _, cp := range cpPods.Items {
		if cp.Name == pod.Name {
			t.Fatalf("session pod %s IS a control-plane pod — the planes are not separated (AC-A1)", pod.Name)
		}
	}
	if pod.Labels["app"] == "control-plane" {
		t.Fatalf("session pod %s is labelled app=control-plane (AC-A1)", pod.Name)
	}
}

func TestPlaneSeparation_ControlPlaneHostsNoWorkload(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — the control-plane assertion needs the deployed cluster")
	}
	ns := sessionNamespace()

	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=control-plane",
	})
	if err != nil {
		t.Fatalf("list control-plane pods: %v", err)
	}
	if len(pods.Items) == 0 {
		t.Fatalf("no control-plane pods labelled app=control-plane in %q", ns)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		stdout, _, err := execInPod(ctx, cs, cfg, ns, pods.Items[0].Name, []string{sh, "-c", "true"})
		if err == nil {
			t.Fatalf("%s ran inside the control-plane pod (stdout=%q); the control plane must host no session workload (AC-A1)", sh, strings.TrimSpace(stdout))
		}
	}
}
