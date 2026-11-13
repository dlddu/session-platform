package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

type memManifests struct {
	mu     sync.Mutex
	byID   map[string]session.Manifest
	putErr error
	puts   int
}

func newMemManifests() *memManifests {
	return &memManifests{byID: map[string]session.Manifest{}}
}

func (m *memManifests) PutManifest(_ context.Context, man session.Manifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.putErr != nil {
		return m.putErr
	}
	m.byID[man.ID] = man
	return nil
}

func (m *memManifests) GetManifest(_ context.Context, id string) (*session.Manifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	man, ok := m.byID[id]
	if !ok {
		return nil, session.ErrNotFound
	}
	return &man, nil
}

type recoverFixture struct {
	svc       *service.Service
	store     *configmap.Store
	orch      *k8s.StubOrchestrator
	manifests *memManifests
}

func newRecoverFixture(opts ...service.Option) recoverFixture {
	f := recoverFixture{
		store:     configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		orch:      k8s.NewStubOrchestrator("sessions"),
		manifests: newMemManifests(),
	}
	opts = append([]service.Option{
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, archive.NewStubCheckpointer(true)),
		service.WithSessionManifests(f.manifests),
	}, opts...)
	f.svc = service.New(f.orch, f.store, archive.NewStubCheckpointer(true), agent.NewStubClient(), opts...)
	return f
}

