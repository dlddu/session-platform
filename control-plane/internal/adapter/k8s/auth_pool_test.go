package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

type poolClock struct{ t time.Time }

func (c *poolClock) now() time.Time { return c.t }

func newTestPool(t *testing.T, slots int) (*LeaseAuthPool, *fake.Clientset, *poolClock) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	clock := &poolClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	pool, err := NewLeaseAuthPool(cs, "sessions", AuthPoolConfig{
		Pool: AuthPoolCodexTask, Slots: slots, StorageClass: "local-path", Now: clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool, cs, clock
}

func holderOf(t *testing.T, cs *fake.Clientset, slot int) string {
	t.Helper()
	lease, err := cs.CoordinationV1().Leases("sessions").Get(context.Background(), AuthSlotClaimName(AuthPoolCodexTask, slot), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return leaseHolder(lease)
}

func TestLeaseAuthPoolAcquireCreatesLeaseAndClaim(t *testing.T) {
	pool, cs, _ := newTestPool(t, 2)
	ctx := context.Background()
	slot, err := pool.Acquire(ctx, "s1", nil)
	if err != nil || slot != 0 {
		t.Fatalf("Acquire = %d, %v", slot, err)
	}
	if got := holderOf(t, cs, 0); got != "s1" {
		t.Fatalf("holder = %q", got)
	}
	claim, err := cs.CoreV1().PersistentVolumeClaims("sessions").Get(ctx, "codex-task-auth-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("slot claim not created: %v", err)
	}
	if claim.Labels[LabelAuthPool] != AuthPoolCodexTask || claim.Labels[LabelAuthSlot] != "0" {
		t.Errorf("claim labels = %v", claim.Labels)
	}
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "local-path" {
		t.Errorf("storage class = %v", claim.Spec.StorageClassName)
	}
	if got := claim.Spec.Resources.Requests.Storage().String(); got != DefaultAuthSlotStorageSize {
		t.Errorf("size = %s", got)
	}
	// Idempotent for the holder: a retried create keeps its slot.
	if again, err := pool.Acquire(ctx, "s1", nil); err != nil || again != 0 {
		t.Fatalf("second Acquire = %d, %v", again, err)
	}
}

func TestLeaseAuthPoolExclusiveAndExhausted(t *testing.T) {
	pool, _, _ := newTestPool(t, 2)
	ctx := context.Background()
	if s, err := pool.Acquire(ctx, "s1", nil); err != nil || s != 0 {
		t.Fatalf("s1 = %d, %v", s, err)
	}
	if s, err := pool.Acquire(ctx, "s2", nil); err != nil || s != 1 {
		t.Fatalf("s2 = %d, %v", s, err)
	}
	if _, err := pool.Acquire(ctx, "s3", nil); !errors.Is(err, session.ErrAuthPoolExhausted) {
		t.Fatalf("third acquire err = %v, want exhausted", err)
	}
}

func TestLeaseAuthPoolReleaseRecordsStateAndKeepsClaim(t *testing.T) {
	pool, cs, _ := newTestPool(t, 2)
	ctx := context.Background()
	_, _ = pool.Acquire(ctx, "s1", nil)
	_, _ = pool.Acquire(ctx, "s2", nil)
	signedIn, signedOut := true, false
	if err := pool.Release(ctx, "s2", &signedIn); err != nil {
		t.Fatal(err)
	}
	if err := pool.Release(ctx, "s1", &signedOut); err != nil {
		t.Fatal(err)
	}
	// A signed-in slot outranks a lower-numbered signed-out one (AC-L2).
	if s, err := pool.Acquire(ctx, "s3", nil); err != nil || s != 1 {
		t.Fatalf("s3 = %d, %v, want the signed-in slot 1", s, err)
	}
	lease, _ := cs.CoordinationV1().Leases("sessions").Get(ctx, "codex-task-auth-0", metav1.GetOptions{})
	if lease.Annotations[AnnotationAuthState] != AuthStateSignedOut {
		t.Errorf("slot 0 state = %q", lease.Annotations[AnnotationAuthState])
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("sessions").Get(ctx, "codex-task-auth-0", metav1.GetOptions{}); err != nil {
		t.Errorf("release deleted the slot claim: %v", err)
	}
	// Releasing a slot one does not hold leaves the holder alone.
	if err := pool.Release(ctx, "nobody", nil); err != nil {
		t.Fatal(err)
	}
	if got := holderOf(t, cs, 1); got != "s3" {
		t.Errorf("holder = %q after a stranger's release", got)
	}
}

func TestLeaseAuthPoolNeverLeasedRanksBelowUnknown(t *testing.T) {
	pool, _, _ := newTestPool(t, 2)
	ctx := context.Background()
	_, _ = pool.Acquire(ctx, "s1", nil)
	_ = pool.Release(ctx, "s1", nil) // slot 0 leased once, state unknown
	_, _ = pool.Acquire(ctx, "s2", nil)
	_, _ = pool.Acquire(ctx, "s3", nil)
	_ = pool.Release(ctx, "s2", nil)
	if s, err := pool.Acquire(ctx, "s4", nil); err != nil || s != 0 {
		t.Fatalf("s4 = %d, %v, want slot 0", s, err)
	}
}

func TestLeaseAuthPoolReclaimNeedsStaleHolderAndAge(t *testing.T) {
	pool, cs, clock := newTestPool(t, 1)
	ctx := context.Background()
	if _, err := pool.Acquire(ctx, "crashed", nil); err != nil {
		t.Fatal(err)
	}
	stale := func(context.Context, string) (bool, error) { return true, nil }
	live := func(context.Context, string) (bool, error) { return false, nil }

	clock.t = clock.t.Add(DefaultAuthPoolReclaimGrace - time.Second)
	if _, err := pool.Acquire(ctx, "s2", stale); !errors.Is(err, session.ErrAuthPoolExhausted) {
		t.Fatalf("reclaimed inside the grace: %v", err)
	}
	clock.t = clock.t.Add(2 * time.Second)
	if _, err := pool.Acquire(ctx, "s2", live); !errors.Is(err, session.ErrAuthPoolExhausted) {
		t.Fatalf("reclaimed from a live holder: %v", err)
	}
	if s, err := pool.Acquire(ctx, "s2", stale); err != nil || s != 0 {
		t.Fatalf("stale reclaim = %d, %v", s, err)
	}
	if got := holderOf(t, cs, 0); got != "s2" {
		t.Errorf("holder = %q", got)
	}
}

func TestLeaseAuthPoolSkipsSlotLostToConflict(t *testing.T) {
	pool, cs, _ := newTestPool(t, 2)
	ctx := context.Background()
	_, _ = pool.Acquire(ctx, "s1", nil)
	_, _ = pool.Acquire(ctx, "s2", nil)
	_ = pool.Release(ctx, "s1", nil)
	_ = pool.Release(ctx, "s2", nil)
	conflicted := false
	cs.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		lease := action.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease)
		if lease.Name == "codex-task-auth-0" && !conflicted {
			conflicted = true
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, lease.Name, errors.New("moved"))
		}
		return false, nil, nil
	})
	if s, err := pool.Acquire(ctx, "s3", nil); err != nil || s != 1 {
		t.Fatalf("s3 = %d, %v, want slot 1 after losing slot 0", s, err)
	}
}

