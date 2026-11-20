// The snapshots the transition left beside the database, and when they stop
// being required (AC-N10).
//
// Every snapshot this area takes is retained on purpose: a migration's is the
// starting point of a full restore, a rollback export's is the state before the
// ledger took over again, and a retirement's is the archive of the preserved
// copies it dropped. "Retained" was written as "never deleted", and that is not
// a policy — it is the absence of one. A slot's storage budget is shared with
// the session's own archive, so a database that keeps every snapshot for ever
// eventually fails the thing it was protecting.
//
// So retention here is a stated condition with an end: a snapshot is **required**
// while something could still need it, **eligible** once nothing can, and
// **unverifiable** when its bytes no longer match what was recorded — which is
// never eligible, because a snapshot that is not the file we think it is is
// exactly the one to investigate rather than delete.
//
// Deleting is separate from deciding, and both are explicit. work_backups
// reports; work_backups_prune deletes only the exact paths it is given, only
// when each is eligible and each still matches its recorded checksum, and
// refuses the whole call if any one of them does not — a cleanup that half
// happened is harder to reason about than one that did not.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	workBackupsTool      = "work_backups"
	workBackupsPruneTool = "work_backups_prune"

	workRetentionRequired     = "required"
	workRetentionEligible     = "eligible"
	workRetentionUnverifiable = "unverifiable"

	workBackupPruneStep = "backup_pruned"
)

// workBackup is one snapshot file beside the database.
type workBackup struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Bytes int64  `json:"bytes"`
	// SHA256 is this file's bytes now; RecordedSHA256 is what the run that took
	// it wrote down. They are both shown because the comparison is the point:
	// equal means this is the snapshot the record describes.
	SHA256         string `json:"sha256"`
	RecordedSHA256 string `json:"recorded_sha256,omitempty"`
	RecordedBytes  int64  `json:"recorded_bytes,omitempty"`
	TakenAt        string `json:"taken_at,omitempty"`
	Retention      string `json:"retention"`
	Reason         string `json:"reason"`
}

// workBackupRelease is what has to be true of this database before any snapshot
// beside it may be deleted.
//
// It replaces a fixed retention window, and the reason is that a window answers
// the wrong question. A date cannot tell whether the move the snapshot was
// protecting actually finished, whether the records it carried are reachable
// now, or whether this database is healthy enough that the snapshot is not the
// best copy in the slot. Waiting thirty days establishes none of those, and a
// snapshot kept because a clock has not run out is kept for no stated reason —
// which is the "retained for ever" this area exists to replace, with an extra
// step.
//
// So the conditions are the ones a person would actually check before deleting
// a backup, and each is read from this database rather than assumed:
//
//   - the move **finished**: the slot is retired, at the retired schema
//     version, with its own record of the run committed;
//   - it finished **successfully**: that record reports no refusals, so nothing
//     was left behind for a later run;
//   - the cleanup **happened**: every table the move removes is gone, so this
//     is not a half-run that still has the ledger beside it;
//   - the database is **sound now**: it passes its own integrity check and has
//     no turn whose task is missing — a damaged database is the one case where
//     a snapshot is the most valuable thing in the slot;
//   - the adopted records are **there**: the move recorded carrying records,
//     and the owned tables are not all empty.
//
// Each condition that fails names itself, so a snapshot that is still required
// says what it is waiting on rather than when.
type workBackupRelease struct {
	State   string `json:"state"`
	Version int    `json:"schema_version"`
	// Recorded is whether the retirement committed its own record, and Refusals
	// the count that record reports.
	Recorded bool `json:"retirement_recorded"`
	Refusals int  `json:"retirement_refusals"`
	// Remaining is every table the move removes that is still there. Empty is
	// the condition; a non-empty list is a cleanup that did not complete.
	Remaining []string `json:"transition_tables_remaining"`
	// Integrity is this database's check now, and OrphanTurns the records that
	// are reachable from nothing.
	Integrity   string `json:"integrity_check"`
	OrphanTurns int    `json:"orphan_turns"`
	// Records is what the owned tables hold now, reported so a person can
	// compare them against the counts in the retirement record. They are not a
	// floor: a parent completing and deleting work afterwards is ordinary, and
	// a snapshot that became undeletable because the slot got on with its work
	// would be retained for ever by another name.
	Records map[string]int `json:"records_now"`

	OK     bool   `json:"released"`
	Reason string `json:"reason"`
}

func workBackupsToolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name": workBackupsTool,
			"description": "The snapshots this slot's transition left beside the database: path, size, the checksum " +
				"of the bytes now and the checksum the run recorded, and a retention verdict — required (something " +
				"could still need it), eligible (nothing can) or unverifiable (the bytes no longer match what was " +
				"recorded, so it is never eligible and wants investigating rather than deleting). Each verdict " +
				"carries its reason. There is no minimum age: a snapshot is released once this database is verified " +
				"finished with it — the move is retired at the retired schema version, its retirement is recorded " +
				"with no refusals, every transition table is gone, and the database passes its integrity check with " +
				"the adopted records in it and no unreachable ones. The release object reports every condition, so a " +
				"required snapshot says which one it is waiting on. Reading changes nothing, and nothing here " +
				"deletes anything.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{},
				"required": []string{}, "additionalProperties": false},
		},
		{
			"name": workBackupsPruneTool,
			"description": "Deletes snapshot files beside this database — only those work_backups calls eligible, and " +
				"only while each still matches its recorded checksum, re-read immediately before the unlink. Name the " +
				"exact paths, or pass all_eligible=true to delete every snapshot this slot is verified finished with; " +
				"give one or the other, never both. No globs. If any named path is unknown, still required, " +
				"unverifiable or missing, the whole call is refused and nothing is deleted — a cleanup that half " +
				"happened is harder to reason about than one that did not. dry_run=true reports exactly what it would " +
				"delete and changes nothing. What was deleted is recorded, so the record does not later look like a " +
				"snapshot that was never taken. request_id replays the original committed answer.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"request_id": map[string]any{"type": "string"},
				"paths": map[string]any{
					"type": "array", "maxItems": workBackupPruneMax,
					"items":       map[string]any{"type": "string"},
					"description": "Absolute paths of the snapshots to delete, exactly as work_backups reports them.",
				},
				"all_eligible": map[string]any{
					"type": "boolean",
					"description": "Delete every snapshot work_backups calls eligible. Each is still verified one by " +
						"one, so this is the list this slot just reported rather than a wildcard.",
				},
				"dry_run": map[string]any{"type": "boolean"},
			}, "required": []string{"request_id"}, "additionalProperties": false},
		},
	}
}

// workBackupPruneMax bounds one prune call. A parent that has compared more
// snapshots than this against its own records can call again; an unbounded
// list is how a mistyped argument deletes a directory.
const workBackupPruneMax = 50

// runWorkBackups answers both snapshot tools. They work on files beside the
// database rather than rows in it, so they are not inside a record transaction
// — but the retention verdict comes from the records, so the state and the
// recorded checksums are read in one.
func (s *sessionDB) runWorkBackups(ctx context.Context, name string, a workArgs, now time.Time) (any, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return nil, err
	}
	// The integrity check reads the whole file, so it runs on its own before the
	// record transaction rather than holding one open for the length of it.
	var integrity string
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, err
	}
	var state string
	var release workBackupRelease
	recorded := map[string]workBackup{}
	if err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		var readErr error
		if state, readErr = workState(ctx, tx); readErr != nil {
			return readErr
		}
		if release, readErr = releaseWorkBackups(ctx, tx, state, integrity); readErr != nil {
			return readErr
		}
		recorded, readErr = recordedSnapshots(ctx, tx)
		return readErr
	}); err != nil {
		return nil, err
	}
	backups, err := s.scanBackups(release, recorded)
	if err != nil {
		return nil, err
	}
	if name == workBackupsTool {
		return map[string]any{
			"state": state, "backups": backups, "release": release,
			"policy": workRetentionPolicyText(),
		}, nil
	}
	return s.pruneBackups(ctx, db, a, backups, now)
}

// workBackupOwnedRecords are the owned tables a reader compares against the
// counts the retirement recorded, and the query that reads each one now.
var workBackupOwnedRecords = map[string]string{
	"work_tasks":          "SELECT count(*) FROM sp_work_tasks",
	"work_turns":          "SELECT count(*) FROM sp_work_turns",
	"bookkeeping_records": "SELECT count(*) FROM sp_work_task_book",
	"monitors":            "SELECT count(*) FROM sp_work_monitor",
	"task_origins":        "SELECT count(*) FROM sp_work_task_origin",
	"turn_origins":        "SELECT count(*) FROM sp_work_turn_origin",
}

