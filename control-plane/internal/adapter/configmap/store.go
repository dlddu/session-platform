// Package configmap is the Kubernetes-backed StateStore adapter. Session
// metadata lives in one ConfigMap per session (name "session-<id>", the
// session.Session JSON under a single data key), and per-session occupancy
// locks are coordination.k8s.io/v1 Leases ("session-lock-<id>").
//
// Atomicity (AC-C1) comes from the API server, not a mutex:
//   - CompareAndSwapState reads the ConfigMap (capturing its resourceVersion)
//     and writes it back; the API server rejects the write with a 409 Conflict
//     if the resourceVersion moved underneath us, so concurrent transitions on
//     the same session converge to a single winner across control plane replicas.
//   - Lock creates the Lease; the API server admits exactly one Create for a
//     given name, so the occupancy claim is exclusive. A crashed holder's lock
//     self-heals via leaseDurationSeconds (a stale Lease can be taken over).
//
// This keeps the same StateStore contract the in-memory stub had, so the
// service layer is unchanged — only the backend (and its atomicity guarantee)
// differs.
package configmap

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/store"
)

const (
	// labelManagedBy / managedByValue mark the objects this control plane owns
	// so List never picks up a ConfigMap it did not create, mirroring the pod
	// orchestrator's ownership convention.
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "control-plane"
	// labelSessionID ties an object 1:1 to its session.
	labelSessionID = "session-id"

	// dataKey is the ConfigMap data entry holding the session.Session JSON.
	dataKey = "session"

	// namePrefix / lockPrefix derive DNS-safe object names from a session id so
	// the session<->object mapping is recoverable from the id alone.
	namePrefix = "session-"
	lockPrefix = "session-lock-"

	// defaultLeaseDuration bounds how long a crashed lock holder keeps the lock:
	// once renewTime + this duration has passed, another caller may take over.
	defaultLeaseDuration = 15 * time.Second

	// putMaxRetries bounds Put's optimistic-update retry loop. Put is a
	// last-writer-wins save (the atomic guard is CompareAndSwapState/Lock), so a
	// lost race just re-reads the current object and reapplies.
	putMaxRetries = 5
)

// Store is the ConfigMap + Lease backed StateStore.
type Store struct {
	client    kubernetes.Interface
	namespace string
	leaseDur  time.Duration
	now       func() time.Time // injectable clock for lease-staleness in tests
}

// compile-time assertion that Store satisfies the port.
var _ store.StateStore = (*Store)(nil)

// Option customises a Store.
type Option func(*Store)

// WithLeaseDuration overrides how long a held lock survives without renewal
// before it can be taken over (default 15s).
func WithLeaseDuration(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.leaseDur = d
		}
	}
}

// WithClock injects the clock used for lease-staleness decisions (tests only).
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// NewStore builds a ConfigMap-backed store bound to a namespace. Injecting
// kubernetes.Interface lets tests drive it with a fake clientset; main builds
// the client and namespace via k8s.BuildClient (the same client the pod
// orchestrator uses).
func NewStore(client kubernetes.Interface, namespace string, opts ...Option) *Store {
	s := &Store{
		client:    client,
		namespace: namespace,
		leaseDur:  defaultLeaseDuration,
		now:       func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Put upserts the session's ConfigMap. It is a last-writer-wins save: a
// concurrent update is retried on its fresh resourceVersion. The atomic
// guarantees live in CompareAndSwapState and Lock, not here.
func (s *Store) Put(ctx context.Context, sess *session.Session) error {
	cms := s.client.CoreV1().ConfigMaps(s.namespace)
	for attempt := 0; ; attempt++ {
		existing, err := cms.Get(ctx, cmName(sess.ID), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cm := s.configMapFor(sess)
			if encErr := encodeInto(cm, sess); encErr != nil {
				return encErr
			}
			_, cErr := cms.Create(ctx, cm, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(cErr) && attempt < putMaxRetries {
				continue // raced with another create; reload and update instead
			}
			if cErr != nil {
				return fmt.Errorf("create configmap %s: %w", cmName(sess.ID), cErr)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("get configmap %s: %w", cmName(sess.ID), err)
		}
		ensureLabels(existing, sess.ID)
		if encErr := encodeInto(existing, sess); encErr != nil {
			return encErr
		}
		_, uErr := cms.Update(ctx, existing, metav1.UpdateOptions{})
		if apierrors.IsConflict(uErr) && attempt < putMaxRetries {
			continue // someone updated underneath us; reload and reapply
		}
		if uErr != nil {
			return fmt.Errorf("update configmap %s: %w", cmName(sess.ID), uErr)
		}
		return nil
	}
}

// Get returns the session, or session.ErrNotFound if no ConfigMap exists.
func (s *Store) Get(ctx context.Context, id string) (*session.Session, error) {
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, cmName(id), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, session.ErrNotFound
		}
		return nil, fmt.Errorf("get configmap %s: %w", cmName(id), err)
	}
	return decode(cm)
}

// List returns every session this control plane owns, found by label selector.
func (s *Store) List(ctx context.Context) ([]*session.Session, error) {
	list, err := s.client.CoreV1().ConfigMaps(s.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelManagedBy + "=" + managedByValue,
	})
	if err != nil {
		return nil, fmt.Errorf("list configmaps: %w", err)
	}
	out := make([]*session.Session, 0, len(list.Items))
	for i := range list.Items {
		sess, err := decode(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, nil
}

// Delete removes the session's ConfigMap (idempotent) and best-effort releases
// its lock Lease so a terminated session leaves nothing behind.
func (s *Store) Delete(ctx context.Context, id string) error {
	err := s.client.CoreV1().ConfigMaps(s.namespace).Delete(ctx, cmName(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete configmap %s: %w", cmName(id), err)
	}
	// The lock Lease, if any, is no longer meaningful once the session is gone.
	_ = s.client.CoordinationV1().Leases(s.namespace).Delete(ctx, lockName(id), metav1.DeleteOptions{})
	return nil
}

// CompareAndSwapState atomically moves a session from->to. It returns
// session.ErrConflict if the current state is not `from`, or if the ConfigMap
// changed underneath us (resourceVersion conflict) — the optimistic-concurrency
// primitive that makes transitions atomic across replicas (AC-C1).
func (s *Store) CompareAndSwapState(ctx context.Context, id string, from, to session.State) error {
	cms := s.client.CoreV1().ConfigMaps(s.namespace)
	cm, err := cms.Get(ctx, cmName(id), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return session.ErrNotFound
		}
		return fmt.Errorf("get configmap %s: %w", cmName(id), err)
	}
	sess, err := decode(cm)
	if err != nil {
		return err
	}
	if sess.State != from {
		return session.ErrConflict
	}
	sess.State = to
	if err := encodeInto(cm, sess); err != nil {
		return err
	}
	// cm carries the resourceVersion from Get; the API server rejects the Update
	// with 409 if another writer moved it first — so only one CAS wins.
	if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return session.ErrConflict
		}
		return fmt.Errorf("update configmap %s: %w", cmName(id), err)
	}
	return nil
}

// Lock acquires the per-session occupancy Lease. Creating the Lease is the
// atomic claim: the API server admits exactly one Create for the name, so a
// concurrent loser gets AlreadyExists and, unless the existing lock is its own
// or has expired, session.ErrConflict.
func (s *Store) Lock(ctx context.Context, id, token string) error {
	leases := s.client.CoordinationV1().Leases(s.namespace)
	if _, err := leases.Create(ctx, s.leaseFor(id, token), metav1.CreateOptions{}); err == nil {
		return nil // acquired a fresh lock
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create lease %s: %w", lockName(id), err)
	}

	// A Lease already exists; take it over only if it is ours or has expired.
	existing, err := leases.Get(ctx, lockName(id), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return session.ErrConflict // released between our Create and Get; caller retries
		}
		return fmt.Errorf("get lease %s: %w", lockName(id), err)
	}
	if !s.heldByUsOrExpired(existing, token) {
		return session.ErrConflict
	}
	now := metav1.NewMicroTime(s.now())
	dur := leaseSeconds(s.leaseDur)
	existing.Spec.HolderIdentity = &token
	existing.Spec.AcquireTime = &now
	existing.Spec.RenewTime = &now
	existing.Spec.LeaseDurationSeconds = &dur
	if _, err := leases.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return session.ErrConflict // another taker won the race
		}
		return fmt.Errorf("take over lease %s: %w", lockName(id), err)
	}
	return nil
}

