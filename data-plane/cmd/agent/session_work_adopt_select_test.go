package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

		"lifecycle":   map[string]any{"stage": "implementation"},
		"resumption":  map[string]any{"state": "intent"},
		"blocker":     "리뷰 대기",
		"monitor_ref": map[string]any{"id": "loop-1"},

		"부모가 쓰는 무언가": "이 이미지가 모르는 키",
	}
	live, dropped, unclassified := liveDetails(details)

	// Current state the parent acts on comes across.
	for _, key := range []string{"approval", "restrictions", "preservation", "cancellation",
		"notification", "direct_execution", "kind",
		// The lifecycle keys, which the managing skillset reads every run.
		"lifecycle", "resumption", "blocker", "monitor_ref", "next_action"} {
		if _, ok := live[key]; !ok {
			t.Errorf("liveDetails dropped %q, which is the state the parent acts on", key)
		}
	}
	// An account of something that already happened does not: the turn that did
	// the work holds its own result and artifacts, and the dispatch record holds
	// the intent.
	for _, key := range []string{"result", "summary", "pr", "head", "links", "validation", "provenance",
		"observed_at", "history", "events", "log", "operation", "intent"} {
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
	// The catch-all is a narrow safety net, not the path ordinary state travels
	// on. A key the managing skillset reads every run must be classified, or
	// every real row asks a person about state the parent depends on — which is
	// how these keys were reaching the record before.
	for _, key := range []string{"lifecycle", "resumption", "observation", "monitor_ref", "monitoring",
		"blocker", "next_action", "report", "priority", "goal", "stage"} {
		if workLiveDetailKeys[key] == "" {
			t.Errorf("%q is read by the managing skillset but is not classified live", key)
		}
		if _, past := workPastDetailKeys[key]; past {
			t.Errorf("%q is read by the managing skillset but is classified as past-tense", key)
		}
	}
	// And the two lists cannot both claim a key: a key in both would be carried
	// or dropped depending on which map was consulted first.
	for key := range workLiveDetailKeys {
		if _, past := workPastDetailKeys[key]; past {
			t.Errorf("%q is in both the live and the past list", key)
		}
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

// A carried-over loop has to be a row monitor_put would accept.
//
// The adoption writes sp_work_monitor directly, so it does not pass through the
// tool's own validation — and the ledger bounded nothing against this table's
// limits. A note past the bound, or a loop with no title, lands in a row that
// monitor_get reads happily and monitor_put then refuses: monitor_put is a full
// replacement that requires title, state and note, so the parent could never
// record that its own loop had run again. The row would be readable for ever
// and writable never.
//
// Real ledgers have such rows — a loop that keeps 5 KiB of state in its details
// is past a 4096-byte note — so this is not a hypothetical.
func TestAdoptedMonitorIsAlwaysWritableAfterwards(t *testing.T) {
	for _, test := range []struct {
		name   string
		row    workMonitor
		expect func(t *testing.T, got workMonitor, changed []string)
	}{
		{
			name: "a note past the bound is cut to fit",
			row:  workMonitor{ID: "loop-1", Title: "감시", State: monitorStateRunning, Note: strings.Repeat("가", 2000)},
			expect: func(t *testing.T, got workMonitor, changed []string) {
				// 2000 Korean runes is 6000 bytes, which is what a real ledger
				// row looks like and what the bound is measured in.
				if len(got.Note) > monitorNoteMax {
					t.Fatalf("note = %d bytes, want at most %d", len(got.Note), monitorNoteMax)
				}
				if !utf8.ValidString(got.Note) {
					t.Fatal("the cut split a rune, which stores invalid UTF-8")
				}
				if len(changed) == 0 || !strings.Contains(strings.Join(changed, " "), "never written again") {
					t.Fatalf("changed = %v, want it to say why the cut matters", changed)
				}
			},
		},
		{
			name: "a loop with no title is named by its id",
			row:  workMonitor{ID: "loop-2", Title: "   ", State: monitorStateRunning},
			expect: func(t *testing.T, got workMonitor, changed []string) {
				if got.Title != "loop-2" {
					t.Fatalf("title = %q, want the id", got.Title)
				}
				if len(changed) == 0 {
					t.Fatal("renaming the loop was not reported")
				}
			},
		},
		{
			name: "a state this record does not have becomes stopped",
			row:  workMonitor{ID: "loop-3", Title: "감시", State: "completed"},
			expect: func(t *testing.T, got workMonitor, changed []string) {
				if got.State != monitorStateStopped {
					t.Fatalf("state = %q, want %q", got.State, monitorStateStopped)
				}
				if len(changed) == 0 {
					t.Fatal("the state change was not reported")
				}
			},
		},
		{
			name: "a row already within the bounds is untouched and reports nothing",
			row:  workMonitor{ID: "loop-4", Title: "감시", State: monitorStateRunning, Note: "마지막 확인 10:00"},
			expect: func(t *testing.T, got workMonitor, changed []string) {
				if got.Note != "마지막 확인 10:00" || got.Title != "감시" || got.State != monitorStateRunning {
					t.Fatalf("an acceptable row was changed: %+v", got)
				}
				if len(changed) != 0 {
					t.Fatalf("changed = %v, want nothing reported for a row that was already fine", changed)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, changed := adoptableMonitor(test.row)
			test.expect(t, got, changed)
			// Whatever it did, the result has to be a row the tool accepts —
			// that is the whole point, so it is asserted for every case.
			args := workArgs{ID: got.ID, ExpectedVersion: lvInt(0), Monitor: &workMonitorInput{
				Title: got.Title, State: got.State, Note: got.Note,
			}}
			if err := validateMonitorTool(monitorPutTool, args); err != nil {
				t.Fatalf("%s would refuse the adopted row: %v", monitorPutTool, err)
			}
		})
	}
}

func lvInt(v int64) *int64 { return &v }

// End to end: a ledger loop whose details are larger than the note bound is
// carried, is writable afterwards, and the retirement record says it was cut.
func TestRetireCarriesAnOverLongMonitorNoteAndSaysSo(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	// The loop keeps more state than a note holds, which is what a real one
	// that has been running for months looks like.
	workExec(t, s, "UPDATE sp_ledger_tasks SET document=? WHERE id='task-m'",
		ledgerTaskDocument(t, "모니터 루프 기록", "watch", "parent", nil, map[string]any{
			"kind": "monitor", "seen": strings.Repeat("가", 2000),
		}))
	out := retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))

	monitor := mustWork(t, s, monitorGetTool, map[string]any{"id": "task-m"})
	if len(monitor["note"].(string)) > monitorNoteMax {
		t.Fatalf("the adopted note is %d bytes, over the bound the tool enforces", len(monitor["note"].(string)))
	}
	// The parent can record its loop again, which is what the bound protects.
	mustWork(t, s, monitorPutTool, map[string]any{
		"id": "task-m", "request_id": "m1", "expected_version": monitor["version"],
		"monitor": map[string]any{"title": "모니터 루프 기록", "state": monitorStateRunning, "note": "다시 확인"},
	})
	// And the record says it had to cut, so nobody reads the note as whole.
	dropped := strings.Join(planNotes(t, retirementPlan(t, out), "dropped"), "\n")
	if !strings.Contains(dropped, "sp_work_monitor:task-m") {
		t.Fatalf("the retirement record does not name what it changed:\n%s", dropped)
	}
}
