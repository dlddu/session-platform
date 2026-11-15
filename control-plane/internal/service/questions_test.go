package service_test

import (
	"context"
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

// questioningAgent is a stub agent whose running turn asked one question.
type questioningAgent struct {
	*agent.StubClient
	listedPod   string
	answeredPod string
	answeredID  string
	answers     map[string]string
}

func (a *questioningAgent) PendingQuestions(_ context.Context, pod string) ([]session.PendingQuestion, error) {
	a.listedPod = pod
	return []session.PendingQuestion{{ID: "q_1", Questions: []session.Question{{Question: "Which?"}}}}, nil
}

func (a *questioningAgent) AnswerQuestion(_ context.Context, pod, id string, answers map[string]string) error {
	if id != "q_1" {
		return session.ErrQuestionNotFound
	}
	a.answeredPod, a.answeredID, a.answers = pod, id, answers
	return nil
}

// AC-E2: a claude-code session's waiting questions are listed and answered
// through its pod's agent; other types and frozen sessions have none, and
// listing never wakes a frozen session.
func TestQuestions(t *testing.T) {
	ctx := context.Background()
	ckpt := archive.NewStubCheckpointer(true)
	ag := &questioningAgent{StubClient: agent.NewStubClient()}
	svc := service.New(
		k8s.NewStubOrchestrator("sessions"),
		configmap.NewStore(fake.NewSimpleClientset(), "sessions"),
		ckpt, ag,
		service.WithWorkloadCheckpointer(session.WorkloadTypeClaudeCode, ckpt),
	)
	claude, err := svc.Create(ctx, session.CreateRequest{Name: "cc", WorkloadType: session.WorkloadTypeClaudeCode})
	if err != nil {
		t.Fatal(err)
	}
	questions, err := svc.Questions(ctx, claude.ID)
	if err != nil || len(questions) != 1 || questions[0].ID != "q_1" || ag.listedPod != claude.Pod {
		t.Fatalf("questions = %+v, %v (pod %q)", questions, err, ag.listedPod)
	}
	answers := map[string]string{"Which?": "A"}
	if err := svc.AnswerQuestion(ctx, claude.ID, "q_1", answers); err != nil {
		t.Fatal(err)
	}
	if ag.answeredPod != claude.Pod || ag.answeredID != "q_1" || !reflect.DeepEqual(ag.answers, answers) {
		t.Fatalf("agent got %q %q %v", ag.answeredPod, ag.answeredID, ag.answers)
	}
	if err := svc.AnswerQuestion(ctx, claude.ID, "q_9", answers); !errors.Is(err, session.ErrQuestionNotFound) {
		t.Fatalf("unknown question = %v, want ErrQuestionNotFound", err)
	}
	if err := svc.AnswerQuestion(ctx, claude.ID, "q_1", nil); !errors.Is(err, session.ErrInvalidInput) {
		t.Fatalf("no answers = %v, want ErrInvalidInput", err)
	}

	if _, err := svc.Snapshot(ctx, claude.ID); err != nil {
		t.Fatal(err)
	}
	ag.listedPod = ""
	questions, err = svc.Questions(ctx, claude.ID)
	if err != nil || len(questions) != 0 || ag.listedPod != "" {
		t.Fatalf("questions of a frozen session = %+v, %v (asked pod %q)", questions, err, ag.listedPod)
	}
	if frozen, _ := svc.Get(ctx, claude.ID); frozen.State != session.StateSnapshot {
		t.Fatalf("listing questions woke the session: %s", frozen.State)
	}
	if err := svc.AnswerQuestion(ctx, claude.ID, "q_1", answers); !errors.Is(err, session.ErrQuestionNotFound) {
		t.Fatalf("answer to a frozen session = %v, want ErrQuestionNotFound", err)
	}

	shell, err := svc.Create(ctx, session.CreateRequest{Name: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Questions(ctx, shell.ID); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("shell questions = %v, want ErrInvalidState", err)
	}
	if _, err := svc.Questions(ctx, "nope"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("unknown session = %v, want ErrNotFound", err)
	}
}
