//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// This file seeds the *deferred* e2e scenarios — the ones blocked on real
// adapters or lifecycle triggers that the α stub SUT cannot exercise. Each test
// still blocked skips with its precondition and the AC it will verify; the ones
// whose precondition has landed (the real client-go PodOrchestrator) are filled
// and assert directly against the cluster API.
//
// The mapping from these placeholders to the documented scenarios/ACs lives in
// docs/test/e2e.md.

// sessionNamespace is where the deployed control plane provisions its data plane
// pods — the same namespace it runs in (default in the kind deploy/). Overridable
// for clusters that place the control plane elsewhere.
func sessionNamespace() string {
	if v := os.Getenv("E2E_SESSION_NAMESPACE"); v != "" {
		return v
	}
	return "default"
}

// kubeClient builds a client for the cluster the SUT runs in, from the ambient
// kubeconfig (kind writes one) or the in-cluster config. It also returns the
// rest config so callers can open exec streams (e2e_shell_test.go). It reports
// ok=false when neither is available, so a run pointed at a non-cluster SUT
// skips rather than fails.
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

// AC-A1/A2 (real pod): the deployed control-plane creates a dedicated Pod object
// per session in the session namespace, 1:1 (architecture scenarios 1·2). Filled
// now that the real client-go PodOrchestrator backs the SUT — the API's returned
// pod name corresponds to an actual Pod object labelled to its session.
func TestDeferred_RealPodProvisioned(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — real-pod assertion needs the deployed cluster")
	}
	ns := sessionNamespace()

	// Scenario 1: one session => one Pod object, labelled 1:1 to the session.
	s := createSession(t, uniqueName(t))
	if s.Pod == "" {
		t.Fatal("API returned an empty pod name for a created session (AC-A1/A2)")
	}
	pod := getPodEventually(t, cs, ns, s.Pod)
	if got := pod.Labels["session-id"]; got != s.ID {
		t.Fatalf("pod %s session-id label=%q want %q (AC-A2 1:1)", s.Pod, got, s.ID)
	}

	// Scenario 2: N sessions => N unique Pods, each labelled to its own session.
	const n = 3
	seen := map[string]string{} // pod name -> session id
	for i := 0; i < n; i++ {
		si := createSession(t, fmt.Sprintf("%s-%d", uniqueName(t), i))
		if si.Pod == "" {
			t.Fatalf("session %s has no pod", si.ID)
		}
		if prev, dup := seen[si.Pod]; dup {
			t.Fatalf("pod %q shared by sessions %s and %s (AC-A2 violated)", si.Pod, prev, si.ID)
		}
		seen[si.Pod] = si.ID
		p := getPodEventually(t, cs, ns, si.Pod)
		if got := p.Labels["session-id"]; got != si.ID {
			t.Fatalf("pod %s session-id label=%q want %q", si.Pod, got, si.ID)
		}
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique pods, got %d", n, len(seen))
	}
}

// AC-A3 (real pod): terminating/snapshotting a session deletes its Pod and
// reclaims cluster resources.
// Blocked on: a real client-go PodOrchestrator (and the snapshot/terminate path).
func TestDeferred_RealPodReclaimed(t *testing.T) {
	t.Skip("deferred: needs the real client-go PodOrchestrator to assert Pod deletion + resource reclaim (AC-A3); fill when the k8s adapter lands")
}

// AC-B1: after 60m idle a session is checkpointed (CRIU) and transitions to
// snapshot with its pod reclaimed.
// Blocked on: an idle->snapshot trigger (reaper or test-only endpoint). The α SUT
// never leaves the active state, so there is no way to drive a snapshot here.
func TestDeferred_IdleToSnapshot(t *testing.T) {
	t.Skip("deferred: needs an idle->snapshot trigger (reaper or test endpoint) to reach the snapshot state (AC-B1); fill when the lifecycle trigger lands")
}

// AC-B2: accessing a snapshot session restores it into a new pod and goes active.
// Blocked on: a snapshot-state session (see IdleToSnapshot) + real CRIU.
func TestDeferred_SnapshotRestore(t *testing.T) {
	t.Skip("deferred: needs a snapshot-state session (AC-B1 trigger) to exercise restore-on-access (AC-B2); fill when snapshot + restore land")
}

// AC-B3: a restored session's in-memory state matches the pre-snapshot state.
// Blocked on: a verified CRIU runtime (ContainerCheckpoint feature gate) — the
// stub checkpointer carries no real process state. See docs/criu-verification.md.
func TestDeferred_CRIUIntegrity(t *testing.T) {
	t.Skip("deferred: needs a verified CRIU runtime to assert checkpoint/restore integrity (AC-B3); see docs/criu-verification.md")
}

// AC-C2 (idle/snapshot branches): read dispatches on a non-active state.
// Blocked on: idle/snapshot states (lifecycle trigger). The active-read branch
// is covered by TestReadSession_ActivePath in e2e_test.go.
func TestDeferred_ReadIdleAndSnapshotBranches(t *testing.T) {
	t.Skip("deferred: needs idle/snapshot states to assert the non-active read branches (AC-C2); the active branch is covered today")
}

