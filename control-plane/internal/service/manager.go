// Package service wires the adapter ports together into a concrete
// session.Manager: the uniform "resume-on-access" rule for non-active access
// (AC-C2/AC-C3), and IdleReaper as the idle->snapshot trigger (AC-B1).
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/store"
)

// Service is the concrete Manager. It owns no workload itself (AC-A1).
type Service struct {
	orch               k8s.PodOrchestrator
	store              store.StateStore
	ckpts              map[session.WorkloadType]criu.Checkpointer
	agent              agent.Client
	now                func() time.Time // injectable clock for tests
	leaseRenewInterval time.Duration
}

const defaultLeaseRenewInterval = 5 * time.Second

type checkpointAborter interface {
	AbortCheckpoint(context.Context, k8s.PodRef, *session.Checkpoint) error
}

type generationCheckpointer interface {
	CheckpointWithGeneration(context.Context, k8s.PodRef, string) (*session.Checkpoint, error)
}

type agentCheckpointAborter interface {
	AbortCheckpoint(context.Context, string, string) error
}

// approvalWaitReporter is the agent client's answer to "is this session
// blocked on a human right now" (AC-F3). Optional, because only the
// approval-gated type can be.
type approvalWaitReporter interface {
	AwaitingApproval(context.Context, string) (bool, error)
}

// Option customises workload-specific service behaviour.
type Option func(*Service)

// WithWorkloadCheckpointer installs the snapshot strategy for a workload.
// claude-code supplies an agent filesystem-archive checkpointer even when CRIU
// is disabled (AC-E5).
func WithWorkloadCheckpointer(workload session.WorkloadType, ckpt criu.Checkpointer) Option {
	return func(s *Service) {
		if ckpt != nil {
			s.ckpts[workload] = ckpt
		}
	}
}

// WithClock replaces the service clock. AC-F3's idle exception is a statement
// about time passing without a client — a test can only pin "the count did not
// advance while waiting, and did once the wait ended" if it owns the clock the
// refresh is stamped with (docs/test/approval-gated-workload.md scenario 5).
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithLeaseRenewInterval overrides the heartbeat cadence for long snapshot
// transactions. It has to stay well inside the store's Lease duration, or a
// still-running side effect outlives its own ownership token.
func WithLeaseRenewInterval(interval time.Duration) Option {
	return func(s *Service) {
		if interval > 0 {
			s.leaseRenewInterval = interval
		}
	}
}

// New builds a Service from its adapter ports.
func New(orch k8s.PodOrchestrator, store store.StateStore, ckpt criu.Checkpointer, agent agent.Client, opts ...Option) *Service {
	s := &Service{
		orch: orch, store: store, agent: agent,
		ckpts:              map[session.WorkloadType]criu.Checkpointer{session.WorkloadTypeShell: ckpt},
		now:                func() time.Time { return time.Now().UTC() },
		leaseRenewInterval: defaultLeaseRenewInterval,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ session.Manager = (*Service)(nil)

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// renewLeaseContext keeps a long snapshot's occupancy Lease alive. Losing the
// token cancels the operation context before it can make another irreversible
// phase transition; the durable transaction is then left for the new holder.
func (s *Service) renewLeaseContext(parent context.Context, id, token string) (context.Context, func() error) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	renewErr := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.leaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				// A wedged Kubernetes request must fail before the 15-second
				// Lease can expire underneath a still-running side effect.
				renewCtx, renewCancel := context.WithTimeout(ctx, s.leaseRenewInterval)
				err := s.store.Renew(renewCtx, id, token)
				renewCancel()
				if err != nil {
					// stopRenewal is normal shutdown, not ownership loss.
					select {
					case <-stop:
						return
					default:
					}
					if ctx.Err() != nil {
						return
					}
					renewErr <- err
					cancel()
					return
				}
			}
		}
	}()
	var stopped bool
	return ctx, func() error {
		if !stopped {
			close(stop)
			cancel()
			stopped = true
		}
		<-done
		select {
		case err := <-renewErr:
			return err
		default:
			return nil
		}
	}
}

