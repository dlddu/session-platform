package configmap_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const testNS = "sessions"

func newStore(t *testing.T) (*configmap.Store, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	return configmap.NewStore(cs, testNS), cs
}

func sampleSession(id string) *session.Session {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	return &session.Session{
		ID:         id,
		Name:       "sess-" + id,
		State:      session.StateActive,
		Pod:        "sess-" + id,
		CreatedAt:  now,
		LastAccess: now,
	}
}

// Put writes a ConfigMap, Get reads the same session back, and the underlying
// object is named/labelled 1:1 to its session (V5: single source of truth).
func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, cs := newStore(t)

	in := sampleSession("a1b2")
	if err := store.Put(ctx, in); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := store.Get(ctx, "a1b2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != in.ID || got.Name != in.Name || got.State != in.State || got.Pod != in.Pod {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, in)
	}
	if !got.CreatedAt.Equal(in.CreatedAt) || !got.LastAccess.Equal(in.LastAccess) {
		t.Fatalf("timestamps not preserved: got %+v want %+v", got, in)
	}

	cm, err := cs.CoreV1().ConfigMaps(testNS).Get(ctx, "session-a1b2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("underlying configmap: %v", err)
	}
	if got := cm.Labels["app.kubernetes.io/managed-by"]; got != "control-plane" {
		t.Errorf("managed-by label=%q want control-plane", got)
	}
	if got := cm.Labels["session-id"]; got != "a1b2" {
		t.Errorf("session-id label=%q want a1b2", got)
	}
}

// Put on an existing session updates it in place (no second ConfigMap).
func TestPutUpdatesInPlace(t *testing.T) {
	ctx := context.Background()
	store, cs := newStore(t)

	s := sampleSession("c3d4")
	if err := store.Put(ctx, s); err != nil {
		t.Fatalf("put: %v", err)
	}
	s.State = session.StateIdle
	s.LastAccess = s.LastAccess.Add(time.Hour)
	if err := store.Put(ctx, s); err != nil {
		t.Fatalf("put (update): %v", err)
	}

	got, err := store.Get(ctx, "c3d4")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != session.StateIdle {
		t.Errorf("state=%q want idle after update", got.State)
	}
	list, err := cs.CoreV1().ConfigMaps(testNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly 1 configmap after re-put, got %d", len(list.Items))
	}
}

// List returns every owned session and ignores ConfigMaps this control plane
// does not own (no managed-by label).
func TestListScopedToOwned(t *testing.T) {
	ctx := context.Background()
	store, cs := newStore(t)

	for _, id := range []string{"aa01", "bb02", "cc03"} {
		if err := store.Put(ctx, sampleSession(id)); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	// A foreign ConfigMap that must not appear in List.
	if _, err := cs.CoreV1().ConfigMaps(testNS).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: testNS},
		Data:       map[string]string{"x": "y"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create foreign configmap: %v", err)
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("list len=%d want 3 (owned only)", len(got))
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.ID] = true
	}
	for _, id := range []string{"aa01", "bb02", "cc03"} {
		if !seen[id] {
			t.Errorf("session %s missing from list", id)
		}
	}
}

func TestGetNotFound(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.Get(context.Background(), "nope"); err != session.ErrNotFound {
		t.Fatalf("get unknown err=%v want ErrNotFound", err)
	}
}

// Delete removes the session and is idempotent (AC-A3 reclaim hygiene).
func TestDeleteIdempotent(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	if err := store.Put(ctx, sampleSession("ee05")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete(ctx, "ee05"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, "ee05"); err != session.ErrNotFound {
		t.Fatalf("get after delete err=%v want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "ee05"); err != nil {
		t.Fatalf("delete (idempotent) err=%v", err)
	}
}

// CompareAndSwapState moves the state only when the current state matches
// `from`; a mismatch is ErrConflict and an unknown id is ErrNotFound (AC-C1).
func TestCompareAndSwapState(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	if err := store.CompareAndSwapState(ctx, "ghost", session.StateActive, session.StateIdle); err != session.ErrNotFound {
		t.Fatalf("cas unknown err=%v want ErrNotFound", err)
	}

	if err := store.Put(ctx, sampleSession("ff06")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.CompareAndSwapState(ctx, "ff06", session.StateActive, session.StateIdle); err != nil {
		t.Fatalf("cas active->idle: %v", err)
	}
	got, err := store.Get(ctx, "ff06")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != session.StateIdle {
		t.Fatalf("state=%q want idle after cas", got.State)
	}
	// Current state is now idle, so a from=active swap must conflict.
	if err := store.CompareAndSwapState(ctx, "ff06", session.StateActive, session.StateSnapshot); err != session.ErrConflict {
		t.Fatalf("cas stale-from err=%v want ErrConflict", err)
	}
}

// Lock is exclusive: a second holder conflicts until the first releases (AC-C1).
func TestLockConflictAndRelease(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	if err := store.Lock(ctx, "s1", "tokenA"); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if err := store.Lock(ctx, "s1", "tokenB"); err != session.ErrConflict {
		t.Fatalf("contended lock err=%v want ErrConflict", err)
	}
	if err := store.Unlock(ctx, "s1", "tokenA"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// Released — a new holder can now acquire it.
	if err := store.Lock(ctx, "s1", "tokenB"); err != nil {
		t.Fatalf("re-lock after release: %v", err)
	}
}

// Unlock only releases a lock the token actually holds, and is a no-op
// otherwise (including when no lock exists).
func TestUnlockScopedToHolder(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)

	if err := store.Unlock(ctx, "s2", "whoever"); err != nil {
		t.Fatalf("unlock with no lock err=%v want nil", err)
	}

	if err := store.Lock(ctx, "s2", "owner"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	// A non-holder's Unlock must not free the lock.
	if err := store.Unlock(ctx, "s2", "intruder"); err != nil {
		t.Fatalf("intruder unlock err=%v want nil (no-op)", err)
	}
	if err := store.Lock(ctx, "s2", "other"); err != session.ErrConflict {
		t.Fatalf("lock still held err=%v want ErrConflict", err)
	}
	// The real holder can release it.
	if err := store.Unlock(ctx, "s2", "owner"); err != nil {
		t.Fatalf("owner unlock: %v", err)
	}
	if err := store.Lock(ctx, "s2", "other"); err != nil {
		t.Fatalf("lock after owner release: %v", err)
	}
}

// A crashed holder's lock self-heals: once renewTime + leaseDuration passes, a
// new caller takes it over rather than being blocked forever.
func TestLockTakesOverStaleLease(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	offset := time.Duration(0)
	clock := func() time.Time { return base.Add(offset) }
	store := configmap.NewStore(cs, testNS,
		configmap.WithClock(clock), configmap.WithLeaseDuration(time.Second))

	if err := store.Lock(ctx, "s3", "stale-holder"); err != nil {
		t.Fatalf("initial lock: %v", err)
	}
	// Still within the lease window: contention is rejected.
	offset = 500 * time.Millisecond
	if err := store.Lock(ctx, "s3", "newcomer"); err != session.ErrConflict {
		t.Fatalf("within-window lock err=%v want ErrConflict", err)
	}
	// Past renewTime + leaseDuration: the stale lock can be taken over.
	offset = 2 * time.Second
	if err := store.Lock(ctx, "s3", "newcomer"); err != nil {
		t.Fatalf("takeover of stale lock: %v", err)
	}
	lease, err := cs.CoordinationV1().Leases(testNS).Get(ctx, "session-lock-s3", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "newcomer" {
		t.Fatalf("holder=%v want newcomer after takeover", lease.Spec.HolderIdentity)
	}
}
