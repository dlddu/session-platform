package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// autoAgent fails workload I/O for the pods in dead and records the
// automatic-archive URLs pushed to and withdrawn from each pod.
type autoAgent struct {
	mu      sync.Mutex
	dead    map[string]bool
	pushed  map[string][]string
	cleared []string
}

func newAutoAgent() *autoAgent {
	return &autoAgent{dead: map[string]bool{}, pushed: map[string][]string{}}
}

func (a *autoAgent) fail(pod string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dead[pod] {
		return errors.New("dial tcp: no route to host")
	}
	return nil
}

func (a *autoAgent) kill(pod string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dead[pod] = true
}

func (a *autoAgent) Write(_ context.Context, pod, _, _ string) error { return a.fail(pod) }
func (a *autoAgent) Read(_ context.Context, pod string, offset int64) (string, int64, error) {
	if err := a.fail(pod); err != nil {
		return "", 0, err
	}
	return "out", offset + 3, nil
}
func (a *autoAgent) Stream(context.Context, string, int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (a *autoAgent) RefreshPlugins(context.Context, string) (session.PluginChanges, error) {
	return session.PluginChanges{}, nil
}

func (a *autoAgent) SetAutoArchiveTarget(_ context.Context, pod, url string, _ time.Time, headers map[string]string) error {
	if headers["X-Amz-Meta-Writer"] != session.PodWriter(pod) {
		return fmt.Errorf("pushed headers %v do not name pod %s", headers, pod)
	}
	if err := a.fail(pod); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pushed[pod] = append(a.pushed[pod], url)
	return nil
}

func (a *autoAgent) ClearAutoArchiveTarget(_ context.Context, pod string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleared = append(a.cleared, pod)
	return nil
}

func (a *autoAgent) pushes(pod string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.pushed[pod]...)
}

// memAutoArchives signs numbered URLs and keeps the writer of each
// session's one archive key.
type memAutoArchives struct {
	mu      sync.Mutex
	now     func() time.Time
	signed  int
	writers map[string]string
}

func (m *memAutoArchives) AutoArchiveTarget(_ context.Context, id, pod string) (string, time.Time, map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signed++
	return fmt.Sprintf("https://bucket.s3/sessions/%s/checkpoint.tar?sig=%d", id, m.signed), m.now().Add(2 * time.Hour),
		map[string]string{"X-Amz-Meta-Writer": session.PodWriter(pod)}, nil
}

func (m *memAutoArchives) ArchiveRef(id string) (string, error) {
	return "s3://bucket/sessions/" + id + "/checkpoint.tar", nil
}

func (m *memAutoArchives) ArchiveWriter(_ context.Context, ref string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writer, ok := m.writers[ref]
	return writer, ok, nil
}

// write overwrites the session's archive key as writer would.
func (m *memAutoArchives) write(id, writer string) string {
	ref, _ := m.ArchiveRef(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writers[ref] = writer
	return ref
}

func (m *memAutoArchives) upload(id, pod string) string { return m.write(id, session.PodWriter(pod)) }

func (m *memAutoArchives) signatures() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.signed
}

// restoreRecorder is an enabled checkpointer that remembers what it restored.
type restoreRecorder struct {
	*archive.StubCheckpointer
	mu       sync.Mutex
	restored []string
}

func (r *restoreRecorder) Restore(ctx context.Context, cp *session.Checkpoint, into k8s.PodRef) error {
	r.mu.Lock()
	r.restored = append(r.restored, cp.Ref)
	r.mu.Unlock()
	return r.StubCheckpointer.Restore(ctx, cp, into)
}

func (r *restoreRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.restored) == 0 {
		return ""
	}
	return r.restored[len(r.restored)-1]
}

type autoFixture struct {
	svc       *service.Service
	orch      *k8s.StubOrchestrator
	store     *configmap.Store
	agent     *autoAgent
	archives  *memAutoArchives
	manifests *memManifests
	ckpt      *restoreRecorder
	clock     time.Time
	clockMu   sync.Mutex
}

func newAutoFixture() *autoFixture {
	f := &autoFixture{
		orch:      k8s.NewStubOrchestrator("sessions"),
		store:     configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		agent:     newAutoAgent(),
		manifests: newMemManifests(),
		ckpt:      &restoreRecorder{StubCheckpointer: archive.NewStubCheckpointer(true)},
		clock:     time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC),
	}
	f.archives = &memAutoArchives{now: f.now, writers: map[string]string{}}
	f.svc = service.New(f.orch, f.store, f.ckpt, f.agent,
		service.WithSessionManifests(f.manifests),
		service.WithAutoArchives(f.archives),
		service.WithClock(f.now),
	)
	return f
}

func (f *autoFixture) now() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.clock
}

func (f *autoFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *autoFixture) create(t *testing.T, name string) *session.Session {
	t.Helper()
	sess, err := f.svc.Create(context.Background(), session.CreateRequest{Name: name})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sess
}