// Create provisions a dedicated pod and registers the session as active
// (AC-A1, AC-A2). "Active" means more than pod Ready: the selected workload
// agent must pass the control-plane reachability check before the session is
// stored (AC-D1/E1).
func (s *Service) Create(ctx context.Context, req session.CreateRequest) (*session.Session, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, session.ErrInvalidInput
	}
	// Normalize before provisioning: a bad request must not leak a pod (AC-E1).
	workload, err := session.NormalizeWorkloadType(req.WorkloadType)
	if err != nil {
		return nil, err
	}
	model, err := session.NormalizeModel(workload, req.Model)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}

	pods, err := s.orch.Start(ctx, id, k8s.WorkloadSpec{Type: workload, Model: model})
	if err != nil {
		return nil, err
	}
	// Only the workload pod runs a session agent (AC-F4).
	if err := s.orch.Reach(ctx, pods.Workload); err != nil {
		// A pod whose workload agent is unreachable must not become active;
		// reclaim the whole set instead of leaking it (AC-A3 hygiene).
		_ = s.orch.Stop(ctx, pods.All()...)
		return nil, err
	}

	now := s.now()
	sess := &session.Session{
		ID:            id,
		WorkloadType:  workload,
		Model:         model,
		Name:          name,
		State:         session.StateActive,
		Pod:           pods.Workload.Name,
		AuxiliaryPods: pods.Names(),
		CreatedAt:     now,
		LastAccess:    now,
	}
	if err := s.store.Put(ctx, sess); err != nil {
		// best-effort rollback of every pod we just started
		_ = s.orch.Stop(ctx, pods.All()...)
		return nil, err
	}
	return sess, nil
}

func (s *Service) Get(ctx context.Context, id string) (*session.Session, error) {
	sess, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return normalizeStoredSession(sess)
}

func (s *Service) List(ctx context.Context) ([]*session.Session, error) {
	sessions, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	for i, sess := range sessions {
		sessions[i], err = normalizeStoredSession(sess)
		if err != nil {
			return nil, err
		}
	}
	return sessions, nil
}

func normalizeStoredSession(sess *session.Session) (*session.Session, error) {
	workload, err := session.NormalizeWorkloadType(sess.WorkloadType)
	if err != nil {
		return nil, err
	}
	model, err := session.NormalizeModel(workload, sess.Model)
	if err != nil {
		return nil, err
	}
	sess.WorkloadType = workload
	sess.Model = model
	return sess, nil
}

