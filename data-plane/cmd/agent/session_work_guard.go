// The compare-and-swap counters that make two parents exclusive (AC-N9).
//
// The agreed task carries one version, and it moves when the *task* moves — a
// title, a status, an assignee. That version cannot also serve as the lock for
// the two other things a parent writes:
//
//   - a turn write (append, patch, dispatch) is an event on the task, not a new
//     state of it, so bumping the task version for it would move the basis the
//     app's offered choices are written against and invalidate a screen nobody
//     had touched;
//   - the parent's own bookkeeping (approval scope, session cursors,
//     preservation) is a record beside the task, and a cursor advance has no
//     business invalidating a choice either.
//
// Comparing without swapping is not a lock at all. That is what this file
// fixes: before it, a turn write and a bookkeeping write *read* the task
// version, compared it, and then only touched updated_at — so two parents
// holding the same version both won, the second silently overwriting the
// first's turn or replacing its whole approval record. Reproduced: both calls
// returned success, two turns existed, and the losing parent's approval scope
// was the one that survived.
//
// So each lane gets its own counter, kept out of the agreed entities in an
// internal management row. A write names the counter it read and the swap is a
// single conditional UPDATE: the row's own value is both the test and the
// write, so exactly one of two racing parents can succeed, and the loser is
// told to read again.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	// workGuardTurn is the lane for writes to a task's turns, and
	// workGuardBook the lane for the parent's bookkeeping beside it. They are
	// column names in sp_work_task_guard; nothing outside this file builds the
	// SQL, so the names are never caller input.
	workGuardTurn = "turn_version"
	workGuardBook = "book_version"
)

// workGuards reads both counters for a task, creating the row at 1 the first
// time a task is seen. A task that exists but has never been written in a lane
// still has a counter to name, so a first writer has something to hold.
func workGuards(ctx context.Context, tx *sql.Tx, taskID string) (turn int64, book int64, err error) {
	err = tx.QueryRowContext(ctx,
		"SELECT turn_version,book_version FROM sp_work_task_guard WHERE task_id=?", taskID).
		Scan(&turn, &book)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO sp_work_task_guard (task_id,turn_version,book_version) VALUES (?,1,1)",
			taskID); err != nil {
			return 0, 0, err
		}
		return 1, 1, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return turn, book, nil
}

// swapWorkGuard is the compare-and-swap itself: one conditional UPDATE whose
// WHERE clause carries the expected value, so the test and the write cannot be
// separated by another writer. Exactly one of two parents holding the same
// value can change it.
//
// The returned value is the counter after the swap, which is what the winner
// has to hold for its next write.
func swapWorkGuard(ctx context.Context, tx *sql.Tx, taskID, lane string, expected int64) (int64, error) {
	if _, _, err := workGuards(ctx, tx, taskID); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf("UPDATE sp_work_task_guard SET %s=%s+1 WHERE task_id=? AND %s=?", lane, lane, lane),
		taskID, expected)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed != 1 {
		return 0, workGuardConflict(lane)
	}
	return expected + 1, nil
}

// workGuardConflict names the counter the caller has to read again. A refusal
// that only said "conflict" would leave a parent guessing which of the two
// numbers it was holding had moved.
func workGuardConflict(lane string) error {
	switch lane {
	case workGuardTurn:
		return ledgerFail("conflict",
			"expected_turn_version does not match: another parent has written a turn on this task since you read it. "+
				"Read the task's turns again and write against the turn_version they carry")
	default:
		return ledgerFail("conflict",
			"expected_book_version does not match: another parent has written this task's bookkeeping since you read it. "+
				"Read it again with work_legacy_get and write against the book_version it carries")
	}
}

// dropWorkGuard removes a deleted task's counters. The retired id cannot come
// back, so keeping them would only leave a row nothing can reach.
func dropWorkGuard(ctx context.Context, tx *sql.Tx, taskID string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM sp_work_task_guard WHERE task_id=?", taskID)
	return err
}
