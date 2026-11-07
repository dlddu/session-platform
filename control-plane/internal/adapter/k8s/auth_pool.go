package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Agent auth pool (docs/prd/agent-auth-pool.md, AC-L1~L5). A pool is a fixed
// row of login slots, each a PVC named like a StatefulSet's volume claim and
// a Lease of the same name whose holder is the one session using it.
const (
	// AuthPoolCodexTask and AuthPoolClaudeCode name the two pools.
	AuthPoolCodexTask  = "codex-task"
	AuthPoolClaudeCode = "claude-code"

	LabelAuthPool       = "session-platform.dev/auth-pool"
	LabelAuthSlot       = "session-platform.dev/auth-slot"
	AnnotationAuthState = "session-platform.dev/auth-state"
	AuthStateSignedIn   = "signed-in"
	AuthStateSignedOut  = "signed-out"

	// AuthSlotVolumeName, AuthSlotMountPath and AuthSlotDirEnvVar are where a
	// session pod sees its slot. Keep in sync with data-plane/cmd/agent.
	AuthSlotVolumeName = "auth-slot"
	AuthSlotMountPath  = "/var/run/session-platform/auth-slot"
	AuthSlotDirEnvVar  = "AGENT_AUTH_SLOT_DIR"

	// DefaultAuthSlotStorageSize holds one login file with room to spare.
	DefaultAuthSlotStorageSize = "16Mi"
	// DefaultAuthPoolReclaimGrace outlasts one create or restore, so a lease
	// taken before the session record exists, or while it is still frozen, is
	// never mistaken for a leftover (AC-L2).
	DefaultAuthPoolReclaimGrace = 10 * time.Minute
)

// AuthSlotLeaseName is slot i's Lease name.
func AuthSlotLeaseName(pool string, slot int) string {
	return pool + "-auth-" + strconv.Itoa(slot)
}

// AuthSlotClaimName is slot i's PVC name. Unlike the Lease it carries the
// namespace: a dynamic provisioner may key the volume on the claim name alone
// (the homelab's EFS class maps `/${.PVC.name}` to one shared access point), so
// a preview's slot of the same name would share — and on teardown delete —
// production's login (AC-L1).
func AuthSlotClaimName(namespace, pool string, slot int) string {
	return namespace + "-" + AuthSlotLeaseName(pool, slot)
}

// StaleHolder reports whether a lease holder no longer runs a pod: its session
// record is gone or frozen. The service answers from its store; the pool only
// asks about leases older than the reclaim grace.
type StaleHolder func(ctx context.Context, holder string) (bool, error)

// AuthPool leases login slots to sessions exclusively (AC-L2, AC-L3).
type AuthPool interface {
	// Acquire returns the slot the session holds, leasing a free one first if
	// it holds none. A full pool is session.ErrAuthPoolExhausted.
	Acquire(ctx context.Context, sessionID string, stale StaleHolder) (int, error)
	// Release frees the session's slot, recording whether it holds a login
	// when signedIn is known. Holding no slot is not an error.
	Release(ctx context.Context, sessionID string, signedIn *bool) error
}

// AuthPoolConfig sizes one pool.
type AuthPoolConfig struct {
	Pool         string
	Slots        int
	StorageClass string
	StorageSize  string
	// ReclaimGrace and Now are injectable for tests.
	ReclaimGrace time.Duration
	Now          func() time.Time
}

// LeaseAuthPool is the AuthPool on Lease and PVC objects. Every lease change
// is an Update or Create carrying the read resourceVersion, so two control
// plane paths can never both win the same slot.
type LeaseAuthPool struct {
	client      kubernetes.Interface
	namespace   string
	cfg         AuthPoolConfig
	storageSize resource.Quantity
}

var _ AuthPool = (*LeaseAuthPool)(nil)

