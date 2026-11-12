package service_test

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func newLostPodService() (*service.Service, *k8s.StubOrchestrator, *configmap.Store) {
	orch := k8s.NewStubOrchestrator("sessions")
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	svc := service.New(orch, store, archive.NewStubCheckpointer(true),
		failingAgent{err: errors.New("dial tcp: no route to host")})
	return svc, orch, store
}

// AC-C5: an active session whose pod vanished underneath it (node reclaimed,
// eviction) answers read and write with not found and is removed, so a client
// can open a new session instead of retrying a dead one.
func TestLostPodSessionIsReportedNotFound(t *testing.T) {
	for _, op := range []string{"read", "write"} {
		t.Run(op, func(t *testing.T) {
			ctx := context.Background()
			svc, orch, store := newLostPodService()
			sess, err := svc.Create(ctx, session.CreateRequest{Name: "lost-" + op})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			// The pod disappears without the service being told.
			if err := orch.Stop(ctx, k8s.PodRef{Name: sess.Pod}); err != nil {
				t.Fatalf("lose pod: %v", err)
			}

			if op == "read" {
				_, err = svc.Read(ctx, sess.ID, 0)
			} else {
				_, err = svc.Write(ctx, sess.ID, "hello", session.WriteOptions{})
			}
			if !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("%s on lost pod: err = %v, want ErrNotFound", op, err)
			}
			if _, err := store.Get(ctx, sess.ID); !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("record after lost pod: err = %v, want removed", err)
			}
		})
	}
}

// AC-C5: an agent failure while the pod still exists is not a lost pod — the
// error surfaces as before and the session is kept.
func TestAgentFailureWithLivePodKeepsSession(t *testing.T) {
	ctx := context.Background()
	svc, _, store := newLostPodService()
	sess, err := svc.Create(ctx, session.CreateRequest{Name: "flaky"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.Read(ctx, sess.ID, 0)
	if err == nil || errors.Is(err, session.ErrNotFound) {
		t.Fatalf("read with live pod: err = %v, want the agent error", err)
	}
	if _, err := store.Get(ctx, sess.ID); err != nil {
		t.Fatalf("record after agent failure: %v, want kept", err)
	}
}
