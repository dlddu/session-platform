package service_test

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// AC-J7: a refresh is a codex session's, brings the session active the way a
// write does — a frozen one is restored first — and refuses anything else
// before it reaches a pod.
func TestRefreshPlugins(t *testing.T) {
	ctx := context.Background()
	ckpt := archive.NewStubCheckpointer(true)
	svc := service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		ckpt, agent.NewStubClient(),
		service.WithWorkloadCheckpointer(session.WorkloadTypeCodexChat, ckpt),
	)
	codex, err := svc.Create(ctx, session.CreateRequest{Name: "long", WorkloadType: session.WorkloadTypeCodexChat})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.RefreshPlugins(ctx, codex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "active" || res.Changes.State != "unconfigured" || res.Session.ID != codex.ID {
		t.Fatalf("refresh = %+v", res)
	}

	if _, err := svc.Snapshot(ctx, codex.ID); err != nil {
		t.Fatal(err)
	}
	res, err = svc.RefreshPlugins(ctx, codex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "snapshot->restore->refresh-plugins" || res.Session.State != session.StateActive {
		t.Fatalf("refresh of a frozen session = %+v", res)
	}

	shell, err := svc.Create(ctx, session.CreateRequest{Name: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshPlugins(ctx, shell.ID); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("shell refresh = %v, want ErrInvalidState", err)
	}
	if _, err := svc.RefreshPlugins(ctx, "nope"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("unknown refresh = %v, want ErrNotFound", err)
	}
}