// NewLeaseAuthPool validates the pool configuration.
func NewLeaseAuthPool(client kubernetes.Interface, namespace string, cfg AuthPoolConfig) (*LeaseAuthPool, error) {
	if cfg.Pool == "" {
		return nil, errors.New("auth pool needs a name")
	}
	if cfg.Slots < 1 {
		return nil, fmt.Errorf("auth pool %s needs at least one slot", cfg.Pool)
	}
	if cfg.StorageSize == "" {
		cfg.StorageSize = DefaultAuthSlotStorageSize
	}
	size, err := resource.ParseQuantity(cfg.StorageSize)
	if err != nil || size.Sign() <= 0 {
		return nil, fmt.Errorf("auth pool %s storage size %q is not a positive quantity", cfg.Pool, cfg.StorageSize)
	}
	if cfg.ReclaimGrace <= 0 {
		cfg.ReclaimGrace = DefaultAuthPoolReclaimGrace
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &LeaseAuthPool{client: client, namespace: namespace, cfg: cfg, storageSize: size}, nil
}

type slotCandidate struct {
	slot  int
	rank  int
	lease *coordinationv1.Lease // nil: the slot was never leased
}

// acquireAttempts bounds re-listing when a concurrent write moves the held
// lease between our read and our renew.
const acquireAttempts = 5

func (p *LeaseAuthPool) Acquire(ctx context.Context, sessionID string, stale StaleHolder) (int, error) {
	leases, err := p.leases(ctx)
	if err != nil {
		return 0, err
	}
	for attempt := 0; ; attempt++ {
		slot, ok := heldSlot(leases, sessionID)
		if !ok {
			break
		}
		// Reusing a held slot renews it: a lease left behind by a failed
		// release keeps its old renewTime, and a restore reusing it unrenewed
		// would look reclaimable to every other path while it runs (AC-L2).
		renewed, err := p.renew(ctx, leases[slot])
		if err != nil {
			return 0, err
		}
		if renewed {
			return slot, p.ensureClaim(ctx, slot)
		}
		if attempt+1 >= acquireAttempts {
			return 0, fmt.Errorf("renew %s auth slot %d of session %s: lease kept changing", p.cfg.Pool, slot, sessionID)
		}
		if leases, err = p.leases(ctx); err != nil {
			return 0, err
		}
	}
	// A slot that cannot be judged or claimed is skipped, not fatal: one bad
	// lease must not keep a session out of the others. Its error is reported
	// only when nothing else could be claimed.
	var skipped []error
	var candidates []slotCandidate
	for slot := 0; slot < p.cfg.Slots; slot++ {
		lease, ok := leases[slot]
		if !ok {
			// A never-leased slot has never held a login.
			candidates = append(candidates, slotCandidate{slot: slot, rank: 2})
			continue
		}
		free, err := p.free(ctx, lease, stale)
		if err != nil {
			skipped = append(skipped, fmt.Errorf("judge %s: %w", lease.Name, err))
			continue
		}
		if free {
			candidates = append(candidates, slotCandidate{slot: slot, rank: authStateRank(lease), lease: lease})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		return candidates[i].slot < candidates[j].slot
	})
	for _, c := range candidates {
		won, err := p.claim(ctx, c, sessionID)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}
		if !won {
			continue // another path took it between our list and our write
		}
		if err := p.ensureClaim(ctx, c.slot); err != nil {
			return 0, errors.Join(err, p.Release(context.WithoutCancel(ctx), sessionID, nil))
		}
		if len(skipped) > 0 {
			slog.Warn("auth pool skipped slots it could not judge or claim",
				"pool", p.cfg.Pool, "session", sessionID, "slot", c.slot, "err", errors.Join(skipped...))
		}
		return c.slot, nil
	}
	if len(skipped) > 0 {
		return 0, fmt.Errorf("no %s auth slot could be leased: %w", p.cfg.Pool, errors.Join(skipped...))
	}
	return 0, fmt.Errorf("%w: all %d %s slots are held", session.ErrAuthPoolExhausted, p.cfg.Slots, p.cfg.Pool)
}

// renew CAS-writes a fresh renewTime on the session's own lease. false means
// the lease moved under us and the caller re-reads.
func (p *LeaseAuthPool) renew(ctx context.Context, lease *coordinationv1.Lease) (bool, error) {
	now := metav1.NewMicroTime(p.cfg.Now())
	next := lease.DeepCopy()
	next.Spec.AcquireTime = &now
	next.Spec.RenewTime = &now
	_, err := p.client.CoordinationV1().Leases(p.namespace).Update(ctx, next, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("renew %s: %w", lease.Name, err)
	}
	return true, nil
}

// free applies AC-L2: an empty holder is free; a held lease is reclaimable
// only once it is older than the grace and its holder runs no pod.
func (p *LeaseAuthPool) free(ctx context.Context, lease *coordinationv1.Lease, stale StaleHolder) (bool, error) {
	holder := leaseHolder(lease)
	if holder == "" {
		return true, nil
	}
	if stale == nil {
		return false, nil
	}
	renewed := lease.CreationTimestamp.Time
	if lease.Spec.RenewTime != nil {
		renewed = lease.Spec.RenewTime.Time
	}
	if p.cfg.Now().Sub(renewed) < p.cfg.ReclaimGrace {
		return false, nil
	}
	return stale(ctx, holder)
}

