//go:build e2e

// 검증 시나리오: agent-auth-pool.md#시나리오 3
package e2e_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAuthPool3_DeleteReleasesTheSlotAndKeepsThePVC(t *testing.T) {
	cs, _, ok := kubeClient(t)
	if !ok {
		t.Skip("no kube credentials for the SUT")
	}
	made, status, raw := fillCodexTaskPool(t)
	if len(made) == 0 || status != http.StatusServiceUnavailable {
		t.Fatalf("could not fill the pool: made %d, last %d %s", len(made), status, raw)
	}
	victim := made[0]
	slot := heldSlotOf(t, cs, codexTaskPool, victim.ID)
	if slot < 0 {
		t.Fatalf("session %s holds no slot", victim.ID)
	}

	deleteSessionNow(t, victim.ID)
	eventuallyUnheld(t, cs, codexTaskPool, slot)
	claim := sessionNamespace() + "-" + codexTaskPool + "-auth-" + strconv.Itoa(slot)
	if _, err := cs.CoreV1().PersistentVolumeClaims(sessionNamespace()).Get(context.Background(), claim, metav1.GetOptions{}); err != nil {
		t.Fatalf("slot PVC %s after delete: %v, want it kept", claim, err)
	}

	status, next, raw := createCodexTask(t, nil)
	if status != http.StatusCreated {
		t.Fatalf("create after release: %d %s", status, raw)
	}
	if got := heldSlotOf(t, cs, codexTaskPool, next.ID); got != slot {
		t.Fatalf("new session holds slot %d, want the released slot %d", got, slot)
	}
}
