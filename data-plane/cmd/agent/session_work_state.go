// Where this slot's records stand, after the move that put them here is gone
// (AC-N10).
//
// The ledger retirement arrived with three tools of its own — the one-way door
// that adopted and dropped, and the pair that reported and pruned the snapshots
// it left beside the database. They were right while a slot could still be
// mid-move. They are wrong once no slot can be: a finished move that still
// offers to move describes records that no longer exist, and the step logs it
// kept are a history of a transition rather than a record of anyone's work.
//
// So those three are gone and this is what is left: one read that answers "what
// does this slot keep" from the records themselves, by counting them. It does
// not replay a run's own account of what it did — the counts are re-measured on
// every call, so a reader checks rather than trusts.
//
// What protects a slot the move never reached is not here but in the schema
// gate (session_work.go, migrateWork): a database whose ledger was never
// adopted is refused outright rather than served the empty page it would
// otherwise produce, because this image no longer has the door that would move
// it.
package main

import (
	"context"
	"database/sql"
	"sort"
)

const (
	// workStateTool is the read-only state oracle, and the one call from the
	// transition that outlives it: a parent has to know which records this slot
	// keeps before it touches any of them, and afterwards the answer is still a
	// fact rather than an assumption.
	workStateTool = "work_state"

	// workStateRetired is the terminal state, and now the only one this image
	// serves. workStateRetiring is kept because a database can still carry it:
	// an interrupted move left the fence up, and a fence this image cannot lower
	// has to be recognised to be refused (session_work.go).
	workStateRetiring = "retiring"
	workStateRetired  = "retired"
)

// workStateReport is where this slot's records stand: the state and schema
// version the database carries, and the counts as they are now.
func workStateReport(ctx context.Context, tx *sql.Tx) (any, error) {
	state, err := workState(ctx, tx)
	if err != nil {
		return nil, err
	}
	version := workSchemaVersionRetired
	if err = tx.QueryRowContext(ctx, "SELECT version FROM sp_work_meta").Scan(&version); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	queries := map[string]string{
		"work_tasks":          "SELECT count(*) FROM sp_work_tasks",
		"work_turns":          "SELECT count(*) FROM sp_work_turns",
		"open_turns":          "SELECT count(*) FROM sp_work_turns WHERE request IS NOT NULL AND ended_at IS NULL",
		"bookkeeping_records": "SELECT count(*) FROM sp_work_task_book",
		"monitors":            "SELECT count(*) FROM sp_work_monitor",
		"tasks_from_ledger":   "SELECT count(*) FROM sp_work_task_origin WHERE origin='" + workOriginLedger + "'",
		"turn_origins":        "SELECT count(*) FROM sp_work_turn_origin",
		"retired_ids":         "SELECT count(*) FROM sp_work_retired",
	}
	for name, query := range queries {
		var count int
		if err := tx.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	return map[string]any{
		"state":          state,
		"schema_version": version,
		"counts":         counts,
		"removed_tools":  workRemovedToolNames(),
	}, nil
}

// workRemovedToolNames is every name this image answers with a successor rather
// than with "unknown tool". A parent that still holds an older tools/list reads
// it here instead of discovering one refusal at a time.
func workRemovedToolNames() []string {
	names := make([]string, 0, len(workRemovedTools))
	for name := range workRemovedTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func workStateToolDefinition() map[string]any {
	return map[string]any{
		"name": workStateTool,
		"description": "Where this slot's records stand: state (retired = the work records are the only " +
			"records, which is where every slot this image serves is, and where a new slot starts), the schema " +
			"version, and the current counts of tasks, turns, open turns, bookkeeping records and monitoring " +
			"loops, plus the names of the tools this image removed and what to call instead. Read this before " +
			"deciding which records to use; do not infer the state from which tools exist. Reading changes nothing.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{},
			"required": []string{}, "additionalProperties": false},
	}
}

// workTx runs one read or write against the work records in its own
// transaction, with the schema gate ahead of it — so a caller can never reach
// the records of a slot the gate refuses (session_work.go, migrateWork).
func (s *sessionDB) workTx(ctx context.Context, db *sql.DB, body func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrateWork(ctx, tx); err != nil {
		return err
	}
	if err := body(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sessionDB) workStateReportOf(ctx context.Context, db *sql.DB) (map[string]any, error) {
	var report map[string]any
	err := s.workTx(ctx, db, func(tx *sql.Tx) error {
		result, readErr := workStateReport(ctx, tx)
		if readErr != nil {
			return readErr
		}
		report = result.(map[string]any)
		return nil
	})
	return report, err
}
