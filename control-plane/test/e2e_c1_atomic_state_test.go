//go:build e2e

// 검증 AC: AC-C1
//
// Concurrent requests to one session, served by a multi-replica control plane
// sharing the ConfigMap(resourceVersion CAS) + Lease state store, converge to a
// single consistent state — no torn state, no duplicate pod, and crucially no
// replica reporting "not found". The deploy/ overlay runs 2 replicas behind one
// Service, so the burst below load-balances across both: with a per-replica
// in-memory store roughly half these requests would 404.
//
// The hermetic single-winner proof against a real apiserver lives in the envtest
// suite. This file asserts the dimension that suite cannot: two real
// control-plane processes sharing one store.
package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAtomicState_ConcurrentAccessConvergesAcrossReplicas(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube API access (kubeconfig/in-cluster) — cross-replica assertion needs the deployed multi-replica SUT")
	}
	ns := sessionNamespace()

	// One session, created once. It lands in a ConfigMap visible to every replica.
	s := createSession(t, uniqueName(t))
	if s.Pod == "" {
		t.Fatal("created session has no pod")
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
			t.Errorf("%s observed pod=%q want %q (no duplicate pod under concurrency, AC-C1)", r.op, r.pod, s.Pod)
		}
	}

	// Ground truth from the cluster: exactly one pod backs the session — concurrent
	// access never provisioned a duplicate.
	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: "session-id=" + s.ID,
	})
	if err != nil {
		t.Fatalf("list pods for session %s: %v", s.ID, err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("session %s backed by %d pods, want exactly 1 (AC-C1)", s.ID, len(pods.Items))
	}

	// Final read-back: still a single, consistent, active session.
	final := getSession(t, s.ID)
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