func TestNewLeaseAuthPoolValidates(t *testing.T) {
	cs := fake.NewSimpleClientset()
	for _, cfg := range []AuthPoolConfig{
		{Pool: "", Slots: 1},
		{Pool: "p", Slots: 0},
		{Pool: "p", Slots: 1, StorageSize: "lots"},
		{Pool: "p", Slots: 1, StorageSize: "0"},
	} {
		if _, err := NewLeaseAuthPool(cs, "ns", cfg); err == nil {
			t.Errorf("config %+v accepted", cfg)
		}
	}
}

func TestParseClaudeCodeAuthMode(t *testing.T) {
	for in, want := range map[string]ClaudeCodeAuthMode{"": ClaudeCodeAuthGateway, "gateway": ClaudeCodeAuthGateway, "subscription": ClaudeCodeAuthSubscription} {
		if got, err := ParseClaudeCodeAuthMode(in); err != nil || got != want {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	if _, err := ParseClaudeCodeAuthMode("openrouter"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestMemoryAuthPoolMatchesLeasePoolRules(t *testing.T) {
	pool := NewMemoryAuthPool(1)
	ctx := context.Background()
	if s, err := pool.Acquire(ctx, "a", nil); err != nil || s != 0 {
		t.Fatalf("a = %d, %v", s, err)
	}
	if _, err := pool.Acquire(ctx, "b", nil); !errors.Is(err, session.ErrAuthPoolExhausted) {
		t.Fatalf("b err = %v", err)
	}
	_ = pool.Release(ctx, "a", nil)
	if s, err := pool.Acquire(ctx, "b", nil); err != nil || s != 0 {
		t.Fatalf("b after release = %d, %v", s, err)
	}
}
