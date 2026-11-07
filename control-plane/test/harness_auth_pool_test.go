//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Names and labels of docs/prd/agent-auth-pool.md (AC-N1, AC-N2), spelled out
// here because this suite knows the wire and the cluster, not the domain types.
const (
	authPoolLabel    = "session-platform.dev/auth-pool"
	authSlotLabel    = "session-platform.dev/auth-slot"
	authSlotVolume   = "auth-slot"
	authSlotMount    = "/var/run/session-platform/auth-slot"
	codexTaskPool    = "codex-task"
	poolProvisionCap = 10
)

// createCodexTask posts a codex-task create and returns the status and the
// session it made, if any. Cleanup deletes what was made, which hands the slot
// back for the next test (AC-N3).
func createCodexTask(t *testing.T, extra map[string]any) (int, tenantSession, []byte) {
	t.Helper()
	body := map[string]any{"name": uniqueName(t), "workloadType": codexTaskPool}
	for k, v := range extra {
		body[k] = v
	}
	resp, raw := doAs(t, "", http.MethodPost, "/api/v1/sessions", body)
	var s tenantSession
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("decode: %v body=%s", err, raw)
		}
		t.Cleanup(func() { doAs(t, "", http.MethodDelete, "/api/v1/sessions/"+s.ID, nil) })
	}
	return resp.StatusCode, s, raw
}

func deleteSessionNow(t *testing.T, id string) {
	t.Helper()
	resp, raw := doAs(t, "", http.MethodDelete, "/api/v1/sessions/"+id, nil)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete %s: %d %s", id, resp.StatusCode, raw)
	}
}

func poolLeases(t *testing.T, cs kubernetes.Interface, pool string) []coordinationv1.Lease {
	t.Helper()
	list, err := cs.CoordinationV1().Leases(sessionNamespace()).List(context.Background(), metav1.ListOptions{
		LabelSelector: authPoolLabel + "=" + pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// heldSlotOf is the slot whose lease names the session as holder, or -1.
func heldSlotOf(t *testing.T, cs kubernetes.Interface, pool, sessionID string) int {
	t.Helper()
	for _, l := range poolLeases(t, cs, pool) {
		if l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == sessionID {
			slot, err := strconv.Atoi(l.Labels[authSlotLabel])
			if err != nil {
				t.Fatalf("lease %s has slot label %q", l.Name, l.Labels[authSlotLabel])
			}
			return slot
		}
	}
	return -1
}

// eventuallyUnheld waits for the slot's lease to drop its holder: release runs
// after the delete has removed the pods (AC-N3).
func eventuallyUnheld(t *testing.T, cs kubernetes.Interface, pool string, slot int) {
	t.Helper()
	name := pool + "-auth-" + strconv.Itoa(slot)
	deadline := time.Now().Add(60 * time.Second)
	for {
		l, err := cs.CoordinationV1().Leases(sessionNamespace()).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get lease %s: %v", name, err)
		}
		if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease %s still held by %s", name, *l.Spec.HolderIdentity)
		}
		time.Sleep(time.Second)
	}
}

func sessionWorkloadPod(t *testing.T, cs kubernetes.Interface, id string) corev1.Pod {
	t.Helper()
	pods, err := cs.CoreV1().Pods(sessionNamespace()).List(context.Background(), metav1.ListOptions{
		LabelSelector: "session-id=" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pods for session %s = %d, want 1", id, len(pods.Items))
	}
	return pods.Items[0]
}

func containerEnv(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// fillCodexTaskPool creates codex-task sessions until the pool refuses one and
// returns the ones it made and the refusal. Other suites' sessions may hold
// slots already, so it does not assume how many are free — only that the
// deployment's pool is smaller than the cap.
func fillCodexTaskPool(t *testing.T) ([]tenantSession, int, []byte) {
	t.Helper()
	var made []tenantSession
	for i := 0; i < poolProvisionCap; i++ {
		status, s, raw := createCodexTask(t, nil)
		if status == http.StatusCreated {
			made = append(made, s)
			continue
		}
		return made, status, raw
	}
	t.Fatalf("pool accepted %d codex-task sessions; want it to run out first", poolProvisionCap)
	return nil, 0, nil
}