// releaseWorkBackups establishes whether this database has finished with the
// snapshots beside it. It reads; it decides nothing about any individual file —
// that is judgeBackup's job, because a released database can still hold a
// snapshot whose own bytes no longer match what was recorded.
func releaseWorkBackups(ctx context.Context, tx *sql.Tx, state, integrity string) (workBackupRelease, error) {
	release := workBackupRelease{
		State: state, Integrity: integrity,
		Remaining: []string{}, Records: map[string]int{},
	}
	if err := tx.QueryRowContext(ctx, "SELECT version FROM sp_work_meta").Scan(&release.Version); err != nil {
		return release, err
	}
	steps, recorded, refusals, err := retirementOutcome(ctx, tx)
	if err != nil {
		return release, err
	}
	release.Recorded, release.Refusals = recorded, refusals
	for _, table := range workRetiredTables {
		present, existsErr := workTableExists(ctx, tx, table)
		if existsErr != nil {
			return release, existsErr
		}
		if present {
			release.Remaining = append(release.Remaining, table)
		}
	}
	if err = tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sp_work_turns t LEFT JOIN sp_work_tasks k ON t.task_id=k.id
		 WHERE k.id IS NULL`).Scan(&release.OrphanTurns); err != nil {
		return release, err
	}
	total := 0
	for name, query := range workBackupOwnedRecords {
		var count int
		if err = tx.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return release, err
		}
		release.Records[name] = count
		total += count
	}
	// Each condition in the order a person would check it, so the reason names
	// the first thing that is actually missing rather than the last.
	switch {
	case state != workStateRetired:
		release.Reason = "this slot is " + state + ": the move off the ledger has not finished, so an interrupted " +
			"run still resumes from these snapshots and the records they hold have not all been adopted"
	case release.Version != workSchemaVersionRetired:
		release.Reason = fmt.Sprintf(
			"this slot reads as retired but carries schema version %d rather than %d, so the one-way door is not "+
				"actually shut and an older image would still serve it", release.Version, workSchemaVersionRetired)
	case steps == 0 && len(release.Remaining) == 0:
		// Retired, at the retired version, with no transition table and not one
		// recorded step: this slot never had a ledger, which is where a new slot
		// starts. There is no move whose snapshot could still be needed. It is
		// specifically *no steps at all* rather than "no retire step", because a
		// slot whose retirement record was removed is not a slot that never had
		// one — that is the case below, and it keeps its snapshots.
		release.OK = true
		release.Reason = "this slot never had a ledger, so there is no move whose snapshot could still be needed"
	case !recorded:
		release.Reason = "no retirement is recorded in sp_work_retirement, so nothing here establishes that the " +
			"move ran — a snapshot is not released on the strength of a state flag alone"
	case refusals != 0:
		release.Reason = fmt.Sprintf(
			"the recorded retirement reports %d refusal(s): it did not carry everything, so what these snapshots "+
				"hold is still the only copy of whatever was left behind", refusals)
	case len(release.Remaining) != 0:
		release.Reason = "the cleanup did not complete — " + strings.Join(release.Remaining, ", ") +
			" is still beside this database, so these snapshots are the archive of rows that have not gone yet"
	case integrity != "ok":
		release.Reason = "this database fails its own integrity check (" + integrity + "), which is the one state " +
			"in which a snapshot is the best copy in the slot"
	case release.OrphanTurns != 0:
		release.Reason = fmt.Sprintf(
			"this database has %d turn(s) whose task is missing, which is damage a snapshot would be restored to "+
				"repair", release.OrphanTurns)
	case total == 0:
		release.Reason = "the move is recorded but every owned record table is empty, so the records these " +
			"snapshots hold are not in this database — investigate before deleting the only copy"
	default:
		release.OK = true
		release.Reason = "the move finished and recorded no refusals, every transition table is gone, this " +
			"database passes its integrity check with no unreachable records, and the adopted records are in it — " +
			"so these snapshots are a historical copy of a record shape nothing reads"
	}
	return release, nil
}

// retirementOutcome is how many steps this slot recorded on its way here,
// whether the retirement itself committed its own record, and how many refusals
// that record reports. A run that refused changed nothing, so its refusals are
// the reason the rows it would have dropped are still there.
//
// The step count is reported separately from the retire step because the two
// answer different questions: zero steps is a slot that never moved anything,
// while steps without a retire step is a move that did not finish — or a record
// somebody removed. Those must not be read as the same thing.
func retirementOutcome(ctx context.Context, tx *sql.Tx) (int, bool, int, error) {
	present, err := workTableExists(ctx, tx, "sp_work_retirement")
	if err != nil || !present {
		return 0, false, 0, err
	}
	var steps int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_retirement").Scan(&steps); err != nil {
		return 0, false, 0, err
	}
	var detail string
	err = tx.QueryRowContext(ctx,
		"SELECT detail FROM sp_work_retirement WHERE step=? ORDER BY seq DESC LIMIT 1",
		workRetirementStepRetire).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return steps, false, 0, nil
	}
	if err != nil {
		return steps, false, 0, err
	}
	var decoded struct {
		Refusals []string `json:"refusals"`
	}
	if err = json.Unmarshal([]byte(detail), &decoded); err != nil {
		// A record this code cannot read is not a record of success.
		return steps, false, 0, nil
	}
	return steps, true, len(decoded.Refusals), nil
}

// recordedSnapshots is every snapshot the transition wrote down, by path, from
// both step logs. A file with no record here is one this code did not take.
func recordedSnapshots(ctx context.Context, tx *sql.Tx) (map[string]workBackup, error) {
	out := map[string]workBackup{}
	collect := func(detail string, at string) {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
			return
		}
		// A migration records its snapshot as the step's whole detail; a
		// retirement does the same. Either way the three fields are what matter.
		path, _ := decoded["path"].(string)
		if path == "" {
			return
		}
		sum, _ := decoded["sha256"].(string)
		size, _ := decoded["bytes"].(float64)
		out[path] = workBackup{RecordedSHA256: sum, RecordedBytes: int64(size), TakenAt: at}
	}
	for _, table := range []string{"sp_work_migration", "sp_work_retirement"} {
		present, err := workTableExists(ctx, tx, table)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		rows, err := tx.QueryContext(ctx, "SELECT at,detail FROM "+table+" ORDER BY seq")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var at, detail string
			if scanErr := rows.Scan(&at, &detail); scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			collect(detail, at)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	// A retirement folded the migration's steps into its own record, so the
	// snapshots the migration took are in there too once the table is gone.
	present, err := workTableExists(ctx, tx, "sp_work_retirement")
	if err != nil || !present {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT at,detail FROM sp_work_retirement ORDER BY seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var at, detail string
		if scanErr := rows.Scan(&at, &detail); scanErr != nil {
			return nil, scanErr
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(detail), &decoded) != nil {
			continue
		}
		folded, ok := decoded["migration_steps"].([]any)
		if !ok {
			continue
		}
		for _, step := range folded {
			entry, isMap := step.(map[string]any)
			if !isMap {
				continue
			}
			stepAt, _ := entry["at"].(string)
			if stepAt == "" {
				stepAt = at
			}
			nested, isMap := entry["detail"].(map[string]any)
			if !isMap {
				continue
			}
			encoded, marshalErr := json.Marshal(nested)
			if marshalErr != nil {
				continue
			}
			collect(string(encoded), stepAt)
		}
	}
	return out, rows.Err()
}

// scanBackups is the snapshot files that are actually there, each judged. A
// file this code never recorded is reported rather than hidden — it is beside
// the database and a person clearing space needs to see it — but it is never
// eligible, because nothing here knows what it is.
func (s *sessionDB) scanBackups(
	release workBackupRelease, recorded map[string]workBackup,
) ([]workBackup, error) {
	matches, err := filepath.Glob(s.path + ".*.bak")
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	out := []workBackup{}
	for _, path := range matches {
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, statErr
		}
		backup := workBackup{Path: path, Bytes: info.Size(), Label: backupLabelOf(s.path, path)}
		sum, sumErr := fileSHA256(path)
		if sumErr != nil {
			return nil, sumErr
		}
		backup.SHA256 = sum
		if known, ok := recorded[path]; ok {
			backup.RecordedSHA256, backup.RecordedBytes, backup.TakenAt =
				known.RecordedSHA256, known.RecordedBytes, known.TakenAt
		}
		judgeBackup(&backup, release)
		out = append(out, backup)
	}
	return out, nil
}

// backupLabelOf names which run took a snapshot, from the name it was given.
func backupLabelOf(dbPath, path string) string {
	rest := strings.TrimSuffix(strings.TrimPrefix(path, dbPath+"."), ".bak")
	switch {
	case strings.HasPrefix(rest, workRetireSnapshotLabel):
		return workRetireSnapshotLabel
	case strings.HasPrefix(rest, "pre-rollback"):
		// A rollback export an older image took. This image has no rollback, so
		// the file is a historical snapshot like any other.
		return "pre-rollback"
	case strings.HasPrefix(rest, "pre-work"):
		// An older image's migration took this one.
		return "pre-work"
	default:
		return "unknown"
	}
}

// judgeBackup applies the policy to one file. The order matters: a file this
// code cannot identify is never eligible however finished the move is, because
// the one thing worse than keeping a snapshot for ever is deleting a file
// nothing here can name.
func judgeBackup(backup *workBackup, release workBackupRelease) {
	if backup.RecordedSHA256 == "" {
		backup.Retention = workRetentionUnverifiable
		backup.Reason = "no run recorded this file, so nothing here knows what it is — identify it before deleting it"
		return
	}
	if backup.SHA256 != backup.RecordedSHA256 {
		backup.Retention = workRetentionUnverifiable
		backup.Reason = "the bytes do not match the checksum the run recorded, so this is not the snapshot that " +
			"record describes — investigate rather than delete"
		return
	}
	if backup.Label == "unknown" {
		backup.Retention = workRetentionUnverifiable
		backup.Reason = "this file does not carry a name any run of this code gives a snapshot"
		return
	}
	// Everything above is about this file. What is left is about the database:
	// whether it is finished with its snapshots at all.
	if !release.OK {
		backup.Retention = workRetentionRequired
		backup.Reason = release.Reason
		return
	}
	backup.Retention = workRetentionEligible
	backup.Reason = "the bytes still match the recorded checksum, and this database is finished with its " +
		"snapshots: " + release.Reason
}

func workRetentionPolicyText() map[string]any {
	return map[string]any{
		"eligible_when": "every one of these holds, each read from this database rather than assumed: the slot is " +
			"retired at the retired schema version, its retirement is recorded and reports no refusals, every " +
			"transition table is gone, the database passes its integrity check with no turn whose task is missing, " +
			"the adopted records are in it, and this file still matches the checksum the run that took it recorded. " +
			"The release object reports each condition, so a snapshot that is still required says which one it is " +
			"waiting on",
		"required_while": "any of those does not hold — most often because the move has not finished, so an " +
			"interrupted run still resumes from this snapshot and the records it holds have not all been adopted",
		"never_eligible": "a file no run recorded, a file whose bytes no longer match the recorded checksum, or a " +
			"file whose name no run of this code would give — each is investigated rather than deleted",
		"no_time_window": "there is no minimum age. A date establishes none of the conditions above, and a snapshot " +
			"kept because a clock has not run out is kept for no stated reason — which is the \"retained for ever\" " +
			"this policy replaces. Once the move is verified finished, its snapshots are ordinary finished backups",
		"deletion": "work_backups_prune with the exact paths, or all_eligible=true for every eligible one. " +
			"Restoring a snapshot is an offline operation and deleting one is not a restore: no sidecar may have the " +
			"database open when a snapshot is copied over it",
	}
}

// pruneBackups deletes the named snapshots, or refuses the call. Every path is
// judged before anything is removed, so a list with one bad entry deletes
// nothing rather than deleting the entries before it.
func (s *sessionDB) pruneBackups(
	ctx context.Context, db *sql.DB, a workArgs, backups []workBackup, now time.Time,
) (any, error) {
	// The receipt comes first, before anything is judged. A replay of a call
	// that already deleted its files finds none of them beside the database any
	// more, so judging first would answer a successful retry with "nothing is
	// eligible" — the one answer that reads as "your deletion never happened".
	replay, err := s.replayedWorkBackupPrune(ctx, db, a)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return replay, nil
	}
	paths := a.Paths
	if a.AllEligible != nil && *a.AllEligible {
		// Every snapshot this code judged eligible, which is the ordinary way the
		// move is finished: the parent does not have to transcribe a list it just
		// read, and each file is still verified one by one below.
		paths = []string{}
		for _, backup := range backups {
			if backup.Retention == workRetentionEligible {
				paths = append(paths, backup.Path)
			}
		}
		if len(paths) == 0 {
			return map[string]any{
				"deleted": []workBackup{}, "count": 0,
				"note": "no snapshot beside this database is eligible yet; " + workBackupsTool +
					" says what each one is still waiting on",
			}, nil
		}
	}
	known := map[string]workBackup{}
	for _, backup := range backups {
		known[backup.Path] = backup
	}
	targets := []workBackup{}
	refusals := []string{}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		backup, ok := known[path]
		if !ok {
			refusals = append(refusals,
				path+" is not a snapshot beside this database that "+workBackupsTool+" reports")
			continue
		}
		if backup.Retention != workRetentionEligible {
			refusals = append(refusals, path+" is "+backup.Retention+": "+backup.Reason)
			continue
		}
		targets = append(targets, backup)
	}
	if len(refusals) != 0 {
		return nil, workFail("conflict",
			"nothing was deleted — "+strings.Join(refusals, "; "))
	}
	dryRun := a.DryRun != nil && *a.DryRun
	if dryRun {
		return map[string]any{"dry_run": true, "would_delete": targets}, nil
	}
	var result map[string]any
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		_, fingerprint, receiptErr := workReceipt(ctx, tx, workBackupsPruneTool, a)
		if receiptErr != nil {
			return receiptErr
		}
		deleted := []workBackup{}
		for _, backup := range targets {
			// Checked again immediately before the unlink: the judgement above
			// read the file, and a snapshot that changed in between is not the
			// one that was approved.
			sum, sumErr := fileSHA256(backup.Path)
			if sumErr != nil {
				return sumErr
			}
			if sum != backup.RecordedSHA256 {
				return workFail("conflict",
					backup.Path+" changed while this call was deciding; nothing further was deleted")
			}
			if removeErr := os.Remove(backup.Path); removeErr != nil {
				return removeErr
			}
			deleted = append(deleted, backup)
		}
		result = map[string]any{"deleted": deleted, "count": len(deleted)}
		if recordErr := recordWorkRetirementStep(ctx, tx, workBackupPruneStep, result, now); recordErr != nil {
			return recordErr
		}
		return storeWorkReceipt(ctx, tx, a.RequestID, fingerprint, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// replayedWorkBackupPrune is the answer this request_id already committed, or
// nil if it has not. It is a read of its own rather than part of the deleting
// transaction, because the decision about what to delete has to come after it.
func (s *sessionDB) replayedWorkBackupPrune(ctx context.Context, db *sql.DB, a workArgs) (map[string]any, error) {
	var result map[string]any
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		replay, _, receiptErr := workReceipt(ctx, tx, workBackupsPruneTool, a)
		if receiptErr != nil || replay == nil {
			return receiptErr
		}
		return json.Unmarshal(replay, &result)
	})
	return result, err
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// validateWorkBackupPrune bounds a prune call's target list. The paths
// themselves are checked against what is actually beside the database before
// anything is deleted; this only refuses a call that could not name a file at
// all, or one that names files two different ways.
func validateWorkBackupPrune(a workArgs) error {
	bad := func(s string) error { return workFail("invalid_argument", s) }
	all := a.AllEligible != nil && *a.AllEligible
	if all && len(a.Paths) != 0 {
		return bad("give paths or all_eligible, not both — naming files two ways is how the wrong one goes")
	}
	if !all && len(a.Paths) == 0 {
		return bad("name at least one snapshot path, or pass all_eligible=true; " + workBackupsTool + " reports them")
	}
	if len(a.Paths) > workBackupPruneMax {
		return bad(fmt.Sprintf("at most %d paths in one call", workBackupPruneMax))
	}
	for _, path := range a.Paths {
		if strings.TrimSpace(path) == "" || len(path) > 4096 {
			return bad("each path must be the absolute path of one snapshot")
		}
	}
	return nil
}
