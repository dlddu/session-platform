package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// A loop this code cannot record faithfully stops the whole run and keeps the
// ledger row exactly as it is.
//
// The adoption writes sp_work_monitor directly, so a row monitor_put would
// refuse could be read for ever and never written again. An earlier attempt at
// this made the row fit by cutting the note, which was wrong: the note is where
// the parent keeps the approval it is acting under, the claim it holds and the
// cursor it resumes from, and it is JSON. Cutting it destroys state nothing
// else holds and leaves the rest unparseable. So each case refuses, names what
// to do, and changes nothing — and that last part is what these tests check,
// because a refusal that half-moved the data would be the worst of both.
func TestRetireRefusesAMonitorItCannotRecordFaithfully(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		document func(t *testing.T) string
		status   string
		expect   string
	}{
		{
			name:   "more state than a note holds",
			status: "running",
			document: func(t *testing.T) string {
				return ledgerTaskDocument(t, "모니터", "watch", "", nil, map[string]any{
					"kind": "monitor", "approval": "PR only", "cursor": strings.Repeat("가", 2000),
				})
			},
			expect: "is not cut",
		},
		{
			name:   "a status that is not running",
			status: "completed",
			document: func(t *testing.T) string {
				return ledgerTaskDocument(t, "끝난 듯한 루프", "watch", "", nil, map[string]any{"kind": "monitor"})
			},
			expect: "finished for good",
		},
		{
			name:   "an owner the record has no column for",
			status: "running",
			document: func(t *testing.T) string {
				return ledgerTaskDocument(t, "모니터", "watch", "parent", nil, map[string]any{"kind": "monitor"})
			},
			expect: "records owner",
		},
		{
			name:   "a session relation the record has no column for",
			status: "running",
			document: func(t *testing.T) string {
				return ledgerTaskDocument(t, "모니터", "watch", "",
					[]workBookSession{{ID: "monitor-1", Ownership: "owned", Cleanup: "pending", NextOffset: 4096}},
					map[string]any{"kind": "monitor"})
			},
			expect: "session relation",
		},
		{
			name:   "no title, which monitor_put requires",
			status: "running",
			document: func(t *testing.T) string {
				return ledgerTaskDocument(t, "   ", "watch", "", nil, map[string]any{"kind": "monitor"})
			},
			expect: "has no title",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newTestSessionDB(t)
			seedLegacyLedger(t, s)
			workExec(t, s, "INSERT INTO sp_ledger_tasks VALUES (?,?,?,?,?,?)",
				"loop-x", int64(1), test.status, "watch", "", test.document(t))
			before := ledgerSnapshotOf(t, s)

			// The rehearsal says it is blocked, in the words the real run uses.
			out := retireAt(t, s, "rehearse", true, now)
			if out["blocked"] != true {
				t.Fatalf("the rehearsal did not say it is blocked: %v", out)
			}
			rehearsed := refusalText(t, out["refusals"])
			if !strings.Contains(rehearsed, test.expect) {
				t.Fatalf("rehearsal refusals do not name %q:\n%s", test.expect, rehearsed)
			}
			if !strings.Contains(rehearsed, "loop-x") {
				t.Fatalf("rehearsal refusals do not name the row:\n%s", rehearsed)
			}

			// The real run refuses, as a conflict a caller branches on.
			err := retireFails(t, s, "real", now)
			assertWorkCode(t, err, "conflict")
			if !strings.Contains(err.Error(), test.expect) {
				t.Fatalf("the refusal does not say why: %v", err)
			}

			// Nothing moved: the ledger is byte for byte what it was, no work
			// record was half-written, and the tables are all still there.
			if after := ledgerSnapshotOf(t, s); after != before {
				t.Fatalf("a refused run changed the ledger:\n%s\n--- became ---\n%s", before, after)
			}
			if n := workCount(t, s, "sp_work_monitor"); n != 0 {
				t.Fatalf("a refused run wrote %d monitoring record(s)", n)
			}
			if n := workCount(t, s, "sp_work_tasks"); n != 0 {
				t.Fatalf("a refused run adopted %d task(s) — no partial adoption is allowed", n)
			}
			if n := workCount(t, s, "sp_work_turns"); n != 0 {
				t.Fatalf("a refused run wrote %d turn(s)", n)
			}
			for _, table := range []string{"sp_ledger_tasks", "sp_ledger_user_requests", "sp_ledger_task_events"} {
				if !workTableIsThere(t, s, table) {
					t.Fatalf("a refused run dropped %s", table)
				}
			}
		})
	}
}

// ledgerSnapshotOf is every ledger task row as text, so a test can assert the
// original is unchanged rather than only that the row count matches.
func ledgerSnapshotOf(t *testing.T, s *sessionDB) string {
	t.Helper()
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT id,version,status,stage,owner,document FROM sp_ledger_tasks ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id, status, stage, owner, document string
		var version int64
		if err := rows.Scan(&id, &version, &status, &stage, &owner, &document); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s|%d|%s|%s|%s|%s", id, version, status, stage, owner, document))
	}
	return strings.Join(out, "\n")
}

func refusalText(t *testing.T, raw any) string {
	t.Helper()
	switch v := raw.(type) {
	case []string:
		return strings.Join(v, "\n")
	case []any:
		out := []string{}
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return strings.Join(out, "\n")
	default:
		t.Fatalf("refusals = %T %v", raw, raw)
		return ""
	}
}

// The other side of the gate: a loop small enough and plain enough to record
// faithfully goes across whole and comes back out of monitor_get byte for byte,
// and the parent can then write it. A refusal gate that also blocked the
// ordinary row would have stopped every cleanup.
func TestRetireCarriesAPlainMonitorWholeAndItStaysWritable(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedLegacyLedger(t, s)
	retireAt(t, s, "r1", false, time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))

	monitor := mustWork(t, s, monitorGetTool, map[string]any{"id": "task-m"})
	// The note is the loop's own state, carried whole — not cut, and still the
	// JSON the parent wrote.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(monitor["note"].(string)), &decoded); err != nil {
		t.Fatalf("the carried note is not the JSON the parent wrote: %v (%q)", err, monitor["note"])
	}
	if decoded["last_seen"] != "2026-11-01T00:00:00Z" {
		t.Fatalf("the loop's state did not come across whole: %v", decoded)
	}
	if _, marker := decoded["kind"]; marker {
		t.Fatalf("the kind convention came across with it: %v", decoded)
	}
	// And it is writable, which is the invariant the refusals protect.
	mustWork(t, s, monitorPutTool, map[string]any{
		"id": "task-m", "request_id": "m1", "expected_version": monitor["version"],
		"monitor": map[string]any{"title": "모니터 루프 기록", "state": monitorStateRunning, "note": "다시 확인"},
	})
	if back := mustWork(t, s, monitorGetTool, map[string]any{"id": "task-m"}); back["note"] != "다시 확인" {
		t.Fatalf("the round trip did not hold: %v", back)
	}
}
