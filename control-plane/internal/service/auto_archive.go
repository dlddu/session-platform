package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AutoArchives is the durable side of AC-B5's automatic archive: the pod
// uploads straight to the session's archive key through a URL signed for it,
// naming itself as the object's writer.
type AutoArchives interface {
	AutoArchiveTarget(ctx context.Context, sessionID, pod string) (url string, expires time.Time, headers map[string]string, err error)
	ArchiveRef(sessionID string) (string, error)
	ArchiveWriter(ctx context.Context, ref string) (writer string, exists bool, err error)
}

// WithAutoArchives turns on AC-B5 for every type whose archive gate is on.
func WithAutoArchives(a AutoArchives) Option {
	return func(s *Service) { s.autoArchives = a }
}

// autoArchiveRenewBefore is the remaining URL lifetime at or below which a new
// URL replaces it.
const autoArchiveRenewBefore = time.Hour

// autoArchiveRepushAfter re-sends a still valid URL, for an agent that lost it
// in a container restart.
const autoArchiveRepushAfter = 10 * time.Minute

const autoArchiveCallTimeout = 10 * time.Second

type autoArchiveTargeter interface {
	SetAutoArchiveTarget(ctx context.Context, pod, url string, expires time.Time, headers map[string]string) error
	ClearAutoArchiveTarget(ctx context.Context, pod string) error
}

type autoArchiveTarget struct {
	url      string
	headers  map[string]string
	expires  time.Time
	pushedAt time.Time
}

type autoArchiveTargets struct {
	mu   sync.Mutex
	pods map[string]autoArchiveTarget
}

func (t *autoArchiveTargets) get(pod string) (autoArchiveTarget, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	target, ok := t.pods[pod]
	return target, ok
}

func (t *autoArchiveTargets) set(pod string, target autoArchiveTarget) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pods == nil {
		t.pods = map[string]autoArchiveTarget{}
	}
	t.pods[pod] = target
}

func (t *autoArchiveTargets) forget(pod string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pods, pod)
}

func (t *autoArchiveTargets) keepOnly(pods map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for pod := range t.pods {
		if !pods[pod] {
			delete(t.pods, pod)
		}
	}
}

func (s *Service) autoArchiveFor(workload session.WorkloadType) bool {
	if s.autoArchives == nil {
		return false
	}
	_, err := s.checkpointerFor(workload)
	return err == nil
}

// armAutoArchive gives the session's pod an upload URL, signing a new one when
// the one it holds has autoArchiveRenewBefore or less left.
func (s *Service) armAutoArchive(ctx context.Context, sess *session.Session) error {
	if sess.Pod == "" || sess.State == session.StateSnapshot || !s.autoArchiveFor(sess.WorkloadType) {
		return nil
	}
	targeter, ok := s.agent.(autoArchiveTargeter)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, autoArchiveCallTimeout)
	defer cancel()
	now := s.now()
	target, held := s.autoTargets.get(sess.Pod)
	if held && target.expires.Sub(now) > autoArchiveRenewBefore && now.Sub(target.pushedAt) < autoArchiveRepushAfter {
		return nil
	}
	if !held || target.expires.Sub(now) <= autoArchiveRenewBefore {
		url, expires, headers, err := s.autoArchives.AutoArchiveTarget(ctx, sess.ID, sess.Pod)
		if err != nil {
			return err
		}
		target = autoArchiveTarget{url: url, headers: headers, expires: expires}
	}
	if err := targeter.SetAutoArchiveTarget(ctx, sess.Pod, target.url, target.expires, target.headers); err != nil {
		return err
	}
	target.pushedAt = now
	s.autoTargets.set(sess.Pod, target)
	return nil
}

func (s *Service) armAutoArchiveBestEffort(sess *session.Session) {
	if err := s.armAutoArchive(context.Background(), sess); err != nil {
		slog.Warn("automatic archive URL not delivered; the next scan retries", "session", sess.ID, "pod", sess.Pod, "err", err)
	}
}

// disarmAutoArchiveBestEffort takes the URL back from a pod the control plane
// is about to reclaim after a committed snapshot, so the pod does not archive
// the same state again. A pod that keeps its URL only uploads a duplicate.
func (s *Service) disarmAutoArchiveBestEffort(sess *session.Session, pod string) {
	if pod == "" || !s.autoArchiveFor(sess.WorkloadType) {
		return
	}
	s.autoTargets.forget(pod)
	targeter, ok := s.agent.(autoArchiveTargeter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoArchiveCallTimeout)
	defer cancel()
	if err := targeter.ClearAutoArchiveTarget(ctx, pod); err != nil {
		slog.Warn("automatic archive URL not withdrawn; the pod may upload a duplicate", "session", sess.ID, "pod", pod, "err", err)
	}
}

// RefreshAutoArchiveTargets keeps every live pod holding a valid URL. The idle
// reaper calls it on each scan.
func (s *Service) RefreshAutoArchiveTargets(ctx context.Context, sessions []*session.Session) {
	live := map[string]bool{}
	for _, sess := range sessions {
		if sess.Pod == "" || sess.State == session.StateSnapshot {
			continue
		}
		live[sess.Pod] = true
		if err := s.armAutoArchive(ctx, sess); err != nil {
			// Debug: a pod created before AC-B5 refuses every scan's push.
			slog.Debug("automatic archive URL refresh failed", "session", sess.ID, "pod", sess.Pod, "err", err)
		}
	}
	s.autoTargets.keepOnly(live)
}

