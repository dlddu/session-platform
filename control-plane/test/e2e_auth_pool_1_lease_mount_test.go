//go:build e2e

// 검증 시나리오: agent-auth-pool.md#시나리오 1
package e2e_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAuthPool1_SessionLeasesOneSlotAndMountsOnlyIt(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	status, s, raw := createCodexTask(t, nil)
	if status != http.StatusCreated {
		t.Fatalf("create codex-task: %d %s", status, raw)
	}
	slot := heldSlotOf(t, cs, codexTaskPool, s.ID)
	if slot < 0 {
		t.Fatalf("no %s lease names session %s as holder", codexTaskPool, s.ID)
	}
	claim := codexTaskPool + "-auth-" + strconv.Itoa(slot)
	pvc, err := cs.CoreV1().PersistentVolumeClaims(sessionNamespace()).Get(context.Background(), claim, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("slot PVC %s: %v", claim, err)
	}
	if pvc.Labels[authPoolLabel] != codexTaskPool || pvc.Labels[authSlotLabel] != strconv.Itoa(slot) {
		t.Errorf("slot PVC labels = %v", pvc.Labels)
	}

	pod := sessionWorkloadPod(t, cs, s.ID)
	var claims []string
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.PersistentVolumeClaim.ClaimName)
		}
	}
	if len(claims) != 1 || claims[0] != claim {
		t.Fatalf("pod PVC volumes = %v, want only %s", claims, claim)
	}
	for i, c := range pod.Spec.Containers {
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == authSlotVolume {
				mounted = true
				if m.MountPath != authSlotMount {
					t.Errorf("container %s mounts the slot at %s", c.Name, m.MountPath)
				}
			}
		}
		if mounted != (i == 0) {
			t.Errorf("container %s slot mount = %v, want only the main container", c.Name, mounted)
		}
	}
	if e, ok := containerEnv(pod.Spec.Containers[0], "AGENT_AUTH_SLOT_DIR"); !ok || e.Value != authSlotMount {
		t.Errorf("AGENT_AUTH_SLOT_DIR = %+v, want %s", e, authSlotMount)
	}
}
