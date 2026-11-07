package session

import (
	"strings"
	"testing"
)

func TestWorkloadTypeCatalogMatchesValidation(t *testing.T) {
	known := []WorkloadType{
		WorkloadTypeShell, WorkloadTypeClaudeCode,
		WorkloadTypeApprovalGated, WorkloadTypeMiniMaxCode,
		WorkloadTypeCodexChat, WorkloadTypeCodexTask,
	}
	catalog := WorkloadTypes()
	if len(catalog) != len(known) {
		t.Fatalf("catalog has %d types, want %d", len(catalog), len(known))
	}
	seen := map[WorkloadType]bool{}
	for _, info := range catalog {
		if !info.Type.Valid() {
			t.Errorf("catalog type %q is not Valid()", info.Type)
		}
		if seen[info.Type] {
			t.Errorf("catalog lists %q twice", info.Type)
		}
		seen[info.Type] = true
		if info.Label == "" || info.Description == "" {
			t.Errorf("%q needs a label and description", info.Type)
		}
		if info.Input != InputModeTerminal && info.Input != InputModePrompt {
			t.Errorf("%q input = %q", info.Type, info.Input)
		}
		_, err := NormalizeModel(info.Type, "some-model")
		if accepts := err == nil; accepts != info.TakesModel {
			t.Errorf("%q TakesModel = %v, but NormalizeModel accepts a model: %v", info.Type, info.TakesModel, accepts)
		}
	}
	for _, w := range known {
		info := infoFor(t, catalog, w)
		for _, effort := range info.ReasoningEfforts {
			if got, err := NormalizeReasoningEffort(w, effort); err != nil || got != effort {
				t.Errorf("%q lists effort %q but NormalizeReasoningEffort = %q, %v", w, effort, got, err)
			}
		}
		if _, err := NormalizeReasoningEffort(w, "not-an-effort"); err == nil {
			t.Errorf("%q accepted an unlisted effort", w)
		}
		if !seen[w] {
			t.Errorf("valid type %q is missing from the catalog", w)
		}
	}
	if catalog[0].Type != WorkloadTypeShell {
		t.Errorf("first type = %q, want the default shell", catalog[0].Type)
	}
}

func TestWorkloadTypesReturnsCopy(t *testing.T) {
	got := WorkloadTypes()
	got[0].Label = "mutated"
	if WorkloadTypes()[0].Label == "mutated" {
		t.Fatal("WorkloadTypes exposed the package catalog")
	}
}

func infoFor(t *testing.T, catalog []WorkloadTypeInfo, w WorkloadType) WorkloadTypeInfo {
	t.Helper()
	for _, info := range catalog {
		if info.Type == w {
			return info
		}
	}
	t.Fatalf("%q missing from the catalog", w)
	return WorkloadTypeInfo{}
}

func TestReasoningEffortsPerType(t *testing.T) {
	want := map[WorkloadType][]string{
		WorkloadTypeClaudeCode: {"low", "medium", "high", "xhigh", "max"},
		WorkloadTypeCodexTask:  {"minimal", "low", "medium", "high", "xhigh"},
	}
	for _, info := range WorkloadTypes() {
		if got, exp := strings.Join(info.ReasoningEfforts, ","), strings.Join(want[info.Type], ","); got != exp {
			t.Errorf("%q efforts = %q, want %q", info.Type, got, exp)
		}
	}
	if _, err := NormalizeReasoningEffort(WorkloadTypeClaudeCode, "minimal"); err == nil {
		t.Error("claude-code accepted codex's minimal")
	}
	if _, err := NormalizeReasoningEffort(WorkloadTypeCodexTask, "max"); err == nil {
		t.Error("codex-task accepted claude's max")
	}
	if got, err := NormalizeReasoningEffort(WorkloadTypeShell, ""); err != nil || got != "" {
		t.Errorf("unset effort on shell = %q, %v", got, err)
	}
}

func TestLegacyCodexIsCodexChat(t *testing.T) {
	got, err := NormalizeWorkloadType("codex")
	if err != nil || got != WorkloadTypeCodexChat {
		t.Fatalf("NormalizeWorkloadType(codex) = %q, %v", got, err)
	}
	for _, info := range WorkloadTypes() {
		if info.Type == "codex" {
			t.Fatal("the catalog must not offer the legacy name")
		}
	}
}

func TestWorkloadTypesCopiesEfforts(t *testing.T) {
	got := WorkloadTypes()
	for i := range got {
		if len(got[i].ReasoningEfforts) > 0 {
			got[i].ReasoningEfforts[0] = "mutated"
		}
	}
	for _, info := range WorkloadTypes() {
		if len(info.ReasoningEfforts) > 0 && info.ReasoningEfforts[0] == "mutated" {
			t.Fatal("WorkloadTypes exposed a catalog effort list")
		}
	}
}
