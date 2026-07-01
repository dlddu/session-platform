//go:build e2e

// AC-D1 runtime assertions against the kind-deployed SUT (scenario 1 in
// docs/test/shell-workload.md): a created session's pod runs exactly ONE
// PTY-attached interactive shell, and the control plane runs none. This is the
// half of the AC-D1 split the fake-clientset suite cannot cover — it verifies
// the pod *spec* (client_orchestrator_test.go); this file verifies the
// resulting *processes* in a real cluster.
package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// execInPod runs command in the pod's (single) container via the exec
// subresource. Only this e2e runner uses pods/exec, authorised by its own
// kubeconfig — the control plane never execs into pods (it dials the session
// agent over the network), so its RBAC stays exec-free.
func execInPod(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod string, command []string) (string, string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("build exec executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), err
}

// ptyShellProbe prints "comm tty" for every process in the pod whose stdin is
// a PTY slave — i.e. the PTY-attached processes. The probe itself is exec'd
// without a TTY (stdin is a pipe/null), so neither it nor its command-
// substitution subshells ever match.
const ptyShellProbe = `for d in /proc/[0-9]*; do
  tty=$(readlink "$d/fd/0" 2>/dev/null) || continue
  case "$tty" in /dev/pts/*) echo "$(cat "$d/comm" 2>/dev/null) $tty" ;; esac
done`

// AC-D1: once a session is active, its dedicated pod contains exactly one
// PTY-attached interactive shell — the default /bin/bash — and nothing else is
// attached to a PTY.
func TestShell_ExactlyOnePTYShellInSessionPod(t *testing.T) {
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

// AC-D1: the control plane only orchestrates — it runs no shell. Its distroless
// image ships no shell binary at all, so exec'ing one must fail (while the same
// exec works against a session pod, as the test above proves).
func TestShell_ControlPlaneRunsNoShell(t *testing.T) {
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
			t.Fatalf("%s ran inside the control-plane pod (stdout=%q); the control plane must host no shell (AC-D1)", sh, stdout)
		}
	}
}