// The pod vanishes underneath the session, the way a spot reclaim takes it.
func (f *autoFixture) losePod(t *testing.T, sess *session.Session, uploaded bool) {
	t.Helper()
	if uploaded {
		f.archives.upload(sess.ID, sess.Pod)
	}
	f.agent.kill(sess.Pod)
	if err := f.orch.Stop(context.Background(), k8s.PodRef{Name: sess.Pod}); err != nil {
		t.Fatalf("lose pod: %v", err)
	}
}

// AC-B5: a created pod is handed its upload URL at once.
func TestAutoArchiveURLIsDeliveredOnCreate(t *testing.T) {
	f := newAutoFixture()
	sess := f.create(t, "armed")
	got := f.agent.pushes(sess.Pod)
	if len(got) != 1 || !strings.Contains(got[0], "/sessions/"+sess.ID+"/checkpoint.tar") {
		t.Fatalf("pushed URLs = %v, want one for the session's pod", got)
	}
}

// AC-B5: the URL is re-sent while valid and replaced once an hour or less of
// it remains.
func TestAutoArchiveURLIsRenewedBeforeItExpires(t *testing.T) {
	ctx := context.Background()
	f := newAutoFixture()
	sess := f.create(t, "renewed")
	list := func() []*session.Session {
		all, err := f.svc.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return all
	}

	f.advance(5 * time.Minute)
	f.svc.RefreshAutoArchiveTargets(ctx, list())
	if n := len(f.agent.pushes(sess.Pod)); n != 1 {
		t.Fatalf("pushes 5m in = %d, want 1 (nothing due)", n)
	}

	f.advance(30 * time.Minute)
	f.svc.RefreshAutoArchiveTargets(ctx, list())
	pushes := f.agent.pushes(sess.Pod)
	if len(pushes) != 2 || pushes[1] != pushes[0] || f.archives.signatures() != 1 {
		t.Fatalf("35m in: pushes=%v signatures=%d, want the same URL re-sent", pushes, f.archives.signatures())
	}

	f.advance(30 * time.Minute) // 65m in: 55m of the URL left
	f.svc.RefreshAutoArchiveTargets(ctx, list())
	pushes = f.agent.pushes(sess.Pod)
	if len(pushes) != 3 || pushes[2] == pushes[1] || f.archives.signatures() != 2 {
		t.Fatalf("65m in: pushes=%v signatures=%d, want a newly signed URL", pushes, f.archives.signatures())
	}
}

// AC-B5 + AC-C5: a session whose pod was lost after writing its automatic
// archive is frozen onto that archive and the read is served from a restored
// pod instead of a 404.
func TestLostPodWithAutomaticArchiveIsRestored(t *testing.T) {
	for _, op := range []string{"read", "write"} {
		t.Run(op, func(t *testing.T) {
			ctx := context.Background()
			f := newAutoFixture()
			sess := f.create(t, "spot-"+op)
			f.losePod(t, sess, true)
			want, _ := f.archives.ArchiveRef(sess.ID)

			var err error
			if op == "read" {
				_, err = f.svc.Read(ctx, sess.ID, 0)
			} else {
				_, err = f.svc.Write(ctx, sess.ID, "hello", session.WriteOptions{})
			}
			if err != nil {
				t.Fatalf("%s after losing the pod: %v", op, err)
			}
			got, err := f.svc.Get(ctx, sess.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.State != session.StateActive || got.Pod == "" || got.Pod == sess.Pod {
				t.Fatalf("session = %+v, want active in a new pod", got)
			}
			if f.ckpt.last() != want {
				t.Fatalf("restored %q, want the automatic archive %q", f.ckpt.last(), want)
			}
			m, err := f.manifests.GetManifest(ctx, sess.ID)
			if err != nil || m.Checkpoint.Ref != want {
				t.Fatalf("manifest = %+v, %v; want it to name the automatic archive", m, err)
			}
		})
	}
}

// AC-C5 still holds when the pod left no archive.
func TestLostPodWithoutAutomaticArchiveIsNotFound(t *testing.T) {
	ctx := context.Background()
	f := newAutoFixture()
	sess := f.create(t, "no-upload")
	f.losePod(t, sess, false)
	if _, err := f.svc.Read(ctx, sess.ID, 0); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("read = %v, want ErrNotFound", err)
	}
	if _, err := f.store.Get(ctx, sess.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("record = %v, want removed", err)
	}
}