// Unlock releases the lock only if token still holds it, so a late Unlock never
// frees a lock a different holder has since taken over. Idempotent.
func (s *Store) Unlock(ctx context.Context, id, token string) error {
	leases := s.client.CoordinationV1().Leases(s.namespace)
	existing, err := leases.Get(ctx, lockName(id), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already released
		}
		return fmt.Errorf("get lease %s: %w", lockName(id), err)
	}
	if existing.Spec.HolderIdentity == nil || *existing.Spec.HolderIdentity != token {
		return nil // not ours — leave it for its holder
	}
	// Precondition on resourceVersion so we never delete a lock that changed
	// (e.g. was taken over) between this Get and the Delete.
	err = leases.Delete(ctx, lockName(id), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{ResourceVersion: &existing.ResourceVersion},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete lease %s: %w", lockName(id), err)
	}
	return nil
}

// heldByUsOrExpired reports whether the lock can be taken: either token already
// holds it, or it has gone stale (renewTime + leaseDurationSeconds in the past).
func (s *Store) heldByUsOrExpired(lease *coordinationv1.Lease, token string) bool {
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == token {
		return true
	}
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return false
	}
	deadline := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return s.now().After(deadline)
}

func (s *Store) configMapFor(sess *session.Session) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName(sess.ID),
			Namespace: s.namespace,
		},
	}
	ensureLabels(cm, sess.ID)
	return cm
}

func (s *Store) leaseFor(id, token string) *coordinationv1.Lease {
	now := metav1.NewMicroTime(s.now())
	dur := leaseSeconds(s.leaseDur)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lockName(id),
			Namespace: s.namespace,
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelSessionID: id,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &token,
			LeaseDurationSeconds: &dur,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
}

// ---- helpers ----

func cmName(id string) string   { return namePrefix + id }
func lockName(id string) string { return lockPrefix + id }

func leaseSeconds(d time.Duration) int32 {
	s := int32(d / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

func ensureLabels(cm *corev1.ConfigMap, id string) {
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels[labelManagedBy] = managedByValue
	cm.Labels[labelSessionID] = id
}

func decode(cm *corev1.ConfigMap) (*session.Session, error) {
	raw, ok := cm.Data[dataKey]
	if !ok {
		return nil, fmt.Errorf("configmap %s missing %q data key", cm.Name, dataKey)
	}
	var sess session.Session
	if err := json.Unmarshal([]byte(raw), &sess); err != nil {
		return nil, fmt.Errorf("decode session from configmap %s: %w", cm.Name, err)
	}
	return &sess, nil
}

func encodeInto(cm *corev1.ConfigMap, sess *session.Session) error {
	b, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("encode session %s: %w", sess.ID, err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[dataKey] = string(b)
	return nil
}
