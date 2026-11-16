package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/agent"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/archive"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/configmap"
	"github.com/dlddu/session-platform/control-plane/internal/adapter/k8s"
	"github.com/dlddu/session-platform/control-plane/internal/service"
	"github.com/dlddu/session-platform/control-plane/internal/session"
)

const (
	testEpoch    = "ep_1111111111111111"
	testArgsHash = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
)

// approvingAgent is a stub agent whose running turn put one tool call to the
// session owner. It binds a decision the way the real agent does: only the
// call it is waiting on, on the arguments and the run it was put up in.
type approvingAgent struct {
	*agent.StubClient
	listedPod       string
	approvalWrites  []string
	decidedPod      string
	decidedID       string
	decision        session.ToolApprovalDecision
	unsupportedType bool
}

func (a *approvingAgent) PendingToolApprovals(_ context.Context, pod string) (session.ToolApprovalPage, error) {
	a.listedPod = pod
	return session.ToolApprovalPage{
		Epoch: testEpoch,
		Approvals: []session.PendingToolApproval{{
			ID:       "ap_1",
			Tool:     "Bash",
			Input:    json.RawMessage(`{"command":"ls"}`),
			ArgsHash: testArgsHash,
			Epoch:    testEpoch,
		}},
	}, nil
}

func (a *approvingAgent) DecideToolApproval(_ context.Context, pod, id string, decision session.ToolApprovalDecision) error {
	if id != "ap_1" {
		return session.ErrToolApprovalNotFound
	}
	if decision.ArgsHash != testArgsHash || decision.Epoch != testEpoch {
		return session.ErrToolApprovalStale
	}
	a.decidedPod, a.decidedID, a.decision = pod, id, decision
	return nil
}

func (a *approvingAgent) WriteWithToolApprovals(_ context.Context, pod, payload string) error {
	if a.unsupportedType {
		return session.ErrToolApprovalsUnsupported
	}
	a.approvalWrites = append(a.approvalWrites, pod+":"+payload)
	return nil
}

func newApprovalService(ag agent.Client) *service.Service {
	ckpt := archive.NewStubCheckpointer(true)
	return service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		ckpt, ag,
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
}