// recordAutoArchiveOnDelete leaves the deleted session's manifest naming the
// archive its pod is about to write, keeping the last snapshot's archive as
// the fallback (AC-B4, AC-B5).
func (s *Service) recordAutoArchiveOnDelete(ctx context.Context, sess *session.Session) {
	if s.manifests == nil || sess.Pod == "" || sess.State == session.StateSnapshot || !s.autoArchiveFor(sess.WorkloadType) {
		return
	}
	ref, err := s.autoArchives.ArchiveRef(sess.ID)
	if err != nil {
		slog.Warn("automatic archive not recorded for a deleted session", "session", sess.ID, "err", err)
		return
	}
	m := session.ManifestFor(sess, nil)
	if prev, err := s.manifests.GetManifest(ctx, sess.ID); err == nil {
		m.Checkpoint = prev.Checkpoint
	} else if !errors.Is(err, session.ErrNotFound) {
		slog.Warn("previous manifest unreadable; the deleted session keeps only its automatic archive", "session", sess.ID, "err", err)
	}
	m.AutoArchive = &session.Checkpoint{
		Ref: ref, CreatedAt: s.now(), Writer: session.PodWriter(sess.Pod),
		Reclaimed: "pod " + sess.Pod + " (automatic archive)",
	}
	if err := s.manifests.PutManifest(ctx, m); err != nil {
		slog.Warn("automatic archive not recorded for a deleted session", "session", sess.ID, "err", err)
	}
}

type lostPodOutcome int

const (
	podNotLost lostPodOutcome = iota
	lostPodAdopted
	lostPodTerminated
)

// handleLostPod resolves a session whose workload pod no longer exists (AC-C5):
// it becomes a snapshot of the pod's automatic archive when one was written
// (AC-B5), and is terminated otherwise.
func (s *Service) handleLostPod(ctx context.Context, sess *session.Session) (lostPodOutcome, error) {
	if sess.Pod == "" || sess.State == session.StateSnapshot {
		return podNotLost, nil
	}
	gone, err := s.orch.Gone(ctx, k8s.PodRef{Name: sess.Pod})
	if err != nil || !gone {
		return podNotLost, err
	}
	adopted, err := s.adoptAutoArchive(ctx, sess.ID, sess.Pod)
	if err != nil {
		return podNotLost, err
	}
	if adopted {
		return lostPodAdopted, nil
	}
	if err := s.terminate(context.WithoutCancel(ctx), sess.ID, false); err != nil && !errors.Is(err, session.ErrNotFound) {
		return podNotLost, err
	}
	return lostPodTerminated, nil
}

// ReconcileLostPod applies handleLostPod to one session for the idle reaper,
// reporting whether the session's pod was gone.
func (s *Service) ReconcileLostPod(ctx context.Context, sess *session.Session) (bool, error) {
	outcome, err := s.handleLostPod(ctx, sess)
	return outcome != podNotLost, err
}

// adoptAutoArchive freezes the session onto lostPod's automatic archive when
// the session's archive key holds one lostPod wrote. It reports false,
// changing nothing, otherwise — including when the key still holds an older
// snapshot because the pod's upload never landed.
func (s *Service) adoptAutoArchive(ctx context.Context, id, lostPod string) (adopted bool, retErr error) {
	if s.autoArchives == nil {
		return false, nil
	}
	token, err := newID()
	if err != nil {
		return false, err
	}
	if err := s.store.Lock(ctx, id, token); err != nil {
		return false, err
	}
	defer s.unlockBestEffort(id, token)
	operationCtx, stopRenewal := s.renewLeaseContext(ctx, id, token)
	ctx = operationCtx
	defer func() {
		if err := stopRenewal(); err != nil {
			adopted = false
			retErr = errors.Join(retErr, err)
		}
	}()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if sess.Pod != lostPod || sess.State == session.StateSnapshot || !s.autoArchiveFor(sess.WorkloadType) {
		return false, nil
	}
	if txn := sess.SnapshotTransaction; txn != nil && txn.Phase != session.SnapshotPhasePreparing {
		// A committed snapshot is already durable; its recovery finishes it.
		return false, nil
	}
	ref, err := s.autoArchives.ArchiveRef(id)
	if err != nil {
		return false, err
	}
	writer, exists, err := s.autoArchives.ArchiveWriter(ctx, ref)
	if err != nil || !exists || writer != session.PodWriter(lostPod) {
		return false, err
	}
	cp := &session.Checkpoint{
		Ref: ref, CreatedAt: s.now(), Writer: writer,
		Reclaimed: "pod " + lostPod + " (automatic archive)",
	}
	if s.manifests != nil {
		if err := s.manifests.PutManifest(ctx, session.ManifestFor(sess, cp)); err != nil {
			return false, err
		}
	}
	next := *sess
	next.State = session.StateSnapshot
	next.Pod = ""
	next.AuxiliaryPods = nil
	next.Checkpoint = cp
	next.SnapshotTransaction = nil
	if err := s.store.CompareAndSwapSession(ctx, id, token, sess.State, sess.SnapshotTransaction, &next); err != nil {
		return false, err
	}
	s.autoTargets.forget(lostPod)
	s.stopPodsBestEffort(sessionPodRefs(sess))
	if sess.WorkloadType != session.WorkloadTypeCodexManager {
		s.releaseAuthSlotBestEffort(sess.WorkloadType, id, nil)
	}
	return true, nil
}
