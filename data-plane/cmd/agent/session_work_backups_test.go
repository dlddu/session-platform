package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func workBackupList(t *testing.T, s *sessionDB) ([]map[string]any, map[string]any) {
	t.Helper()
	out := mustWork(t, s, workBackupsTool, map[string]any{})
	listed := []map[string]any{}
	for _, raw := range out["backups"].([]any) {
		listed = append(listed, raw.(map[string]any))
	}
	return listed, out["release"].(map[string]any)
}

// theBackup is the one snapshot a seeded run leaves, which is what every test
// here is about. More than one means the fixture changed, and a test that
// silently looked at the wrong file would pass for the wrong reason.
func theBackup(t *testing.T, s *sessionDB) map[string]any {
	t.Helper()
	listed, _ := workBackupList(t, s)
	if len(listed) != 1 {
		t.Fatalf("snapshots = %v, want exactly one", listed)
	}
	return listed[0]
}

// recordRetireSnapshot is the state a run leaves behind: a snapshot taken and
// its path, checksum and size written down. The record is what makes the file
// identifiable later, so a test about retention has to create both.
func recordRetireSnapshot(t *testing.T, s *sessionDB, now time.Time) map[string]any {
	t.Helper()
	snapshot, err := s.snapshotSessionDB(context.Background(), mustHandle(t, s), workRetireSnapshotLabel, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	workExec(t, s, "INSERT INTO sp_work_retirement (at,step,detail) VALUES (?,?,?)",
		workNow(now), workRetirementStepSnapshot, string(encoded))
	return snapshot
}

// The correction this file exists for: a snapshot is released by the state of
// the move, not by a clock.
//
// The policy used to keep every snapshot for a fixed thirty days after the
// retirement. That window established nothing — it did not say whether the
// move finished, whether the records it carried are reachable, or whether this
// database is healthy enough that the snapshot is not the best copy in the slot
// — and a snapshot kept because a clock has not run out is the "retained for
// ever" this area replaced, with an extra step. So a verified run's snapshot is
// deletable *immediately*, and this is the test that says so: the retirement
// and the cleanup happen at the same instant.
func TestWorkBackupsAreReleasedWithoutWaitingForAClock(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	retireAt(t, s, "r1", false, now)

	backup, release := func() (map[string]any, map[string]any) {
		listed, release := workBackupList(t, s)
		if len(listed) != 1 {
			t.Fatalf("snapshots = %v, want the one the run took", listed)
		}
		return listed[0], release
	}()
	if backup["retention"] != workRetentionEligible {
		t.Fatalf("retention = %v (%v), want %s at the very instant of the retirement",
			backup["retention"], backup["reason"], workRetentionEligible)
	}
	if release["released"] != true {
		t.Fatalf("release = %v, want the database reported as finished with its snapshots", release)
	}
	// No answer in this area mentions a waiting period any more, because there
	// is not one. A reader that saw "eligible_at" would plan around a date.
	encoded, err := json.Marshal(mustWork(t, s, workBackupsTool, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"eligible_at", "days have passed", "retention window"} {
		if strings.Contains(string(encoded), gone) {
			t.Fatalf("the answer still talks about a time window (%q): %s", gone, encoded)
		}
	}
	// And the reason it gives is the set of conditions it actually checked.
	reason := backup["reason"].(string)
	for _, want := range []string{"integrity", "refusals", "transition table"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want it to name the %q condition", reason, want)
		}
	}
}

// The other half of the correction: released means deletable. A policy that
// says a file may go and then keeps it is the same as having no policy.
func TestWorkBackupsPruneDeletesAReleasedSnapshot(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	retireAt(t, s, "r1", false, now)
	path := theBackup(t, s)["path"].(string)

	// The rehearsal names the file and leaves it there.
	dry := mustWork(t, s, workBackupsPruneTool, map[string]any{
		"request_id": "p-dry", "all_eligible": true, "dry_run": true,
	})
	if dry["dry_run"] != true || len(dry["would_delete"].([]any)) != 1 {
		t.Fatalf("rehearsal = %v", dry)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the rehearsal deleted the snapshot: %v", err)
	}

	out := mustWork(t, s, workBackupsPruneTool, map[string]any{
		"request_id": "p1", "all_eligible": true,
	})
	if out["count"].(float64) != 1 {
		t.Fatalf("prune = %v, want the one released snapshot deleted", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the snapshot is still there after a prune that said it deleted it: %v", err)
	}
	// What was deleted is recorded, so the record does not later read as a
	// snapshot that was never taken.
	row := workRow(t, s, "SELECT count(*) AS n FROM sp_work_retirement WHERE step=?", workBackupPruneStep)
	if row["n"].(int64) != 1 {
		t.Fatalf("the deletion was not recorded: %v", row)
	}
	// Replaying the same request_id answers what was committed rather than
	// failing on a file that is already gone.
	replay := mustWork(t, s, workBackupsPruneTool, map[string]any{"request_id": "p1", "all_eligible": true})
	if replay["count"].(float64) != 1 {
		t.Fatalf("replay = %v, want the committed answer", replay)
	}
	// And there is nothing left to release.
	listed, _ := workBackupList(t, s)
	if len(listed) != 0 {
		t.Fatalf("snapshots = %v, want none left", listed)
	}
}

// Each condition the release depends on, checked one at a time by breaking it:
// a snapshot that is still required has to say which one it is waiting on, or
// the policy is just a different way of keeping everything.
func TestWorkBackupsStayRequiredUntilEveryConditionHolds(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		break_ func(t *testing.T, s *sessionDB)
		reason string
	}{
		{
			// The move has not run: an interrupted run still resumes from this
			// snapshot, and the records it holds are not all adopted. This is the
			// one case with no retirement, so its snapshot is recorded by hand.
			name: "the move has not finished",
			break_: func(t *testing.T, s *sessionDB) {
				recordRetireSnapshot(t, s, now)
			},
			reason: "has not finished",
		},
		{
			// Retired by the state flag alone, with no record of the run. A flag
			// is not evidence that anything happened.
			name: "no retirement is recorded",
			break_: func(t *testing.T, s *sessionDB) {
				retireAt(t, s, "r1", false, now)
				workExec(t, s, "DELETE FROM sp_work_retirement WHERE step=?", workRetirementStepRetire)
			},
			reason: "no retirement is recorded",
		},
		{
			// A run that reported refusals did not carry everything, so the
			// snapshot is still the only copy of what was left behind.
			name: "the recorded run reported refusals",
			break_: func(t *testing.T, s *sessionDB) {
				retireAt(t, s, "r1", false, now)
				workExec(t, s,
					`UPDATE sp_work_retirement SET detail='{"refusals":["something was not carried"]}' WHERE step=?`,
					workRetirementStepRetire)
			},
			reason: "refusal",
		},
		{
			// The cleanup did not complete: a transition table is still beside
			// the database, so the snapshot is the archive of rows still there.
			name: "a transition table is still there",
			break_: func(t *testing.T, s *sessionDB) {
				retireAt(t, s, "r1", false, now)
				workExec(t, s, "CREATE TABLE sp_ledger_tasks (id TEXT PRIMARY KEY)")
			},
			reason: "did not complete",
		},
		{
			// Damage is the one state in which the snapshot is the best copy in
			// the slot.
			name: "the records are unreachable",
			break_: func(t *testing.T, s *sessionDB) {
				retireAt(t, s, "r1", false, now)
				// The foreign key is what normally prevents this, so the damage
				// has to be made behind it — which is how a database that lost a
				// page gets here too.
				workExec(t, s, "PRAGMA foreign_keys=off")
				workExec(t, s, "DELETE FROM sp_work_tasks")
				workExec(t, s, "PRAGMA foreign_keys=on")
			},
			reason: "task is missing",
		},
		{
			// The schema version is the one-way door. Retired without it means
			// an older image would still serve this database.
			name: "the one-way door is not shut",
			break_: func(t *testing.T, s *sessionDB) {
				retireAt(t, s, "r1", false, now)
				workExec(t, s, "UPDATE sp_work_meta SET version=?", workSchemaVersion)
			},
			reason: "schema version",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newTestSessionDB(t)
			seedMigratedSlot(t, s)
			test.break_(t, s)

			backup := theBackup(t, s)
			if backup["retention"] != workRetentionRequired {
				t.Fatalf("retention = %v (%v), want %s", backup["retention"], backup["reason"], workRetentionRequired)
			}
			if !strings.Contains(backup["reason"].(string), test.reason) {
				t.Fatalf("reason = %q, want it to name %q", backup["reason"], test.reason)
			}
			// And it cannot be deleted while it says that.
			decoded, code := workCall(t, s, workBackupsPruneTool, map[string]any{
				"request_id": "p1", "paths": []any{backup["path"]},
			})
			if code != "conflict" {
				t.Fatalf("prune = %q %v, want a refusal", code, decoded)
			}
			if _, err := os.Stat(backup["path"].(string)); err != nil {
				t.Fatalf("a refused prune deleted the snapshot: %v", err)
			}
		})
	}
}

