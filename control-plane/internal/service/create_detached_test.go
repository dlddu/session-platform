package service_test

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// ctxHonouringOrchestrator fails a Start whose context is already done, the
// way the real orchestrator's waitReady does — and then reclaims the pod.
type ctxHonouringOrchestrator struct {
	*k8s.StubOrchestrator
}

func (o ctxHonouringOrchestrator) Start(ctx context.Context, sessionID string, workload k8s.WorkloadSpec) (k8s.SessionPods, error) {
	if err := ctx.Err(); err != nil {
		return k8s.SessionPods{}, err
	}
	return o.StubOrchestrator.Start(ctx, sessionID, workload)
}

// A caller that goes away mid-create (a proxy timing the request out while the
// pod waits for a node) must not abort provisioning: the session still lands
// in the store, where the caller finds it with List.
func TestCreateOutlivesTheCallersContext(t *testing.T) {
	orch := ctxHonouringOrchestrator{k8s.NewStubOrchestrator("sessions")}
	store := configmap.NewStore(fake.NewSimpleClientset(), "sessions")
	svc := service.New(orch, store, archive.NewStubCheckpointer(true), agent.NewStubClient())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sess, err := svc.Create(ctx, session.CreateRequest{Name: "late"})
	if err != nil {
		t.Fatalf("Create with a cancelled caller context: %v", err)
	}
	if sess.State != session.StateActive || sess.Pod == "" {
		t.Fatalf("session = %+v, want an active session with a pod", sess)
	}
	listed, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != sess.ID {
		t.Fatalf("List = %+v, want the created session", listed)
	}
}
