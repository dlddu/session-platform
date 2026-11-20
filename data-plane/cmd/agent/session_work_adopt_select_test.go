package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The selection itself, at the unit it is decided in.
//
// The move that came before this one copied a ledger row's whole `details` JSON
// verbatim into the record beside the task. That is what made "preserved" do
// the work of "needed": the record kept growing a copy of things that had
// already happened, and a reader that found a result summary or a PR link there
// could not tell it from current state. So the carry is a field selection, and
// these are the cases it has to get right.
func TestLiveDetailsSelectsRatherThanCopying(t *testing.T) {
	// Every key this image classifies, in one row, the way a real ledger task
	// accumulated them.
	details := map[string]any{
		"approval":         "PR only",
		"restrictions":     "no force push",
		"preservation":     "unpushed work on worker-now",
		"cancellation":     "none",
		"notification":     map[string]any{"sent": true},
		"direct_execution": "the parent ran this itself",
		"kind":             "task",

		"result":      "PR #41 을 올렸습니다",
		"summary":     "끝났습니다",
		"pr":          "https://example.invalid/pull/41",
		"head":        "abc123",
		"links":       []any{"https://example.invalid/run/9"},
		"validation":  "CI 초록",
		"provenance":  map[string]any{"imported": "2026-10-01"},
		"observed_at": "2026-10-01T00:00:00Z",
		"history":     []any{"만들었다"},
		"events":      []any{},
		"log":         "...",
		"operation":   map[string]any{"key": "op-1"},
		"intent":      "PR 올리기",
		"next_action": "리뷰 받기",

		"부모가 쓰는 무언가": "이 이미지가 모르는 키",
	}
	live, dropped, unclassified := liveDetails(details)

	// Current state the parent acts on comes across.
	for _, key := range []string{"approval", "restrictions", "preservation", "cancellation",
		"notification", "direct_execution", "kind"} {
		if _, ok := live[key]; !ok {
			t.Errorf("liveDetails dropped %q, which is the state the parent acts on", key)
		}
	}
	// An account of something that already happened does not: the turn that did
	// the work holds its own result and artifacts, and the dispatch record holds
	// the intent.
	for _, key := range []string{"result", "summary", "pr", "head", "links", "validation", "provenance",
		"observed_at", "history", "events", "log", "operation", "intent", "next_action"} {
		if _, ok := live[key]; ok {
			t.Errorf("liveDetails carried %q, which records something that already happened", key)
		}
		if !contains(dropped, key) {
			t.Errorf("liveDetails dropped %q without naming it", key)
		}
	}
	// A key this image has no judgement about is carried, not guessed away —
	// dropping live state on a guess is the one mistake that cannot be noticed
	// from the outside — and it is reported so a person can decide.
	if _, ok := live["부모가 쓰는 무언가"]; !ok {
		t.Error("liveDetails guessed away a key it does not recognise")
	}
	if !contains(unclassified, "부모가 쓰는 무언가") {
		t.Errorf("unclassified = %v, want the unrecognised key reported", unclassified)
	}
	// A key this image does classify is not reported as unclassified, or the
	// rehearsal would ask a person about every ordinary row.
	if contains(unclassified, "approval") {
		t.Errorf("unclassified = %v, want only the keys this image has no judgement about", unclassified)
	}
	// Every dropped key has a stated reason: "nothing is lost" is a claim a
	// reader is entitled to check.
	for _, key := range dropped {
		if workPastDetailKeys[key] == "" {
			t.Errorf("%q is dropped with no reason recorded for it", key)
		}
	}
	// The classification is case- and space-insensitive, because a ledger row's
	// keys were whatever a parent wrote.
	live, dropped, _ = liveDetails(map[string]any{" Result ": "x", "APPROVAL": "y"})
	if _, ok := live["APPROVAL"]; !ok || len(dropped) != 1 || dropped[0] != "result" {
		t.Errorf("live=%v dropped=%v, want the keys matched regardless of case and space", live, dropped)
	}
}

// A relation whose session has been deleted is a cursor into something that is
// not there. Keeping it is keeping a pointer to nothing, and a parent that
// tried to resume from it would read an archive that is gone.
func TestLiveRelationsKeepsOnlyWhatCanBeResumedFrom(t *testing.T) {
	live, dropped := liveRelations([]workBookSession{
		{ID: "worker-old", Ownership: "owned", Cleanup: "deleted"},
		{ID: "worker-now", Ownership: "owned", Cleanup: "pending", NextOffset: 4096},
		{ID: "reviewer", Ownership: "reused", Cleanup: "retained"},
		{ID: "worker-gone", Ownership: "owned", Cleanup: " DELETED "},
	})
	if len(live) != 2 || live[0].ID != "worker-now" || live[1].ID != "reviewer" {
		t.Fatalf("live = %+v, want the two that can still be resumed from", live)
	}
	// The cursor comes across intact: it is the whole point of keeping the
	// relation at all.
	if live[0].NextOffset != 4096 {
		t.Fatalf("relation = %+v, want its cursor kept", live[0])
	}
	if len(dropped) != 2 || !contains(dropped, "worker-old") || !contains(dropped, "worker-gone") {
		t.Fatalf("dropped = %v, want both deleted sessions named", dropped)
	}
	// Nothing at all is an empty list rather than a nil that encodes as null: a
	// reader of the record gets `[]`.
	live, dropped = liveRelations(nil)
	encoded, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" || len(dropped) != 0 {
		t.Fatalf("no relations = %s / %v", encoded, dropped)
	}
}

// The selection is what actually reaches the stored record, not just what the
// helper returns: the row the parent reads back must not contain the whole
// ledger JSON under a different name.
func TestAdoptedBookkeepingStoresOnlyTheSelectedFields(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))

	// The stored column itself, not the tool's projection of it: a copy of the
	// original document hiding in the row would still be a copy.
	stored := workRow(t, s, "SELECT details,sessions FROM sp_work_task_book WHERE task_id='task-a'")
	if stored == nil {
		t.Fatal("the task's bookkeeping was not stored")
	}
	details := stored["details"].(string)
	for _, gone := range []string{"result", "\"pr\"", "observed_at", "PR #41"} {
		if strings.Contains(details, gone) {
			t.Fatalf("the stored details still contain %q: %s", gone, details)
		}
	}
	for _, kept := range []string{"approval", "preservation", "wildcard"} {
		if !strings.Contains(details, kept) {
			t.Fatalf("the stored details lost %q: %s", kept, details)
		}
	}
	if strings.Contains(stored["sessions"].(string), "worker-old") {
		t.Fatalf("the stored relations kept a deleted session: %s", stored["sessions"])
	}

	// And the recorded run names every key it dropped, so the cleanup is
	// auditable afterwards rather than only correct at the time.
	recorded := workRow(t, s, "SELECT detail FROM sp_work_retirement WHERE step=? ORDER BY seq DESC LIMIT 1",
		workRetirementStepRetire)["detail"].(string)
	for _, named := range []string{"details.result", "details.pr", "details.observed_at", "session worker-old"} {
		if !strings.Contains(recorded, named) {
			t.Fatalf("the retirement record does not name %q as dropped: %s", named, recorded)
		}
	}
}

func contains(all []string, want string) bool {
	for _, candidate := range all {
		if candidate == want {
			return true
		}
	}
	return false
}