func (p *LeaseAuthPool) claim(ctx context.Context, c slotCandidate, sessionID string) (bool, error) {
	now := metav1.NewMicroTime(p.cfg.Now())
	holder := sessionID
	leases := p.client.CoordinationV1().Leases(p.namespace)
	if c.lease == nil {
		_, err := leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      AuthSlotLeaseName(p.cfg.Pool, c.slot),
				Namespace: p.namespace,
				Labels:    p.labels(c.slot),
			},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, AcquireTime: &now, RenewTime: &now},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return false, p.foreignLease(ctx, c.slot)
		}
		return err == nil, err
	}
	next := c.lease.DeepCopy()
	next.Spec.HolderIdentity = &holder
	next.Spec.AcquireTime = &now
	next.Spec.RenewTime = &now
	_, err := leases.Update(ctx, next, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return false, nil
	}
	return err == nil, err
}

// foreignLease tells a lost create race from a lease of the slot's name that
// the pool cannot see: one without its labels is invisible to the list, so
// every create would lose to it forever and the slot would quietly never be
// used. That is reported, not skipped silently.
func (p *LeaseAuthPool) foreignLease(ctx context.Context, slot int) error {
	name := AuthSlotLeaseName(p.cfg.Pool, slot)
	lease, err := p.client.CoordinationV1().Leases(p.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read lease %s: %w", name, err)
	}
	if lease.Labels[LabelAuthPool] != p.cfg.Pool || lease.Labels[LabelAuthSlot] != strconv.Itoa(slot) {
		return fmt.Errorf("lease %s exists without the %s auth pool labels; slot %d is unusable until it is relabelled or removed", name, p.cfg.Pool, slot)
	}
	return nil
}

// ensureClaim makes the slot's PVC if it is missing. It is never deleted
// (AC-L1): the login outlives every session that used it.
func (p *LeaseAuthPool) ensureClaim(ctx context.Context, slot int) error {
	claims := p.client.CoreV1().PersistentVolumeClaims(p.namespace)
	name := AuthSlotClaimName(p.namespace, p.cfg.Pool, slot)
	if _, err := claims.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read auth slot claim %s: %w", name, err)
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.namespace, Labels: p.labels(slot)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: p.storageSize},
			},
		},
	}
	if p.cfg.StorageClass != "" {
		class := p.cfg.StorageClass
		claim.Spec.StorageClassName = &class
	}
	if _, err := claims.Create(ctx, claim, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create auth slot claim %s: %w", name, err)
	}
	return nil
}

// releaseAttempts bounds the retries when a concurrent write moves the
// lease's resourceVersion between our read and our update.
const releaseAttempts = 5