// A file this code cannot identify is never eligible, however finished the move
// is. Deleting a file nothing here can name is worse than keeping it: the
// snapshot that is not the file we think it is is exactly the one to look at.
func TestWorkBackupsNeverReleaseAFileItCannotIdentify(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, reason string
		arrange      func(t *testing.T, s *sessionDB, path string)
	}{
		{
			name:   "the bytes no longer match what was recorded",
			reason: "do not match the checksum",
			arrange: func(t *testing.T, s *sessionDB, path string) {
				if err := os.WriteFile(path, []byte("not the snapshot any more"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "no run recorded this file",
			reason: "nothing here knows what it is",
			arrange: func(t *testing.T, s *sessionDB, path string) {
				workExec(t, s, "DELETE FROM sp_work_retirement WHERE step=?", workRetirementStepSnapshot)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newTestSessionDB(t)
			seedMigratedSlot(t, s)
			retireAt(t, s, "r1", false, now)
			path := theBackup(t, s)["path"].(string)
			test.arrange(t, s, path)

			backup := theBackup(t, s)
			if backup["retention"] != workRetentionUnverifiable {
				t.Fatalf("retention = %v (%v), want %s",
					backup["retention"], backup["reason"], workRetentionUnverifiable)
			}
			if !strings.Contains(backup["reason"].(string), test.reason) {
				t.Fatalf("reason = %q, want it to name %q", backup["reason"], test.reason)
			}
			// all_eligible does not sweep it up, and naming it is refused.
			swept := mustWork(t, s, workBackupsPruneTool, map[string]any{
				"request_id": "p-all", "all_eligible": true,
			})
			if swept["count"] != nil && swept["count"].(float64) != 0 {
				t.Fatalf("all_eligible deleted an unverifiable file: %v", swept)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("the unverifiable snapshot was deleted: %v", err)
			}
			decoded, code := workCall(t, s, workBackupsPruneTool, map[string]any{
				"request_id": "p1", "paths": []any{path},
			})
			if code != "conflict" {
				t.Fatalf("prune = %q %v, want a refusal", code, decoded)
			}
		})
	}
}

// One bad path refuses the whole call. A cleanup that half happened is harder
// to reason about than one that did not.
func TestWorkBackupsPruneRefusesTheWholeCallOnOneBadPath(t *testing.T) {
	s, _ := newTestSessionDB(t)
	seedMigratedSlot(t, s)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	retireAt(t, s, "r1", false, now)
	good := theBackup(t, s)["path"].(string)
	absent := filepath.Join(filepath.Dir(good), "not-a-snapshot.bak")

	decoded, code := workCall(t, s, workBackupsPruneTool, map[string]any{
		"request_id": "p1", "paths": []any{good, absent},
	})
	if code != "conflict" {
		t.Fatalf("prune = %q %v, want the whole call refused", code, decoded)
	}
	if !strings.Contains(decoded["error"].(string), "nothing was deleted") {
		t.Fatalf("refusal = %v, want it to say nothing was deleted", decoded["error"])
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatalf("the eligible file in a refused call was deleted: %v", err)
	}
	// Naming files two ways is how the wrong one goes.
	decoded, code = workCall(t, s, workBackupsPruneTool, map[string]any{
		"request_id": "p2", "paths": []any{good}, "all_eligible": true,
	})
	if code != "invalid_argument" {
		t.Fatalf("paths and all_eligible together = %q %v", code, decoded)
	}
	// And naming nothing at all is refused rather than treated as "everything".
	decoded, code = workCall(t, s, workBackupsPruneTool, map[string]any{"request_id": "p3"})
	if code != "invalid_argument" {
		t.Fatalf("an empty prune = %q %v", code, decoded)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatalf("an empty prune deleted something: %v", err)
	}
}

// A slot with nothing to clean up says so, rather than reporting an empty list
// as though it had just deleted everything.
func TestWorkBackupsOnASlotWithNoSnapshots(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-1", "c1", nil)
	listed, release := workBackupList(t, s)
	if len(listed) != 0 {
		t.Fatalf("snapshots = %v, want none on a slot that never moved", listed)
	}
	if release["released"] != true {
		t.Fatalf("release = %v, want a new slot reported as finished: it never had a ledger", release)
	}
	out := mustWork(t, s, workBackupsPruneTool, map[string]any{"request_id": "p1", "all_eligible": true})
	if out["count"].(float64) != 0 || out["note"] == nil {
		t.Fatalf("prune on an empty slot = %v, want a count of zero and a note", out)
	}
}