// AC-B5: the idle reaper resolves a lost pod without waiting for a client.
func TestReaperAdoptsALostPodsAutomaticArchive(t *testing.T) {
	ctx := context.Background()
	f := newAutoFixture()
	kept := f.create(t, "kept")
	lost := f.create(t, "lost")
	gone := f.create(t, "gone")
	f.losePod(t, lost, true)
	f.losePod(t, gone, false)

	reaper := service.NewIdleReaper(f.svc, session.MaxIdle, time.Minute, f.now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := reaper.ScanOnce(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got, err := f.svc.Get(ctx, lost.ID); err != nil || got.State != session.StateSnapshot || got.Pod != "" {
		t.Fatalf("lost session = %+v, %v; want a snapshot of its archive", got, err)
	}
	if _, err := f.svc.Get(ctx, gone.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("session without archive = %v, want removed (AC-C5)", err)
	}
	if got, err := f.svc.Get(ctx, kept.ID); err != nil || got.State != session.StateActive {
		t.Fatalf("live session = %+v, %v; want untouched", got, err)
	}
}

// AC-B5: a committed snapshot takes the URL back before reclaiming the pod, so
// the pod does not archive the same state a second time.
func TestSnapshotWithdrawsTheAutomaticArchiveURL(t *testing.T) {
	f := newAutoFixture()
	sess := f.create(t, "frozen")
	if _, err := f.svc.Snapshot(context.Background(), sess.ID); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	f.agent.mu.Lock()
	cleared := append([]string(nil), f.agent.cleared...)
	f.agent.mu.Unlock()
	if len(cleared) != 1 || cleared[0] != sess.Pod {
		t.Fatalf("withdrawn from %v, want %s", cleared, sess.Pod)
	}
}

// AC-B4 + AC-B5: a deleted session's manifest names its pod's automatic
// archive and keeps the last snapshot as the fallback; recover prefers the
// automatic archive when it was written.
func TestDeletedSessionRecoversFromItsAutomaticArchive(t *testing.T) {
	ctx := context.Background()
	f := newAutoFixture()
	sess := f.create(t, "deleted")
	frozen, err := f.svc.Snapshot(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapRef := frozen.Checkpoint.Ref
	live, err := f.svc.Restore(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Terminate(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	m, err := f.manifests.GetManifest(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	autoRef, _ := f.archives.ArchiveRef(sess.ID)
	if m.AutoArchive == nil || m.AutoArchive.Ref != autoRef || m.AutoArchive.Writer != session.PodWriter(live.Pod) || m.Checkpoint.Ref != snapRef {
		t.Fatalf("manifest = %+v, want auto %q over snapshot %q", m, autoRef, snapRef)
	}

	t.Run("key still holding an older write falls back to the snapshot", func(t *testing.T) {
		f.archives.write(sess.ID, "generation:older")
		if _, err := f.svc.Recover(ctx, sess.ID, nil); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if f.ckpt.last() != snapRef {
			t.Fatalf("restored %q, want the snapshot %q", f.ckpt.last(), snapRef)
		}
		again, err := f.svc.Get(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Terminate(ctx, sess.ID); err != nil {
			t.Fatal(err)
		}
		f.archives.mu.Lock()
		delete(f.archives.writers, autoRef)
		f.archives.mu.Unlock()
		live = again
	})
	t.Run("upload missing falls back to the snapshot", func(t *testing.T) {
		if _, err := f.svc.Recover(ctx, sess.ID, nil); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if f.ckpt.last() != snapRef {
			t.Fatalf("restored %q, want the snapshot %q", f.ckpt.last(), snapRef)
		}
	})
	t.Run("upload present wins", func(t *testing.T) {
		again, err := f.svc.Get(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Terminate(ctx, sess.ID); err != nil {
			t.Fatal(err)
		}
		autoRef := f.archives.upload(sess.ID, again.Pod)
		if _, err := f.svc.Recover(ctx, sess.ID, nil); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if f.ckpt.last() != autoRef {
			t.Fatalf("restored %q, want the automatic archive %q", f.ckpt.last(), autoRef)
		}
	})
}

// AC-B4 + AC-B5: a session deleted before it was ever frozen is recoverable
// only when its pod uploaded.
func TestNeverFrozenDeletedSessionNeedsItsUpload(t *testing.T) {
	ctx := context.Background()
	f := newAutoFixture()
	sess := f.create(t, "never-frozen")
	if err := f.svc.Terminate(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Recover(ctx, sess.ID, nil); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("recover without upload = %v, want ErrNotFound", err)
	}
	f.archives.upload(sess.ID, sess.Pod)
	got, err := f.svc.Recover(ctx, sess.ID, nil)
	if err != nil {
		t.Fatalf("recover with upload: %v", err)
	}
	if got.State != session.StateActive || got.Pod == sess.Pod {
		t.Fatalf("recovered = %+v, want active in a new pod", got)
	}
}

// AC-B5: the key still holding the last snapshot (the pod's upload never
// landed) is not taken for the lost pod's archive; AC-C5 applies.
func TestLostPodIsNotAdoptedFromAnOlderWrite(t *testing.T) {
	ctx := context.Background()
	f := newAutoFixture()
	sess := f.create(t, "stale")
	f.archives.write(sess.ID, "generation:earlier")
	f.losePod(t, sess, false)
	if _, err := f.svc.Read(ctx, sess.ID, 0); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("read = %v, want ErrNotFound", err)
	}
}
