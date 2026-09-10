//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 6
//
// 이 파일이 무엇을 사고 무엇을 사지 않는지, 그리고 이웃 파일과의 경계는
// docs/test/e2e.md 의 매핑 행과 §「남은 미검증 분기」가 갖는다.
package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The state root the control plane hands the agent (CLAUDE_CODE_STATE_DIR) and the
// workspace inside it; the volume mounts at the state root's parent.
const (
	e5StateVolumePath = "/session"
	e5WorkspaceDir    = "/session/state/workspace"
)

const (
	e5PromptBeforeFreeze = "e5-prompt-before-freeze"
	e5PromptAfterRestore = "e5-prompt-after-restore"
)

func containerSpec(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func containerIsPrivileged(c *corev1.Container) bool {
	return c != nil && c.SecurityContext != nil &&
		c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged
}

func deleteSessionOnCleanup(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+id, nil); resp.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", id, resp.StatusCode, raw)
		}
	})
}

func TestClaudeCodeFreeze_ArchivedTypeNeverCarriesTheCRIUPrivilege(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — the pod-level assertion needs the deployed cluster")
	}
	ns := sessionNamespace()

	shell := createSession(t, uniqueName(t))
	deleteSessionOnCleanup(t, shell.ID)
	shellPod := getPodEventually(t, cs, ns, shell.Pod)
	if !containerIsPrivileged(containerSpec(shellPod, workloadContainer)) {
		t.Skip("this SUT runs with the CRIU gate off, so a shell pod is unprivileged too and " +
			"'not privileged' would discriminate nothing — the deploy/ overlay turns the gate on")
	}

	s := claudeSession(t)
	pod := getPodEventually(t, cs, ns, s.Pod)
	c := containerSpec(pod, workloadContainer)
	if c == nil {
		t.Fatalf("claude-code pod %s has no %q container; containers=%v", pod.Name, workloadContainer, pod.Spec.Containers)
	}
	if containerIsPrivileged(c) {
		t.Fatalf("the claude-code workload container is privileged — this type freezes into a filesystem "+
			"archive and must never be given the in-pod CRIU privilege (AC-E5); securityContext=%+v",
			c.SecurityContext)
	}

	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.MountPath == e5StateVolumePath {
			mounted = true
			break
		}
	}
	if !mounted {
		t.Fatalf("claude-code workload container has no volume mounted at %s — the filesystem archive has "+
			"nothing to capture (AC-E5); mounts=%+v", e5StateVolumePath, c.VolumeMounts)
	}
}

