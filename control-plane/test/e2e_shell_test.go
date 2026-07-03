//go:build e2e

// Shell-workload assertions against the kind-deployed SUT
// (docs/test/shell-workload.md):
//   - scenario 1 (AC-D1): a created session's pod runs exactly ONE
//     PTY-attached interactive shell, and the control plane runs none — the
//     half of the AC-D1 split the fake-clientset suite cannot cover (it
//     verifies the pod *spec*; this file verifies the resulting *processes*).
//   - scenario 2 (AC-D2/D3): write injects into the shell's stdin without
//     waiting for the command, read recovers the output.
//   - scenario 3 (AC-D3): read is offset-cursored — a cursor read returns
//     only the delta, offset=0 keeps replaying the full history.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// writeShell posts a payload to the session's write endpoint (AC-D2).
func writeShell(t *testing.T, id, payload string) {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/write", map[string]string{"payload": payload})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status=%d body=%s", resp.StatusCode, body)
	}
}

// readShellAt reads the session's shell output after offset (AC-D3).
func readShellAt(t *testing.T, id string, offset int64) readResp {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/read", map[string]int64{"offset": offset})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read status=%d body=%s", resp.StatusCode, body)
	}
	var r readResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode read: %v body=%s", err, body)
	}
	return r
}

// eventuallyShellRead polls read at offset until ok(payload) holds. Shell
// output timing is non-deterministic (bash prompt, command scheduling), so
// all output assertions are containment + eventually, never exact matches.
func eventuallyShellRead(t *testing.T, id string, offset int64, ok func(string) bool) readResp {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var r readResp
	for {
		r = readShellAt(t, id, offset)
		if ok(r.Payload) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("read at offset %d never matched; last payload=%q", offset, r.Payload)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// Scenario 2 (AC-D2, AC-D3): a command written to the session executes in its
// shell and the output is recovered by read. The $((…)) marker only exists in
// the output once bash actually ran the command — the PTY-echoed input line
// alone cannot contain the computed value. Write must return without waiting
// for the command to finish.
func TestShell_WriteThenReadRecoversOutput(t *testing.T) {
	s := createSession(t, uniqueName(t))

	start := time.Now()
	writeShell(t, s.ID, "sleep 3; echo e2e-marker-$((40+2))\n")
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Fatalf("write blocked for %v on a 3s command — write must not wait for completion (AC-D2)", took)
	}

	r := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "e2e-marker-42")
	})
	if r.NextOffset <= 0 {
		t.Fatalf("nextOffset=%d want > 0 (AC-D3 cursor)", r.NextOffset)
	}
}

// Scenario 3 (AC-D3): reads are offset-cursored deltas. The first read's
// nextOffset yields only output produced after it; offset=0 keeps replaying
// the full ordered history (non-consuming).
func TestShell_ReadCursorDeltaAndFullReplay(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "echo e2e-first-$((40+1))\n")
	first := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "e2e-first-41")
	})

	// Quiet shell: the cursor read must not replay pre-cursor output.
	if d := readShellAt(t, s.ID, first.NextOffset); strings.Contains(d.Payload, "e2e-first-41") {
		t.Fatalf("cursor read replayed old output %q, want delta only (AC-D3)", d.Payload)
	}

	writeShell(t, s.ID, "echo e2e-second-$((40+3))\n")
	delta := eventuallyShellRead(t, s.ID, first.NextOffset, func(p string) bool {
		return strings.Contains(p, "e2e-second-43")
	})
	if strings.Contains(delta.Payload, "e2e-first-41") {
		t.Fatalf("cursor read %q contains pre-cursor output, want only the delta (AC-D3)", delta.Payload)
	}

	// offset=0 replays everything in execution order.
	full := readShellAt(t, s.ID, 0)
	i, j := strings.Index(full.Payload, "e2e-first-41"), strings.Index(full.Payload, "e2e-second-43")
	if i == -1 || j == -1 || i > j {
		t.Fatalf("full read must contain e2e-first-41 then e2e-second-43 in order; payload=%q", full.Payload)
	}
	if full.NextOffset < delta.NextOffset {
		t.Fatalf("full read cursor %d regressed below delta cursor %d", full.NextOffset, delta.NextOffset)
	}
}

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
