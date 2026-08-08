// Package service wires the adapter ports together into a concrete
// session.Manager. Non-active access follows one uniform "resume-on-access"
// rule (AC-C2/AC-C3): read, write and switch all bring the session to active
// first — promoting idle->active (AC-C1) or restoring snapshot->active (AC-B2)
// — and then serve from the live pod. Becoming active includes proving the
// selected workload agent is reachable (AC-D1/E1) before the state lands.
// Read and write then use the workload-neutral AgentClient: shell writes feed
// stdin, Claude writes enqueue prompts, and reads return offset-cursored output
// deltas (AC-D2/D3, AC-E2/E3). The idle->snapshot *trigger* is
// service.IdleReaper (AC-B1); only
// its finer TODO(policy) timing — grace periods / per-session overrides —
// stays a deferred decision.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/criu"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
	"github.com/dlddu/session-platform/control-plane/internal/store"
)

// Service is the concrete Manager. It owns no workload itself (AC-A1) — every
// pod operation goes through the orchestrator, every state mutation through the
// store, every archive through the workload checkpointer, and every workload
// I/O byte through the agent client.
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

// Option customises workload-specific service behaviour.
type Option func(*Service)

// WithWorkloadCheckpointer installs the snapshot strategy for a workload. The
// shell strategy is the constructor's ckpt argument; claude-code supplies an
// agent filesystem-archive checkpointer even when CRIU is disabled (AC-E5).
func WithWorkloadCheckpointer(workload session.WorkloadType, ckpt criu.Checkpointer) Option {
	return func(s *Service) {
		if ckpt != nil {
			s.ckpts[workload] = ckpt
		}
	}
}

// WithLeaseRenewInterval overrides the heartbeat cadence for long snapshot
// transactions. Production uses five seconds for the store's 15-second Lease;
// tests use a shorter interval to exercise ownership loss deterministically.
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

// compile-time assertion that Service satisfies the port.
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
	// AC-E1: an omitted type creates a shell session (unchanged behaviour); an
	// unknown one is rejected before any pod is provisioned.
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

	pod, err := s.orch.Start(ctx, id, k8s.WorkloadSpec{Type: workload, Model: model})
	if err != nil {
		return nil, err
	}
	if err := s.orch.Reach(ctx, pod); err != nil {
		// A pod whose workload agent is unreachable must not become active;
		// reclaim it instead of leaking it (AC-A3 hygiene).
		_ = s.orch.Stop(ctx, pod)
		return nil, err
	}

	now := s.now()
	sess := &session.Session{
		ID:           id,
		WorkloadType: workload,
		Model:        model,
		Name:         name,
		State:        session.StateActive,
		Pod:          pod.Name,
		CreatedAt:    now,
		LastAccess:   now,
	}
	if err := s.store.Put(ctx, sess); err != nil {
		// best-effort rollback of the pod we just started
		_ = s.orch.Stop(ctx, pod)
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
// shared core of the uniform "resume-on-access" policy (AC-C2/AC-C3): an active
// session is served in place, an idle one is atomically promoted (AC-C1), and a
// snapshot is restored by its workload strategy (AC-B2). Switch (AC-C4) is just activate with no
// following read/write.
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
		// pod was reclaimed at freeze; restore the checkpoint into a fresh pod
		// and go active (AC-B2). Restore is lock-guarded for atomicity (AC-C1).
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
// offset, plus the nextOffset cursor (AC-C2, AC-D3/E3). Offset 0 replays the
// full session history; reads are non-consuming.
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

// Write validates workload-specific limits, brings the session active, and
// sends the payload to the agent (AC-C3, AC-D2/E2): shell payloads go to stdin
// while Claude payloads enter the serial prompt queue. The call returns after
// acceptance; output is recovered via Read.
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
// It shares the activate core with Read/Write so switching, reading and writing
// resume a session identically, and switching never breaks isolation (AC-A2).
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
	// reclaim the pod (AC-A3)
	if stopErr := s.orch.Stop(ctx, podRef); stopErr != nil {
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
	if err := s.orch.Stop(ctx, podRef); err != nil {
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
		if err := s.orch.Stop(ctx, k8s.PodRef{Name: txn.SourcePod}); err != nil {
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
	pod, err := s.orch.RestoreInto(ctx, id, checkpointRef(sess.Checkpoint), k8s.WorkloadSpec{Type: workload, Model: model})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.stopPodBestEffort(pod)
		return nil, err
	}
	if err := ckpt.Restore(ctx, sess.Checkpoint, pod); err != nil {
		s.stopPodBestEffort(pod)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.stopPodBestEffort(pod)
		return nil, err
	}
	// Same bar as Create: the restored session only counts as active once its
	// workload agent is reachable again (AC-D1/E1).
	if err := s.orch.Reach(ctx, pod); err != nil {
		s.stopPodBestEffort(pod)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		s.stopPodBestEffort(pod)
		return nil, err
	}
	now := s.now()
	next := *sess
	next.State = session.StateActive
	next.Pod = pod.Name
	next.Model = model
	next.Checkpoint = nil
	next.LastAccess = now
	if err := s.store.CompareAndSwapSession(
		ctx, id, token, session.StateSnapshot, sess.SnapshotTransaction, &next,
	); err != nil {
		fresh, getErr := s.getSessionBestEffort(id)
		if getErr == nil &&
			fresh.State == session.StateActive &&
			fresh.Pod == pod.Name &&
			fresh.Checkpoint == nil &&
			fresh.SnapshotTransaction == nil {
			// The API server committed the aggregate update but the response was
			// lost. Deleting pod here would corrupt an active session.
			return fresh, nil
		}
		if getErr == nil ||
			errors.Is(err, session.ErrConflict) ||
			errors.Is(getErr, session.ErrNotFound) {
			// The authoritative record proves our target was not committed, or
			// the CAS was definitively rejected.
			s.stopPodBestEffort(pod)
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

func (s *Service) stopPodBestEffort(pod k8s.PodRef) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.orch.Stop(cleanupCtx, pod)
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

	pods := []string{sess.Pod}
	if txn := sess.SnapshotTransaction; txn != nil && txn.SourcePod != sess.Pod {
		pods = append(pods, txn.SourcePod)
	}
	for _, pod := range pods {
		if pod == "" {
			continue
		}
		if err := s.orch.Stop(ctx, k8s.PodRef{Name: pod}); err != nil {
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