func TestClaudeCodeFreeze_ArchiveRoundTripsWorkspaceHistoryAndResume(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; the workspace-survival assertion needs to exec into the session pods")
	}
	ns := sessionNamespace()
	s := claudeSession(t)
	oldPod := s.Pod

	// Placed by exec, not by a prompt: the stand-in provider cannot run a tool —
	// e2e.md's exception row for this file records that wall.
	nonce := fmt.Sprintf("e5-workspace-%d", time.Now().UnixNano())
	markerPath := e5WorkspaceDir + "/e5-marker.txt"
	execOK(t, cs, cfg, ns, oldPod, "seed the workspace marker",
		fmt.Sprintf("printf '%%s' %q > %q", nonce, markerPath))
	if got := execOK(t, cs, cfg, ns, oldPod, "read back the workspace marker",
		fmt.Sprintf("cat %q", markerPath)); got != nonce {
		t.Fatalf("pre-freeze marker read back as %q, want %q — the probe cannot attest to anything after "+
			"the freeze if it does not work before it", got, nonce)
	}

	writePromptOK(t, s.ID, e5PromptBeforeFreeze)
	eventuallyClaudeOutput(t, s.ID, 5*time.Minute, func(p string) bool {
		return strings.Contains(p, providerReplyMarker)
	})
	before := readShellAt(t, s.ID, 0)
	cursorBefore := before.NextOffset
	if cursorBefore <= 0 {
		t.Fatalf("pre-freeze nextOffset=%d, want > 0 — there is no cursor to carry across the freeze", cursorBefore)
	}
	if got := strings.Count(before.Payload, providerReplyMarker); got != 1 {
		t.Fatalf("pre-freeze buffer carries %d provider replies, want exactly 1; payload=%q", got, before.Payload)
	}

	frozen, ok := snapshotSession(t, s.ID)
	if !ok {
		t.Skip("SUT predates the product snapshot endpoint — the archive round trip is not exercisable here")
	}
	if frozen.State != "snapshot" {
		t.Fatalf("state after snapshot = %q, want snapshot (AC-E5)", frozen.State)
	}
	if frozen.Pod != "" {
		t.Fatalf("pod after snapshot = %q, want it reclaimed (AC-E5 / AC-A3)", frozen.Pod)
	}
	waitPodReclaimed(t, cs, ns, oldPod)

	// Restoring through read rather than write keeps the restore and the next
	// invocation separate events, so the argv probe below can attach to the
	// restored pod *before* any prompt runs in it.
	restored := readShellAt(t, s.ID, 0)
	if restored.Path != "snapshot->restore->read" {
		t.Fatalf("read on the frozen session took path %q, want snapshot->restore->read", restored.Path)
	}
	newPod := restored.Session.Pod
	if restored.Session.State != "active" || newPod == "" {
		t.Fatalf("session after restore = %+v, want state=active with a pod (AC-E5)", restored.Session)
	}
	if newPod == oldPod {
		t.Fatalf("restore reused pod %q — a reclaimed workload comes back in a new pod, and reuse would "+
			"mean nothing was ever reclaimed (AC-E5 / AC-B2)", oldPod)
	}
	if got := strings.Count(restored.Payload, providerReplyMarker); got != 1 {
		t.Fatalf("the restored buffer carries %d provider replies, want the 1 that predates the freeze — "+
			"the output archive is what carries it across (AC-E5); payload=%q", got, restored.Payload)
	}

	if got := execOK(t, cs, cfg, ns, newPod, "read the workspace marker after restore",
		fmt.Sprintf("cat %q", markerPath)); got != nonce {
		t.Fatalf("workspace marker after restore = %q, want %q — the freeze archives the session's files "+
			"and the restore lays them back down in the new pod (AC-E5)", got, nonce)
	}

	probe := startClaudeArgvProbe(t, cs, cfg, ns, newPod)
	time.Sleep(3 * time.Second)
	writePromptOK(t, s.ID, e5PromptAfterRestore)
	eventuallyClaudeOutput(t, s.ID, 5*time.Minute, func(p string) bool {
		return strings.Count(p, providerReplyMarker) >= 2
	})
	argv := probe.invocations(t)
	if len(argv) == 0 {
		t.Fatal("the argv probe saw no invocation in the restored pod, so it can say nothing about --continue")
	}
	if !strings.Contains(argv[0], "--continue") {
		t.Fatalf("the first invocation in the restored pod = %q, want --continue — the conversation state "+
			"is archived with the session and resumed after restore (AC-E5/AC-E4)", argv[0])
	}
	if want := e5PromptAfterRestore; !strings.Contains(argv[0], want) {
		t.Fatalf("the first invocation in the restored pod = %q, want it to carry the prompt %q", argv[0], want)
	}

	// The buffer settles once the reply is in; give the trailing frames of the
	// invocation a moment so the two reads below see the same cursor.
	time.Sleep(5 * time.Second)

	full := readShellAt(t, s.ID, 0)
	if got := strings.Count(full.Payload, providerReplyMarker); got != 2 {
		t.Fatalf("offset=0 after restore carries %d provider replies, want 2 (one from each side of the "+
			"freeze) (AC-E5); payload=%q", got, full.Payload)
	}
	delta := readShellAt(t, s.ID, cursorBefore)
	if full.NextOffset != delta.NextOffset {
		t.Fatalf("the buffer moved between the two reads (full=%d delta=%d); the ordering assertion below "+
			"needs one settled buffer", full.NextOffset, delta.NextOffset)
	}
	if got := strings.Count(delta.Payload, providerReplyMarker); got != 1 {
		t.Fatalf("the delta from the pre-freeze cursor %d carries %d provider replies, want exactly 1 — a "+
			"cursor issued before the freeze must still address only what came after it (AC-E5); payload=%q",
			cursorBefore, got, delta.Payload)
	}
	if !strings.HasSuffix(full.Payload, delta.Payload) {
		t.Fatalf("the delta from cursor %d is not the tail of the offset=0 history — post-freeze output must "+
			"sit after the archived output, not interleaved with it (AC-E5); full=%q delta=%q",
			cursorBefore, full.Payload, delta.Payload)
	}
	if int64(len(full.Payload)-len(delta.Payload)) <= 0 {
		t.Fatalf("offset=0 (%d bytes) is not longer than the post-freeze delta (%d bytes) — the archived "+
			"history is missing from it (AC-E5)", len(full.Payload), len(delta.Payload))
	}
}

// waitPodReclaimed blocks until the pod is gone or accepted for deletion,
// polling past the default 30s termination grace.
func waitPodReclaimed(t *testing.T, cs kubernetes.Interface, ns, name string) {
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
			t.Fatalf("pod %s/%s still present with no deletion after the freeze — a claude-code freeze "+
				"reclaims the pod like any other (AC-E5 / AC-A3)", ns, name)
		}
		time.Sleep(time.Second)
	}
}

func execOK(t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod, what, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, errOut, err := execInContainer(ctx, cs, cfg, ns, pod, workloadContainer, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatalf("%s in %s: %v (stderr=%q)", what, pod, err, errOut)
	}
	return strings.TrimSpace(out)
}

type claudeArgvProbeRun struct {
	done chan struct {
		out    string
		stderr string
		err    error
	}
}

func startClaudeArgvProbe(t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, pod string) *claudeArgvProbeRun {
	t.Helper()
	const probeWindow = 180
	p := &claudeArgvProbeRun{done: make(chan struct {
		out    string
		stderr string
		err    error
	}, 1)}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), (probeWindow+30)*time.Second)
		defer cancel()
		out, errOut, err := execInContainer(ctx, cs, cfg, ns, pod, workloadContainer,
			[]string{"/bin/sh", "-c", claudeArgvProbe, "probe", strconv.Itoa(probeWindow)})
		p.done <- struct {
			out    string
			stderr string
			err    error
		}{out: out, stderr: errOut, err: err}
	}()
	return p
}

func (p *claudeArgvProbeRun) invocations(t *testing.T) []string {
	t.Helper()
	res := <-p.done
	if res.err != nil {
		t.Fatalf("argv probe: %v (stderr=%q)", res.err, res.stderr)
	}
	var observed []string
	for _, line := range strings.Split(res.out, "\n") {
		_, argv, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		if argv = strings.TrimSpace(argv); argv == "" {
			continue
		}
		if len(observed) == 0 || observed[len(observed)-1] != argv {
			observed = append(observed, argv)
		}
	}
	return observed
}