func (p *LeaseAuthPool) Release(ctx context.Context, sessionID string, signedIn *bool) error {
	leasesAPI := p.client.CoordinationV1().Leases(p.namespace)
	for attempt := 0; attempt < releaseAttempts; attempt++ {
		leases, err := p.leases(ctx)
		if err != nil {
			return err
		}
		slot, ok := heldSlot(leases, sessionID)
		if !ok {
			return nil
		}
		next := leases[slot].DeepCopy()
		next.Spec.HolderIdentity = nil
		now := metav1.NewMicroTime(p.cfg.Now())
		next.Spec.RenewTime = &now
		if signedIn != nil {
			if next.Annotations == nil {
				next.Annotations = map[string]string{}
			}
			next.Annotations[AnnotationAuthState] = AuthStateSignedOut
			if *signedIn {
				next.Annotations[AnnotationAuthState] = AuthStateSignedIn
			}
		}
		_, err = leasesAPI.Update(ctx, next, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("release %s auth slot of session %s: lease kept changing", p.cfg.Pool, sessionID)
}

// leases maps slot index to lease for every lease of this pool.
func (p *LeaseAuthPool) leases(ctx context.Context) (map[int]*coordinationv1.Lease, error) {
	list, err := p.client.CoordinationV1().Leases(p.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelAuthPool + "=" + p.cfg.Pool,
	})
	if err != nil {
		return nil, fmt.Errorf("list %s auth slot leases: %w", p.cfg.Pool, err)
	}
	out := make(map[int]*coordinationv1.Lease, len(list.Items))
	for i := range list.Items {
		lease := &list.Items[i]
		slot, err := strconv.Atoi(lease.Labels[LabelAuthSlot])
		if err != nil || slot < 0 || lease.Name != AuthSlotLeaseName(p.cfg.Pool, slot) {
			continue // not ours to interpret
		}
		out[slot] = lease
	}
	return out, nil
}

func (p *LeaseAuthPool) labels(slot int) map[string]string {
	return map[string]string{
		LabelAuthPool:  p.cfg.Pool,
		LabelAuthSlot:  strconv.Itoa(slot),
		labelManagedBy: managedByValue,
	}
}

// heldSlot finds the session's slot, in or out of range: a pool shrunk under
// a running session still lets that session release it.
func heldSlot(leases map[int]*coordinationv1.Lease, sessionID string) (int, bool) {
	slots := make([]int, 0, len(leases))
	for slot := range leases {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	for _, slot := range slots {
		if leaseHolder(leases[slot]) == sessionID {
			return slot, true
		}
	}
	return 0, false
}

func leaseHolder(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return strings.TrimSpace(*lease.Spec.HolderIdentity)
}

// authStateRank orders free slots signed-in, unknown, signed-out (AC-L2).
func authStateRank(lease *coordinationv1.Lease) int {
	switch lease.Annotations[AnnotationAuthState] {
	case AuthStateSignedIn:
		return 0
	case AuthStateSignedOut:
		return 2
	default:
		return 1
	}
}

// ClaudeCodeAuthMode selects claude-code's provider auth (AC-E7).
type ClaudeCodeAuthMode string

const (
	// ClaudeCodeAuthGateway is AC-E6's credential-proxy sidecar, the default.
	ClaudeCodeAuthGateway ClaudeCodeAuthMode = "gateway"
	// ClaudeCodeAuthSubscription signs the CLI in through an auth pool slot.
	ClaudeCodeAuthSubscription ClaudeCodeAuthMode = "subscription"

	// ClaudeCodeAuthModeEnvVar tells a subscription-mode agent where its login
	// comes from. Keep in sync with data-plane/cmd/agent.
	ClaudeCodeAuthModeEnvVar = "CLAUDE_CODE_AUTH_MODE"
)

// ParseClaudeCodeAuthMode reads CLAUDE_CODE_AUTH_MODE; empty is gateway.
func ParseClaudeCodeAuthMode(value string) (ClaudeCodeAuthMode, error) {
	switch mode := ClaudeCodeAuthMode(strings.TrimSpace(value)); mode {
	case "", ClaudeCodeAuthGateway:
		return ClaudeCodeAuthGateway, nil
	case ClaudeCodeAuthSubscription:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown claude-code auth mode %q (want gateway or subscription)", value)
	}
}

// WithClaudeCodeAuthMode selects claude-code's provider auth (AC-E7).
func WithClaudeCodeAuthMode(mode ClaudeCodeAuthMode) Option {
	return func(o *ClientOrchestrator) {
		if mode != "" {
			o.claudeAuthMode = mode
		}
	}
}

// AuthPoolFor names the pool a session of this type leases from, if any:
// codex-task always, claude-code in subscription mode (AC-M2, AC-E7).
func AuthPoolFor(workload session.WorkloadType, claudeMode ClaudeCodeAuthMode) (string, bool) {
	switch {
	case workload == session.WorkloadTypeCodexTask:
		return AuthPoolCodexTask, true
	case workload == session.WorkloadTypeClaudeCode && claudeMode == ClaudeCodeAuthSubscription:
		return AuthPoolClaudeCode, true
	default:
		return "", false
	}
}

// attachAuthSlot mounts the session's slot PVC into the workload container
// only — never a sidecar — and refuses a pod spec whose type needs a slot but
// was given none, so a misconfigured service cannot start a pod that would
// sign in somewhere the pool does not track (AC-L4, AC-L5).
func (o *ClientOrchestrator) attachAuthSlot(container *corev1.Container, workloadType session.WorkloadType, slot *int) ([]corev1.Volume, error) {
	pool, uses := AuthPoolFor(workloadType, o.claudeAuthMode)
	switch {
	case !uses && slot == nil:
		return nil, nil
	case !uses:
		return nil, fmt.Errorf("workload type %q takes no auth slot", workloadType)
	case slot == nil || *slot < 0:
		return nil, fmt.Errorf("workload type %q needs an auth slot from the %s pool", workloadType, pool)
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: AuthSlotDirEnvVar, Value: AuthSlotMountPath})
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: AuthSlotVolumeName, MountPath: AuthSlotMountPath,
	})
	return []corev1.Volume{{
		Name: AuthSlotVolumeName,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: AuthSlotClaimName(o.namespace, pool, *slot),
		}},
	}}, nil
}
