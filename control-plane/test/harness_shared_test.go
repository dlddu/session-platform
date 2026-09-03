//go:build e2e

// Package e2e_test drives the *deployed* control-plane SUT over HTTP (build tag
// `e2e`, run via `make e2e-up && go test -tags=e2e ./test/...`).
//
// Unlike integration_test.go (which mounts the handlers in-process), this suite
// is a black box: it only knows the wire contract and talks to whatever
// E2E_BASE_URL points at.
//
// LAYOUT — one AC per file. THIS file is deliberately NOT named `e2e_*`: it holds
// the shared harness only, so it is not a matching unit and needs no AC
// declaration.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// Creating a session provisions a real pod and waits for it to report Ready
// (the in-cluster client-go orchestrator), so the create calls can take longer
// than a stub round-trip — the timeout has headroom for image pull + schedule.
var client = &http.Client{Timeout: 90 * time.Second}

func baseURL() string {
	if v := os.Getenv("E2E_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8080"
}

// Session mirrors the JSON the API emits. It is declared locally so the e2e suite
// asserts the wire contract independently of the internal domain types.
type session struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Pod        string `json:"pod"`
	CreatedAt  string `json:"createdAt"`
	LastAccess string `json:"lastAccess"`
}

type readResp struct {
	Session    session `json:"session"`
	Path       string  `json:"path"`
	Payload    string  `json:"payload"`
	NextOffset int64   `json:"nextOffset"`
}

type writeResp struct {
	Session session `json:"session"`
	Path    string  `json:"path"`
}

// Do performs a request against the SUT, failing the test on transport errors.
func do(t *testing.T, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL()+path, r)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v (is the SUT up? run `make e2e-up` or set E2E_BASE_URL)", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s %s: %v", method, path, err)
	}
	return resp, out
}

// uniqueName derives a collision-free session name from the test name; the SUT
// state is shared across runs, so names must be unique.
func uniqueName(t *testing.T) string {
	t.Helper()
	base := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func createSession(t *testing.T, name string) session {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions", map[string]string{"name": name})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %q: status=%d body=%s", name, resp.StatusCode, body)
	}
	var s session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decode created session: %v body=%s", err, body)
	}
	return s
}

func getSession(t *testing.T, id string) session {
	t.Helper()
	resp, body := do(t, http.MethodGet, "/api/v1/sessions/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: status=%d body=%s", id, resp.StatusCode, body)
	}
	var s session
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("decode session: %v body=%s", err, body)
	}
	return s
}

// SnapshotSession freezes a session through the product snapshot endpoint, which
// gives this suite a deterministic way to reach the snapshot state without
// waiting out the idle window. It reports ok=false when the SUT predates the
// endpoint, so callers skip instead of failing.
func snapshotSession(t *testing.T, id string) (session, bool) {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/snapshot", nil)
	if resp.StatusCode == http.StatusNotFound {
		return session{}, false
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", resp.StatusCode, body)
	}
	var frozen session
	if err := json.Unmarshal(body, &frozen); err != nil {
		t.Fatalf("decode snapshot response: %v body=%s", err, body)
	}
	return frozen, true
}

// sessionNamespace is where the deployed control plane provisions its data plane
// pods — the same namespace it runs in. Overridable for other cluster layouts.
func sessionNamespace() string {
	if v := os.Getenv("E2E_SESSION_NAMESPACE"); v != "" {
		return v
	}
	return "default"
}

// kubeClient builds a client for the cluster the SUT runs in, from the ambient
// kubeconfig or the in-cluster config, plus the rest config for exec streams. It
// reports ok=false when neither is available, so a run pointed at a non-cluster
// SUT skips rather than fails.
func kubeClient(t *testing.T) (kubernetes.Interface, *rest.Config, bool) {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		if cfg, err = rest.InClusterConfig(); err != nil {
			return nil, nil, false
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, false
	}
	return cs, cfg, true
}

// getPodEventually fetches a pod, tolerating brief API eventual-consistency.
// Create returns only after the pod is Ready, so it should already exist.
func getPodEventually(t *testing.T, cs kubernetes.Interface, ns, name string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		pod, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			return pod
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("pod %s/%s not found within timeout: %v", ns, name, lastErr)
			return nil
		}
		time.Sleep(time.Second)
	}
}

func writeShell(t *testing.T, id, payload string) {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/write", map[string]string{"payload": payload})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status=%d body=%s", resp.StatusCode, body)
	}
}

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
