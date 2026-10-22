package session

import "testing"

func TestWorkloadTypeCatalogMatchesValidation(t *testing.T) {
	known := []WorkloadType{
		WorkloadTypeShell, WorkloadTypeClaudeCode,
		WorkloadTypeApprovalGated, WorkloadTypeMiniMaxCode,
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