// AC-E2 「턴 중 도구 승인」: a claude-code session's waiting tool calls are
// listed and decided through its pod's agent, a write can ask for them, and
// the decision carries the call it was taken on.
func TestToolApprovals(t *testing.T) {
	ctx := context.Background()
	ag := &approvingAgent{StubClient: agent.NewStubClient()}
	svc := newApprovalService(ag)
	claude, err := svc.Create(ctx, session.CreateRequest{Name: "cc", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.WriteTurn(ctx, claude.ID, "do the thing", session.WriteOptions{ToolApprovals: true})
	if err != nil || res == nil {
		t.Fatalf("write asking for the owner's decision = %v", err)
	}
	if !reflect.DeepEqual(ag.approvalWrites, []string{claude.Pod + ":do the thing"}) {
		t.Fatalf("agent writes = %v, want the prompt sent with approvals", ag.approvalWrites)
	}

	page, err := svc.ToolApprovals(ctx, claude.ID)
	if err != nil || page.Epoch != testEpoch || len(page.Approvals) != 1 || page.Approvals[0].ID != "ap_1" {
		t.Fatalf("approvals = %+v, %v", page, err)
	}
	if ag.listedPod != claude.Pod {
		t.Fatalf("listed pod = %q, want %q", ag.listedPod, claude.Pod)
	}

	decision := session.ToolApprovalDecision{
		Decision: session.ToolApprovalApprove,
		ArgsHash: testArgsHash,
		Epoch:    testEpoch,
		Reason:   "reviewed the command",
	}
	if err := svc.DecideToolApproval(ctx, claude.ID, "ap_1", decision); err != nil {
		t.Fatal(err)
	}
	if ag.decidedPod != claude.Pod || ag.decidedID != "ap_1" || ag.decision.Reason != "reviewed the command" {
		t.Fatalf("agent got %q %q %+v", ag.decidedPod, ag.decidedID, ag.decision)
	}
}

// A decision that does not name the call it was taken on never reaches the
// agent, and one the agent refuses keeps its meaning on the way back.
func TestToolApprovalDecisionIsBound(t *testing.T) {
	ctx := context.Background()
	ag := &approvingAgent{StubClient: agent.NewStubClient()}
	svc := newApprovalService(ag)
	claude, err := svc.Create(ctx, session.CreateRequest{Name: "cc", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	valid := session.ToolApprovalDecision{Decision: session.ToolApprovalApprove, ArgsHash: testArgsHash, Epoch: testEpoch}

	for name, decision := range map[string]session.ToolApprovalDecision{
		"no decision":  {ArgsHash: testArgsHash, Epoch: testEpoch},
		"not an allow": {Decision: "allow", ArgsHash: testArgsHash, Epoch: testEpoch},
		"no args hash": {Decision: session.ToolApprovalApprove, Epoch: testEpoch},
		"no epoch":     {Decision: session.ToolApprovalApprove, ArgsHash: testArgsHash},
	} {
		if err := svc.DecideToolApproval(ctx, claude.ID, "ap_1", decision); !errors.Is(err, session.ErrInvalidInput) {
			t.Errorf("%s = %v, want ErrInvalidInput", name, err)
		}
	}
	if err := svc.DecideToolApproval(ctx, claude.ID, "", valid); !errors.Is(err, session.ErrInvalidInput) {
		t.Errorf("no call named = %v, want ErrInvalidInput", err)
	}
	if ag.decidedID != "" {
		t.Fatalf("an unbound decision reached the agent as %q", ag.decidedID)
	}

	if err := svc.DecideToolApproval(ctx, claude.ID, "ap_9", valid); !errors.Is(err, session.ErrToolApprovalNotFound) {
		t.Errorf("unknown call = %v, want ErrToolApprovalNotFound", err)
	}
	stale := session.ToolApprovalDecision{Decision: session.ToolApprovalApprove, ArgsHash: testArgsHash, Epoch: "ep_0000000000000000"}
	if err := svc.DecideToolApproval(ctx, claude.ID, "ap_1", stale); !errors.Is(err, session.ErrToolApprovalStale) {
		t.Errorf("another run = %v, want ErrToolApprovalStale", err)
	}
	changed := session.ToolApprovalDecision{Decision: session.ToolApprovalDeny, ArgsHash: "sha256:" + testArgsHash[7:63] + "ff", Epoch: testEpoch}
	if err := svc.DecideToolApproval(ctx, claude.ID, "ap_1", changed); !errors.Is(err, session.ErrToolApprovalStale) {
		t.Errorf("other arguments = %v, want ErrToolApprovalStale", err)
	}
}

// Only claude-code puts its tool calls to a person, a frozen session has no
// running turn to decide, and listing never wakes one.
func TestToolApprovalsScope(t *testing.T) {
	ctx := context.Background()
	ag := &approvingAgent{StubClient: agent.NewStubClient()}
	svc := newApprovalService(ag)
	shell, err := svc.Create(ctx, session.CreateRequest{Name: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ToolApprovals(ctx, shell.ID); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("shell approvals = %v, want ErrInvalidState", err)
	}
	if _, err := svc.WriteTurn(ctx, shell.ID, "ls\n", session.WriteOptions{ToolApprovals: true}); !errors.Is(err, session.ErrToolApprovalsUnsupported) {
		t.Fatalf("shell write asking for approvals = %v, want ErrToolApprovalsUnsupported", err)
	}
	if len(ag.approvalWrites) != 0 {
		t.Fatalf("a refused ask still wrote: %v", ag.approvalWrites)
	}

	claude, err := svc.Create(ctx, session.CreateRequest{Name: "cc", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Snapshot(ctx, claude.ID); err != nil {
		t.Fatal(err)
	}
	ag.listedPod = ""
	page, err := svc.ToolApprovals(ctx, claude.ID)
	if err != nil || len(page.Approvals) != 0 {
		t.Fatalf("frozen approvals = %+v, %v", page, err)
	}
	if ag.listedPod != "" {
		t.Fatalf("listing woke a frozen session's agent (pod %q)", ag.listedPod)
	}
	if got, err := svc.Get(ctx, claude.ID); err != nil || got.State != session.StateSnapshot {
		t.Fatalf("session = %+v, %v, want it still frozen", got, err)
	}
	valid := session.ToolApprovalDecision{Decision: session.ToolApprovalApprove, ArgsHash: testArgsHash, Epoch: testEpoch}
	if err := svc.DecideToolApproval(ctx, claude.ID, "ap_1", valid); !errors.Is(err, session.ErrToolApprovalNotFound) {
		t.Fatalf("deciding on a frozen session = %v, want ErrToolApprovalNotFound", err)
	}
}

// An agent that cannot relay approvals at all refuses the ask rather than
// running the turn without it.
func TestToolApprovalsNeedARelayingAgent(t *testing.T) {
	ctx := context.Background()
	svc := newApprovalService(agent.NewStubClient())
	claude, err := svc.Create(ctx, session.CreateRequest{Name: "cc", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WriteTurn(ctx, claude.ID, "do the thing", session.WriteOptions{ToolApprovals: true}); !errors.Is(err, session.ErrToolApprovalsUnsupported) {
		t.Fatalf("write = %v, want ErrToolApprovalsUnsupported", err)
	}
	page, err := svc.ToolApprovals(ctx, claude.ID)
	if err != nil || len(page.Approvals) != 0 {
		t.Fatalf("approvals = %+v, %v", page, err)
	}
	valid := session.ToolApprovalDecision{Decision: session.ToolApprovalApprove, ArgsHash: testArgsHash, Epoch: testEpoch}
	if err := svc.DecideToolApproval(ctx, claude.ID, "ap_1", valid); !errors.Is(err, session.ErrToolApprovalNotFound) {
		t.Fatalf("decide = %v, want ErrToolApprovalNotFound", err)
	}
	// A plain write is untouched by any of this.
	if _, err := svc.Write(ctx, claude.ID, "do the thing"); err != nil {
		t.Fatalf("plain write = %v", err)
	}
}