func TestRecoverBringsBackADeletedSessionFromItsLastSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newRecoverFixture()
	created, err := f.svc.Create(ctx, session.CreateRequest{
		Name:         "long-running-agent",
		WorkloadType: session.WorkloadTypeClaudeCode,
		Model:        "claude-opus-5-5",
		Plugins:      []string{"session-platform", "development"},
		Tenant:       "pocket-aide",
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := f.svc.Snapshot(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Terminate(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(ctx, created.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("get after terminate = %v, want not found", err)
	}

	var admitted *session.Session
	got, err := f.svc.Recover(ctx, created.ID, func(s *session.Session) error {
		admitted = s
		return nil
	})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if admitted == nil || admitted.Tenant != "pocket-aide" || admitted.State != session.StateSnapshot {
		t.Fatalf("admit saw %+v, want the frozen pocket-aide record", admitted)
	}
	if got.ID != created.ID || got.State != session.StateActive || got.Pod == "" || got.Pod == created.Pod {
		t.Fatalf("recovered = %+v, want the same id active in a new pod", got)
	}
	if got.WorkloadType != session.WorkloadTypeClaudeCode || got.Model != "claude-opus-5-5" ||
		got.Name != "long-running-agent" || got.Tenant != "pocket-aide" ||
		len(got.Plugins) != 2 || got.Plugins[1] != "development" ||
		!got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("recovered metadata = %+v, want the original session's", got)
	}
	if f.orch.RunningCount() != 1 {
		t.Fatalf("running pods = %d, want the one restored pod", f.orch.RunningCount())
	}
	man, _ := f.manifests.GetManifest(ctx, created.ID)
	if man.Checkpoint.Ref != frozen.Checkpoint.Ref || man.Checkpoint.AbortToken != "" {
		t.Fatalf("manifest checkpoint = %+v, want the committed ref without the abort token", man.Checkpoint)
	}
}

// The doubled memory option lives in the manifest, not only in the ConfigMap
// record the delete takes away, so a recovered session comes back on the same
// memory tier it was created with — pod spec included.
func TestRecoverKeepsTheDoubledMemoryTier(t *testing.T) {
	ctx := context.Background()
	f := newRecoverFixture()
	created, err := f.svc.Create(ctx, session.CreateRequest{Name: "hungry", DoubleMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Snapshot(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Terminate(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.Recover(ctx, created.ID, func(*session.Session) error { return nil })
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.DoubleMemory {
		t.Errorf("recovered session DoubleMemory = false, want true")
	}
	if !f.orch.SpecFor(created.ID).DoubleMemory {
		t.Errorf("recovery provisioned with DoubleMemory = false, want true")
	}
	man, err := f.manifests.GetManifest(ctx, created.ID)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !man.DoubleMemory {
		t.Errorf("manifest DoubleMemory = false, want true")
	}
}

func TestRecoverRefusesWhatItCannotOrMustNotRecover(t *testing.T) {
	ctx := context.Background()
	f := newRecoverFixture()
	live, err := f.svc.Create(ctx, session.CreateRequest{Name: "still-here"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Snapshot(ctx, live.ID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		id   string
		want error
	}{
		{"never snapshotted", "0123456789abcdef0123456789abcdef", session.ErrNotFound},
		{"malformed id", "../../etc", session.ErrNotFound},
		{"session still exists", live.ID, session.ErrAlreadyExists},
	}
	before, err := f.store.Get(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Recover(ctx, tc.id, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("recover err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, session.ErrConflict) || strings.Contains(err.Error(), "concurrently") {
				t.Fatalf("recover err = %q reads as a race", err)
			}
		})
	}
	stored, err := f.store.Get(ctx, live.ID)
	if err != nil || stored.State != session.StateSnapshot || !stored.LastAccess.Equal(before.LastAccess) ||
		stored.Checkpoint == nil || stored.Checkpoint.Ref != before.Checkpoint.Ref {
		t.Fatalf("existing session after refused recover = %+v, %v; want it untouched (lastAccess %v)", stored, err, before.LastAccess)
	}
}

func TestRecoverAdmitRejectionWritesNothing(t *testing.T) {
	ctx := context.Background()
	f := newRecoverFixture()
	created, err := f.svc.Create(ctx, session.CreateRequest{Name: "other-tenant", Tenant: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Snapshot(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Terminate(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Recover(ctx, created.ID, func(*session.Session) error { return session.ErrNotFound })
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("recover err = %v, want the admit error", err)
	}
	if _, err := f.store.Get(ctx, created.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("record after rejected recover: %v, want none", err)
	}
	if f.orch.RunningCount() != 0 {
		t.Fatalf("running pods = %d, want none", f.orch.RunningCount())
	}
}

func TestRecoverNeedsManifestsAndAnArchiveGate(t *testing.T) {
	ctx := context.Background()
	t.Run("no manifest store", func(t *testing.T) {
		if _, err := newService().Recover(ctx, "0123456789abcdef0123456789abcdef", nil); !errors.Is(err, session.ErrCheckpointDisabled) {
			t.Fatalf("recover err = %v, want checkpoint disabled", err)
		}
	})
	t.Run("workload archive gate off", func(t *testing.T) {
		f := newRecoverFixture()
		created, err := f.svc.Create(ctx, session.CreateRequest{Name: "agent", WorkloadType: session.WorkloadTypeClaudeCode})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Snapshot(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Terminate(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		gateOff := service.New(f.orch, f.store, archive.NewStubCheckpointer(true), agent.NewStubClient(),
			service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, archive.NewStubCheckpointer(false)),
			service.WithSessionManifests(f.manifests))
		if _, err := gateOff.Recover(ctx, created.ID, nil); !errors.Is(err, session.ErrCheckpointDisabled) {
			t.Fatalf("recover err = %v, want checkpoint disabled", err)
		}
		if _, err := f.store.Get(ctx, created.ID); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("record after gated recover: %v, want none", err)
		}
	})
}

func TestSnapshotManifestWriteFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	f := newRecoverFixture()
	f.manifests.putErr = errors.New("s3 unavailable")
	created, err := f.svc.Create(ctx, session.CreateRequest{Name: "keep-me-live"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Snapshot(ctx, created.ID); !errors.Is(err, f.manifests.putErr) {
		t.Fatalf("snapshot err = %v, want the manifest error", err)
	}
	stored, err := f.store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != session.StateActive || stored.Pod != created.Pod || stored.SnapshotTransaction != nil || f.orch.RunningCount() != 1 {
		t.Fatalf("session after failed manifest = %+v running=%d, want it live and untouched", stored, f.orch.RunningCount())
	}
}