// activate ensures the target session is active so a read/write can be served
// from a live pod, and reports which branch brought it there. This is the
// shared core of the uniform "resume-on-access" policy (AC-C2/AC-C3/AC-C4).
func (s *Service) activate(ctx context.Context, id string) (*session.Session, string, error) {
	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if sess.SnapshotTransaction != nil {
		sess, err = s.recoverSnapshot(ctx, id)
		if err != nil {
			return nil, "", err
		}
	}
	switch sess.State {
	case session.StateActive:
		return sess, "active", nil
	case session.StateIdle:
		// idle still holds its pod; resume it to active atomically (AC-C1).
		if err := s.store.CompareAndSwapState(ctx, id, session.StateIdle, session.StateActive); err != nil {
			return nil, "", err
		}
		sess, err = s.Get(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return sess, "idle->active", nil
	case session.StateSnapshot:
		// pod was reclaimed at freeze; restore it into a fresh one (AC-B2).
		sess, err = s.Restore(ctx, id)
		if err != nil {
			return nil, "", err
		}
		return sess, "snapshot->restore", nil
	default:
		return nil, "", session.ErrInvalidState
	}
}

// Read brings the session active and returns workload output accumulated after
// offset, plus the nextOffset cursor (AC-C2, AC-D3/E3).
func (s *Service) Read(ctx context.Context, id string, offset int64) (*session.ReadResult, error) {
	sess, branch, err := s.activate(ctx, id)
	if err != nil {
		return nil, err
	}
	payload, next, err := s.agent.Read(ctx, sess.Pod, offset)
	if err != nil {
		return nil, err
	}
	s.touch(ctx, id)
	return &session.ReadResult{
		Session:    sess,
		Path:       dispatchPath(branch, "read"),
		Payload:    payload,
		NextOffset: next,
	}, nil
}

// Stream passively observes the retained pod's append-only output. Deliberately
// unlike Read, it neither promotes idle nor restores snapshots nor touches
// LastAccess — see the passive-stream note in docs/prd/state-api.md (AC-C2) and
// AC-B1's "SSE is not activity".
func (s *Service) Stream(ctx context.Context, id string, offset int64) (io.ReadCloser, error) {
	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.State == session.StateSnapshot || sess.Pod == "" {
		return nil, session.ErrInvalidState
	}
	return s.agent.Stream(ctx, sess.Pod, offset)
}

// Write validates workload-specific limits, brings the session active, and
// sends the payload to the agent (AC-C3, AC-D2/E2).
func (s *Service) Write(ctx context.Context, id, payload string) (*session.WriteResult, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.WorkloadType == session.WorkloadTypeClaudeCode && len(payload) > session.MaxClaudePromptBytes {
		return nil, session.ErrWorkloadPromptTooLarge
	}
	sess, branch, err := s.activate(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.agent.Write(ctx, sess.Pod, payload); err != nil {
		return nil, err
	}
	s.touch(ctx, id)
	return &session.WriteResult{
		Session: sess,
		Path:    dispatchPath(branch, "write"),
	}, nil
}

// Switch makes the target session active — promoting idle or restoring a
// snapshot as needed — and is a no-op for an already-active session (AC-C4).
func (s *Service) Switch(ctx context.Context, id string) (*session.Session, error) {
	sess, branch, err := s.activate(ctx, id)
	if err != nil {
		return nil, err
	}
	if branch == "idle->active" {
		// resuming from idle counts as an access; record it.
		return s.touchGet(ctx, id)
	}
	return sess, nil
}

// Snapshot checkpoints an active/idle session and reclaims its pod (AC-B1,
// AC-A3). Explicit snapshots have no idle precondition.
func (s *Service) Snapshot(ctx context.Context, id string) (*session.Session, error) {
	return s.snapshot(ctx, id, nil, nil)
}

// SnapshotIfIdle is the reaper-safe variant. It acquires the same occupancy
// Lease as Snapshot, reloads LastAccess under that Lease, and does nothing when
// a client accessed the session after cutoff.
func (s *Service) SnapshotIfIdle(ctx context.Context, id string, cutoff time.Time) (*session.Session, bool, error) {
	snapshotted := false
	sess, err := s.snapshot(ctx, id, &cutoff, &snapshotted)
	return sess, snapshotted, err
}

func (s *Service) snapshot(ctx context.Context, id string, idleCutoff *time.Time, snapshotted *bool) (result *session.Session, retErr error) {
	token, err := newID()
	if err != nil {
		return nil, err
	}
	if err := s.store.Lock(ctx, id, token); err != nil {
		return nil, err
	}
	defer s.unlockBestEffort(id, token)
	operationCtx, stopRenewal := s.renewLeaseContext(ctx, id, token)
	ctx = operationCtx
	defer func() {
		if err := stopRenewal(); err != nil {
			result = nil
			retErr = errors.Join(retErr, err)
		}
	}()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.SnapshotTransaction != nil {
		sess, err = s.recoverSnapshotLocked(ctx, sess, token)
		if err != nil {
			return nil, err
		}
	}
	if idleCutoff != nil {
		// Recovery CAS monotonically merges concurrent Touch values in storage;
		// reload so the cutoff decision observes that authoritative timestamp
		// rather than the caller's pre-recovery copy.
		sess, err = s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if sess.LastAccess.After(*idleCutoff) {
			return sess, nil
		}
		if s.holdingForApproval(ctx, sess) {
			// AC-F3's exception to AC-B1, and the only place the platform
			// advances lastAccess without a client having accessed anything.
			// Refreshing rather than merely declining to freeze is what the
			// requirement asks for and what makes the wait's end observable:
			// once the decision lands the refresh stops, and the ordinary
			// count resumes from here rather than from an hour ago.
			return s.touchGet(ctx, id)
		}
	}
	if sess.State == session.StateSnapshot {
		return sess, nil // already frozen
	}

	ckpt, err := s.checkpointerFor(sess.WorkloadType)
	if err != nil {
		return nil, err
	}
	if sess.WorkloadType == session.WorkloadTypeClaudeCode {
		generated, ok := ckpt.(generationCheckpointer)
		if !ok {
			return nil, errors.New("claude-code checkpoint strategy lacks durable generation support")
		}
		frozen, err := s.snapshotWithTransactionLocked(ctx, sess, token, ckpt, generated)
		if err == nil && snapshotted != nil {
			*snapshotted = true
		}
		return frozen, err
	}

	// Shell CRIU retains its existing single-call protocol. Unlike the Claude
	// archive barrier it cannot be rolled back with a durable generation.
	cp, err := ckpt.Checkpoint(ctx, k8s.PodRef{Name: sess.Pod, Namespace: ""})
	if err != nil {
		return nil, err
	}
	podRef := k8s.PodRef{Name: sess.Pod}
	if stopErr := s.orch.Stop(ctx, sessionPodRefs(sess)...); stopErr != nil {
		if aborter, ok := ckpt.(checkpointAborter); ok {
			abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if abortErr := aborter.AbortCheckpoint(abortCtx, podRef, cp); abortErr != nil {
				return nil, errors.Join(stopErr, abortErr)
			}
		}
		return nil, stopErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	next := *sess
	next.State = session.StateSnapshot
	next.Pod = ""
	next.AuxiliaryPods = nil
	next.Checkpoint = cp
	if err := s.store.CompareAndSwapSession(ctx, id, token, sess.State, sess.SnapshotTransaction, &next); err != nil {
		return nil, err
	}
	if snapshotted != nil {
		*snapshotted = true
	}
	return &next, nil
}

func (s *Service) snapshotWithTransactionLocked(
	ctx context.Context,
	sess *session.Session,
	generation string,
	ckpt criu.Checkpointer,
	generated generationCheckpointer,
) (*session.Session, error) {
	podRef := k8s.PodRef{Name: sess.Pod}
	next := *sess
	next.SnapshotTransaction = &session.SnapshotTransaction{
		Generation: generation,
		Owner:      generation,
		SourcePod:  sess.Pod,
		Phase:      session.SnapshotPhasePreparing,
	}
	// The generation and initial owner fence are durable before admission can
	// close. The single resourceVersion update also refuses to overwrite a
	// concurrently recovered transaction.
	if err := s.store.CompareAndSwapSession(
		ctx, sess.ID, generation, sess.State, sess.SnapshotTransaction, &next,
	); err != nil {
		return nil, err
	}
	sess = &next
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cp, err := generated.CheckpointWithGeneration(ctx, podRef, generation)
	if err != nil {
		if ctx.Err() != nil {
			// Renewal loss (or caller cancellation) leaves the durable prepare for
			// the next Lease holder; this stale owner must not abort underneath it.
			return nil, err
		}
		return nil, s.rollbackPreparingSnapshot(ctx, sess, ckpt, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Persist both the durable archive ref and the commit decision before issuing
	// Kubernetes DELETE. From this point recovery completes reclamation; it never
	// risks reopening admission after an ambiguously accepted DELETE.
	expectedTxn := cloneSnapshotTransaction(sess.SnapshotTransaction)
	committingTxn := cloneSnapshotTransaction(expectedTxn)
	committingTxn.Phase = session.SnapshotPhaseCommitting
	committingTxn.Checkpoint = cp
	next = *sess
	next.SnapshotTransaction = committingTxn
	if err := s.store.CompareAndSwapSession(
		ctx, sess.ID, generation, sess.State, expectedTxn, &next,
	); err != nil {
		if errors.Is(err, session.ErrConflict) {
			// A recovering holder claimed this prepare first. Abort this exact
			// generation and conditionally clear only our old owner record.
			return nil, s.rollbackPreparingSnapshot(ctx, sess, ckpt, err)
		}
		// The update result itself may be ambiguous. Leave admission closed and let
		// recovery re-read the authoritative phase before choosing abort or commit.
		return nil, err
	}
	sess = &next
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.orch.Stop(ctx, sessionPodRefs(sess)...); err != nil {
		// DELETE errors are outcome-ambiguous. The durable commit record makes a
		// retry safe, whereas aborting here could reopen a pod already terminating.
		return nil, err
	}
	return s.finalizeSnapshotLocked(ctx, sess, generation)
}

func (s *Service) recoverSnapshot(ctx context.Context, id string) (result *session.Session, retErr error) {
	token, err := newID()
	if err != nil {
		return nil, err
	}
	if err := s.store.Lock(ctx, id, token); err != nil {
		return nil, err
	}
	defer s.unlockBestEffort(id, token)
	operationCtx, stopRenewal := s.renewLeaseContext(ctx, id, token)
	ctx = operationCtx
	defer func() {
		if err := stopRenewal(); err != nil {
			result = nil
			retErr = errors.Join(retErr, err)
		}
	}()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.recoverSnapshotLocked(ctx, sess, token)
}

func (s *Service) recoverSnapshotLocked(ctx context.Context, sess *session.Session, token string) (*session.Session, error) {
	txn := sess.SnapshotTransaction
	if txn == nil {
		return sess, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if txn.Generation == "" || txn.SourcePod == "" {
		return nil, session.ErrInvalidState
	}
	switch txn.Phase {
	case session.SnapshotPhasePreparing:
		if sess.State == session.StateSnapshot {
			return nil, session.ErrInvalidState
		}
		// Claim the prepare in durable metadata before touching the agent. If the
		// expired holder resumes, its owner=A -> committing CAS now conflicts, so
		// it cannot delete the pod after this holder reopens admission.
		if txn.Owner != token {
			expectedTxn := cloneSnapshotTransaction(txn)
			claimedTxn := cloneSnapshotTransaction(txn)
			claimedTxn.Owner = token
			next := *sess
			next.SnapshotTransaction = claimedTxn
			if err := s.store.CompareAndSwapSession(
				ctx, sess.ID, token, sess.State, expectedTxn, &next,
			); err != nil {
				if errors.Is(err, session.ErrConflict) {
					fresh, getErr := s.Get(ctx, sess.ID)
					if getErr != nil {
						return nil, errors.Join(err, getErr)
					}
					return s.recoverSnapshotLocked(ctx, fresh, token)
				}
				return nil, err
			}
			sess, txn = &next, claimedTxn
		}
		if err := s.abortSnapshotGeneration(ctx, sess.WorkloadType, txn.SourcePod, txn.Generation); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		expectedTxn := cloneSnapshotTransaction(txn)
		next := *sess
		next.SnapshotTransaction = nil
		if err := s.store.CompareAndSwapSession(
			ctx, sess.ID, token, sess.State, expectedTxn, &next,
		); err != nil {
			return nil, err
		}
		return &next, nil
	case session.SnapshotPhaseCommitting:
		if txn.Checkpoint == nil || txn.Checkpoint.Ref == "" {
			return nil, session.ErrInvalidState
		}
		if err := s.orch.Stop(ctx, sessionReclaimRefs(sess, txn.SourcePod)...); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return s.finalizeSnapshotLocked(ctx, sess, token)
	default:
		return nil, session.ErrInvalidState
	}
}

func (s *Service) rollbackPreparingSnapshot(ctx context.Context, sess *session.Session, ckpt criu.Checkpointer, cause error) error {
	txn := sess.SnapshotTransaction
	if txn == nil {
		return cause
	}
	rollbackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.abortSnapshotGenerationWithCheckpointer(rollbackCtx, ckpt, txn.SourcePod, txn.Generation); err != nil {
		return errors.Join(cause, err)
	}
	if err := rollbackCtx.Err(); err != nil {
		return errors.Join(cause, err)
	}
	expectedTxn := cloneSnapshotTransaction(txn)
	next := *sess
	next.SnapshotTransaction = nil
	if err := s.store.CompareAndSwapSession(
		rollbackCtx, sess.ID, txn.Owner, sess.State, expectedTxn, &next,
	); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (s *Service) abortSnapshotGeneration(ctx context.Context, workload session.WorkloadType, pod, generation string) error {
	if ckpt := s.ckpts[workload]; ckpt != nil {
		if _, ok := ckpt.(checkpointAborter); ok {
			return s.abortSnapshotGenerationWithCheckpointer(ctx, ckpt, pod, generation)
		}
	}
	aborter, ok := s.agent.(agentCheckpointAborter)
	if !ok {
		return errors.New("snapshot generation abort is unavailable")
	}
	abortCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return aborter.AbortCheckpoint(abortCtx, pod, generation)
}

func (s *Service) abortSnapshotGenerationWithCheckpointer(ctx context.Context, ckpt criu.Checkpointer, pod, generation string) error {
	aborter, ok := ckpt.(checkpointAborter)
	if !ok {
		return errors.New("snapshot generation abort is unavailable")
	}
	abortCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return aborter.AbortCheckpoint(abortCtx, k8s.PodRef{Name: pod}, &session.Checkpoint{AbortToken: generation})
}

func (s *Service) finalizeSnapshotLocked(ctx context.Context, sess *session.Session, token string) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	txn := sess.SnapshotTransaction
	if txn == nil || txn.Phase != session.SnapshotPhaseCommitting || txn.Checkpoint == nil {
		return nil, session.ErrInvalidState
	}
	expectedTxn := cloneSnapshotTransaction(txn)
	next := *sess
	next.State = session.StateSnapshot
	next.Pod = ""
	next.AuxiliaryPods = nil
	next.Checkpoint = txn.Checkpoint
	next.SnapshotTransaction = nil
	if err := s.store.CompareAndSwapSession(
		ctx, sess.ID, token, sess.State, expectedTxn, &next,
	); err != nil {
		if errors.Is(err, session.ErrConflict) {
			fresh, getErr := s.Get(ctx, sess.ID)
			if getErr == nil && fresh.State == session.StateSnapshot && fresh.SnapshotTransaction == nil {
				return fresh, nil
			}
		}
		return nil, err
	}
	return &next, nil
}

func cloneSnapshotTransaction(txn *session.SnapshotTransaction) *session.SnapshotTransaction {
	if txn == nil {
		return nil
	}
	cloned := *txn
	if txn.Checkpoint != nil {
		checkpoint := *txn.Checkpoint
		cloned.Checkpoint = &checkpoint
	}
	return &cloned
}

// Restore restores a snapshotted session into a new pod and marks it active
// (AC-B2). Guarded by the per-session lock so concurrent restores are atomic
// (AC-C1).
func (s *Service) Restore(ctx context.Context, id string) (result *session.Session, retErr error) {
	token, err := newID()
	if err != nil {
		return nil, err
	}
	if err := s.store.Lock(ctx, id, token); err != nil {
		return nil, err
	}
	defer s.unlockBestEffort(id, token)
	operationCtx, stopRenewal := s.renewLeaseContext(ctx, id, token)
	ctx = operationCtx
	defer func() {
		if err := stopRenewal(); err != nil {
			result = nil
			retErr = errors.Join(retErr, err)
		}
	}()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.SnapshotTransaction != nil {
		sess, err = s.recoverSnapshotLocked(ctx, sess, token)
		if err != nil {
			return nil, err
		}
	}
	if sess.State != session.StateSnapshot {
		return sess, nil // already live; nothing to restore
	}

	// RestoreInto shapes the new pod as a restore target from the snapshot ref.
	// Shell restores resume the CRIU process tree and separately serialized
	// scrollback; filesystem restores unpack workspace, CLI state, and output.
	// Both preserve the byte stream, so a pre-snapshot read cursor remains valid.
	// The restore pod runs the session's own workload type: the type is fixed at
	// creation and a restore never changes it (AC-E1). Sessions stored before
	// the type axis existed carry no type, so normalize to the shell default.
	workload, err := session.NormalizeWorkloadType(sess.WorkloadType)
	if err != nil {
		return nil, err
	}
	model, err := session.NormalizeModel(workload, sess.Model)
	if err != nil {
		return nil, err
	}
	ckpt, err := s.checkpointerFor(workload)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pods, err := s.orch.RestoreInto(ctx, id, checkpointRef(sess.Checkpoint), k8s.WorkloadSpec{Type: workload, Model: model})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.stopPodsBestEffort(pods.All())
		return nil, err
	}
	// The archive is applied to the workload pod; auxiliary pods hold no
	// session state and are started fresh alongside it (AC-B2, AC-F4).
	if err := ckpt.Restore(ctx, sess.Checkpoint, pods.Workload); err != nil {
		s.stopPodsBestEffort(pods.All())
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.stopPodsBestEffort(pods.All())
		return nil, err
	}
	// Same bar as Create: the restored session only counts as active once its
	// workload agent is reachable again (AC-D1/E1).
	if err := s.orch.Reach(ctx, pods.Workload); err != nil {
		s.stopPodsBestEffort(pods.All())
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.stopPodsBestEffort(pods.All())
		return nil, err
	}
	now := s.now()
	next := *sess
	next.State = session.StateActive
	next.Pod = pods.Workload.Name
	next.AuxiliaryPods = pods.Names()
	next.Model = model
	next.Checkpoint = nil
	next.LastAccess = now
	if err := s.store.CompareAndSwapSession(
		ctx, id, token, session.StateSnapshot, sess.SnapshotTransaction, &next,
	); err != nil {
		fresh, getErr := s.getSessionBestEffort(id)
		if getErr == nil &&
			fresh.State == session.StateActive &&
			fresh.Pod == pods.Workload.Name &&
			fresh.Checkpoint == nil &&
			fresh.SnapshotTransaction == nil {
			// The API server committed the aggregate update but the response was
			// lost. Deleting these pods here would corrupt an active session.
			return fresh, nil
		}
		if getErr == nil ||
			errors.Is(err, session.ErrConflict) ||
			errors.Is(getErr, session.ErrNotFound) {
			// The authoritative record proves our target was not committed, or
			// the CAS was definitively rejected.
			s.stopPodsBestEffort(pods.All())
		}
		return nil, errors.Join(err, getErr)
	}
	return &next, nil
}

func (s *Service) getSessionBestEffort(id string) (*session.Session, error) {
	readCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Get(readCtx, id)
}

// stopPodsBestEffort reclaims a whole provisioning attempt, so a failed create
// or restore leaks no pod (AC-A3 hygiene).
func (s *Service) stopPodsBestEffort(pods []k8s.PodRef) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.orch.Stop(cleanupCtx, pods...)
}

// sessionPodRefs builds orchestrator refs for every pod a session record names,
// workload pod first (AC-A2's workload pod plus AC-F4's auxiliary pods). Refs
// rebuilt from stored state carry the name only; the orchestrator falls back to
// its own namespace.
func sessionPodRefs(sess *session.Session) []k8s.PodRef {
	names := sess.Pods()
	refs := make([]k8s.PodRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, k8s.PodRef{Name: name})
	}
	return refs
}

// sessionReclaimRefs is sessionPodRefs plus an extra pod the record may no
// longer name — the snapshot transaction's source pod, which a restore can
// have replaced. Passing both keeps reclamation idempotent without deleting the
// same pod twice.
func sessionReclaimRefs(sess *session.Session, extraPod string) []k8s.PodRef {
	refs := sessionPodRefs(sess)
	if extraPod == "" {
		return refs
	}
	for _, ref := range refs {
		if ref.Name == extraPod {
			return refs
		}
	}
	return append(refs, k8s.PodRef{Name: extraPod})
}

func (s *Service) unlockBestEffort(id, token string) {
	unlockCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.store.Unlock(unlockCtx, id, token)
}

// Terminate reclaims the pod (if any) and removes the session (AC-A3). It uses
// the same per-session Lease as snapshot/restore so deletion cannot race a
// restore target into existence or remove metadata underneath a snapshot
// transaction.
func (s *Service) Terminate(ctx context.Context, id string) (retErr error) {
	token, err := newID()
	if err != nil {
		return err
	}
	if err := s.store.Lock(ctx, id, token); err != nil {
		return err
	}
	defer s.unlockBestEffort(id, token)

	operationCtx, stopRenewal := s.renewLeaseContext(ctx, id, token)
	ctx = operationCtx
	deleteCommitted := false
	defer func() {
		// Once deletion commits, absence is authoritative; a simultaneous heartbeat
		// failure must not turn success into an outcome-ambiguous 500 response.
		if err := stopRenewal(); err != nil && !deleteCommitted {
			retErr = errors.Join(retErr, err)
		}
	}()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if sess.SnapshotTransaction != nil {
		expectedTxn := cloneSnapshotTransaction(sess.SnapshotTransaction)
		claimedTxn := cloneSnapshotTransaction(expectedTxn)
		claimedTxn.Owner = token
		next := *sess
		next.SnapshotTransaction = claimedTxn
		if err := s.store.CompareAndSwapSession(
			ctx, id, token, sess.State, expectedTxn, &next,
		); err != nil {
			return err
		}
		sess = &next
	}

	var sourcePod string
	if txn := sess.SnapshotTransaction; txn != nil {
		sourcePod = txn.SourcePod
	}
	for _, ref := range sessionReclaimRefs(sess, sourcePod) {
		if ref.Name == "" {
			continue
		}
		if err := s.orch.Stop(ctx, ref); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	retErr = s.store.Delete(ctx, id, token)
	deleteCommitted = retErr == nil
	return retErr
}

// holdingForApproval reports whether the idle count must be held for this
// session because it is waiting on a human (AC-F3).
//
// Three narrowings keep the blast radius at this one workload type. The
// caller reaches here only on the reaper's path (idleCutoff != nil), so a
// user-driven Snapshot still freezes a waiting session, as AC-F3's
// "동결·삭제와의 충돌" requires. Only approval-gated sessions are asked, so
// shell and claude-code keep AC-D5/AC-B1 unchanged and never pay for the
// call. And an error answers false: an agent that cannot be reached is one
// whose session should freeze on schedule, not one that pins a pod forever.
func (s *Service) holdingForApproval(ctx context.Context, sess *session.Session) bool {
	if sess.WorkloadType != session.WorkloadTypeApprovalGated || sess.Pod == "" {
		return false
	}
	reporter, ok := s.agent.(approvalWaitReporter)
	if !ok {
		return false
	}
	awaiting, err := reporter.AwaitingApproval(ctx, sess.Pod)
	if err != nil {
		slog.Warn("approval wait unreadable; applying the ordinary idle rule",
			"session", sess.ID, "pod", sess.Pod, "err", err)
		return false
	}
	return awaiting
}

// touch updates LastAccess without changing state.
func (s *Service) touch(ctx context.Context, id string) {
	_ = s.store.Touch(ctx, id, s.now())
}

func (s *Service) touchGet(ctx context.Context, id string) (*session.Session, error) {
	s.touch(ctx, id)
	return s.Get(ctx, id)
}

func (s *Service) checkpointerFor(workload session.WorkloadType) (criu.Checkpointer, error) {
	workload, err := session.NormalizeWorkloadType(workload)
	if err != nil {
		return nil, err
	}
	ckpt := s.ckpts[workload]
	if ckpt == nil || !ckpt.Enabled() {
		// Never reclaim a pod behind synthetic checkpoint metadata. This was a
		// data-loss path when the production CRIU gate was off.
		return nil, session.ErrCheckpointDisabled
	}
	return ckpt, nil
}

// checkpointRef returns the checkpoint archive reference the restore pod should
// resume from, or "" when there is no checkpoint (the orchestrator then
// provisions a plain fresh pod). Snapshot always records one before a session
// reaches StateSnapshot, so the nil case is defensive.
func checkpointRef(cp *session.Checkpoint) string {
	if cp == nil {
		return ""
	}
	return cp.Ref
}

// dispatchPath renders the ReadResult/WriteResult Path label for the branch that
// activate took. Active is served directly; non-active branches append the op,
// e.g. "idle->active->read" or "snapshot->restore->write".
func dispatchPath(branch, op string) string {
	if branch == "active" {
		return "active"
	}
	return branch + "->" + op
}
