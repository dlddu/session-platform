//go:build e2e

// 검증 AC: AC-D1
//
// The session workload IS an interactive shell process (docs/prd/shell-workload.md,
// docs/test/shell-workload.md scenario 1): once a session is active, its
// dedicated pod runs exactly ONE PTY-attached interactive shell — the default
// /bin/bash — and nothing else is attached to a PTY.
//
// This is the half of AC-D1 the fake-clientset unit suite cannot cover: that one
// verifies the pod *spec*, this verifies the resulting *processes*. That the
// control plane hosts no such workload is AC-A1's file.
package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPTYShell_ExactlyOneInteractiveShellInTheSessionPod(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — the runtime shell assertion needs the deployed cluster")
	}
	ns := sessionNamespace()

	// Create returns only after the pod is Ready AND the control plane has
	// reached the shell agent, so the shell must already be up.
	s := createSession(t, uniqueName(t))
	pod := getPodEventually(t, cs, ns, s.Pod)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, stderr, err := execInPod(ctx, cs, cfg, ns, pod.Name, []string{"/bin/bash", "-c", ptyShellProbe})
	if err != nil {
		t.Fatalf("exec PTY probe in session pod %s: %v (stderr=%q)", pod.Name, err, stderr)
	}

	var attached []string
	for _, line := range strings.Split(stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			attached = append(attached, line)
		}
	}
	if len(attached) != 1 {
		t.Fatalf("session pod %s has %d PTY-attached processes %v, want exactly 1 shell (AC-D1)", pod.Name, len(attached), attached)
	}
	if comm := strings.Fields(attached[0])[0]; comm != "bash" {
		t.Fatalf("PTY-attached process is %q, want the interactive shell bash (AC-D1 default /bin/bash)", attached[0])
	}
}