// AC-C3 (idle/snapshot branches): write dispatches on a non-active state.
// Blocked on: idle/snapshot states (lifecycle trigger). The active-write branch
// is covered by TestWriteSession_ActivePath in e2e_test.go.
func TestDeferred_WriteIdleAndSnapshotBranches(t *testing.T) {
	t.Skip("deferred: needs idle/snapshot states to assert the non-active write branches (AC-C3); the active branch is covered today")
}

// AC-C1: concurrent requests to the same session, served by a multi-replica
// control-plane sharing the ConfigMap/Lease state store, converge to a single
// consistent state — no torn state, no duplicate pod, and crucially no replica
// reporting "not found". The deploy/ overlay runs 2 replicas behind one Service,
// so this burst load-balances across both: with the old in-memory store roughly
// half these requests would 404 (each replica had its own map); with the shared
// store every replica sees the one session.
//
// Division of labour: the hermetic single-winner proof (exactly one of N
// concurrent CompareAndSwap / Lock calls wins, against a real apiserver) lives
// in the envtest suite (internal/adapter/configmap/envtest). This test asserts
// the dimension that suite cannot — two real control-plane processes sharing the
// store — which is what makes the α SUT's deferred skip obsolete.
func TestDeferred_CrossReplicaAtomicity(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — cross-replica assertion needs the deployed multi-replica SUT")
	}
	ns := sessionNamespace()

	// One session, created once. It lands in a ConfigMap visible to every replica.
	s := createSession(t, uniqueName(t))
	if s.Pod == "" {
		t.Fatal("created session has no pod (AC-A1/A2)")
	}

	// Fan a burst of concurrent requests at that one session. The Service
	// load-balances them across replicas; a non-shared store surfaces as 404s or
	// torn state.
	const workers = 24
	ops := []string{"get", "read", "write", "switch"}
	type outcome struct {
		op         string
		state, pod string
		err        error
	}
	results := make(chan outcome, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			op := ops[i%len(ops)]
			state, pod, err := callSession(s.ID, op)
			results <- outcome{op: op, state: state, pod: pod, err: err}
		}(i)
	}
	wg.Wait()
	close(results)

	for r := range results {
		if r.err != nil {
			t.Errorf("%s on shared session failed: %v", r.op, r.err)
			continue
		}
		if r.state != "active" {
			t.Errorf("%s observed state=%q want active (no torn cross-replica state, AC-C1)", r.op, r.state)
		}
		if r.pod != "" && r.pod != s.Pod {
			t.Errorf("%s observed pod=%q want %q (no duplicate pod under concurrency, AC-A2/C1)", r.op, r.pod, s.Pod)
		}
	}

	// Ground truth from the cluster: exactly one pod backs the session — concurrent
	// access never provisioned a duplicate (AC-A2/C1).
	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: "session-id=" + s.ID,
	})
	if err != nil {
		t.Fatalf("list pods for session %s: %v", s.ID, err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("session %s backed by %d pods, want exactly 1 (AC-A2/C1)", s.ID, len(pods.Items))
	}

	// Final read-back: still a single, consistent, active session.
	resp, body := do(t, http.MethodGet, "/api/v1/sessions/"+s.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final get status=%d body=%s", resp.StatusCode, body)
	}
	var final session
	if err := json.Unmarshal(body, &final); err != nil {
		t.Fatalf("decode final get: %v body=%s", err, body)
	}
	if final.ID != s.ID || final.State != "active" || final.Pod != s.Pod {
		t.Fatalf("final session %+v diverged from created %+v", final, s)
	}
}

// callSession performs one op (get/read/write/switch) against a session and
// returns the observed state/pod. It returns errors instead of failing the test,
// so it is safe to call from many goroutines (t.Fatal must stay on the test
// goroutine). get/switch return the session directly; read/write wrap it.
func callSession(id, op string) (state, pod string, err error) {
	var method, path string
	var body io.Reader
	switch op {
	case "get":
		method, path = http.MethodGet, "/api/v1/sessions/"+id
	case "read":
		method, path = http.MethodPost, "/api/v1/sessions/"+id+"/read"
	case "write":
		method, path = http.MethodPost, "/api/v1/sessions/"+id+"/write"
		body = strings.NewReader(`{"payload":"x"}`)
	case "switch":
		method, path = http.MethodPost, "/api/v1/sessions/"+id+"/switch"
	default:
		return "", "", fmt.Errorf("unknown op %q", op)
	}
	req, err := http.NewRequest(method, baseURL()+path, body)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s %s: status=%d body=%s", method, path, resp.StatusCode, b)
	}
	switch op {
	case "get", "switch":
		var s session
		if err := json.Unmarshal(b, &s); err != nil {
			return "", "", fmt.Errorf("decode %s: %w body=%s", op, err, b)
		}
		return s.State, s.Pod, nil
	default: // read, write wrap the session
		var wrapped struct {
			Session session `json:"session"`
		}
		if err := json.Unmarshal(b, &wrapped); err != nil {
			return "", "", fmt.Errorf("decode %s: %w body=%s", op, err, b)
		}
		return wrapped.Session.State, wrapped.Session.Pod, nil
	}
}
