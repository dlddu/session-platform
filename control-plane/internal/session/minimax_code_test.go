package session

import (
	"errors"
	"testing"
)

// AC-G1: minimax-code is a valid type with the agent model contract of AC-E6.
func TestMiniMaxCodeWorkloadType(t *testing.T) {
	if !WorkloadTypeMiniMaxCode.Valid() {
		t.Fatal("minimax-code must be a valid workload type")
	}
	got, err := NormalizeWorkloadType(WorkloadTypeMiniMaxCode)
	if err != nil || got != WorkloadTypeMiniMaxCode {
		t.Fatalf("normalize = %q, %v; want minimax-code", got, err)
	}
	if model, err := NormalizeModel(WorkloadTypeMiniMaxCode, ""); err != nil || model != PlatformDefaultModel {
		t.Fatalf("omitted model = %q, %v; want %q", model, err, PlatformDefaultModel)
	}
	if model, err := NormalizeModel(WorkloadTypeMiniMaxCode, " MiniMax-M3 "); err != nil || model != "MiniMax-M3" {
		t.Fatalf("explicit model = %q, %v; want MiniMax-M3", model, err)
	}
	if _, err := NormalizeModel(WorkloadTypeMiniMaxCode, "bad model"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid model err = %v, want ErrInvalidInput", err)
	}
}

// The service's archive path covers claude-code and minimax-code and nothing
// else.
func TestArchivesThroughAgent(t *testing.T) {
	for w, want := range map[WorkloadType]bool{
		WorkloadTypeShell:       false,
		WorkloadTypeClaudeCode:  true,
		WorkloadTypeMiniMaxCode: true,
	} {
		if got := w.ArchivesThroughAgent(); got != want {
			t.Errorf("%s.ArchivesThroughAgent() = %v, want %v", w, got, want)
		}
	}
}
