package main

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The task side of the work records. A task is replaced whole under its
// version, the way the ledger task was: a parent reads, changes what it means
// to change, and writes the version it read — so two parents cannot both
// believe they moved the same task.

const workTaskColumns = `id,session_id,title,description,completion_criteria,status,version,created_at,updated_at`

func scanWorkTask(row interface{ Scan(...any) error }) (workTask, error) {
	var task workTask
	var sessionID, description, criteria, createdAt, updatedAt sql.NullString
	err := row.Scan(&task.ID, &sessionID, &task.Title, &description, &criteria,
		&task.Status, &task.Version, &createdAt, &updatedAt)
	if err != nil {
		return task, err
	}
	task.SessionID = nullableString(sessionID)
	task.Description = nullableString(description)
	task.CompletionCriteria = nullableString(criteria)
	task.CreatedAt = nullableString(createdAt)
	task.UpdatedAt = nullableString(updatedAt)
	return task, nil
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

func loadWorkTask(ctx context.Context, tx *sql.Tx, id string) (workTask, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+workTaskColumns+" FROM sp_work_tasks WHERE id=?", id)
	task, err := scanWorkTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return task, ledgerFail("not_found", "task not found")
	}
	return task, err
}

// readWorkTask answers one task with the two facts a reader would otherwise
// need a second call for: how many turns it has, and the last one — which is
// where an unanswered request or an open question sits.
func readWorkTask(ctx context.Context, tx *sql.Tx, id string) (any, error) {
	task, err := loadWorkTask(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	total, latest, err := workTurnTail(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"task": task, "turns_total": total}
	if latest != nil {
		result["latest_turn"] = *latest
	}
	return result, nil
}

// listWorkTasks pages tasks by id. session_id="none" selects the unassigned
// ones: an empty filter cannot express "no session" because an empty string is
// also "no filter".
func listWorkTasks(ctx context.Context, tx *sql.Tx, a workArgs) (any, error) {
	limit := a.Limit
	if limit == 0 {
		limit = workListLimitDefault
	}
	query := "SELECT " + workTaskColumns + ` FROM sp_work_tasks
		WHERE id>? AND (?='' OR status=?)
		  AND (?='' OR (?='none' AND session_id IS NULL) OR session_id=?)
		ORDER BY id LIMIT ?`
	rows, err := tx.QueryContext(ctx, query,
		a.AfterID, a.Status, a.Status, a.SessionID, a.SessionID, a.SessionID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []workTask{}
	next := ""
	for rows.Next() {
		task, scanErr := scanWorkTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if len(tasks) == limit {
			next = tasks[len(tasks)-1].ID
			break
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"tasks": tasks, "next_cursor": next}, nil
}

// putWorkTask creates or replaces one task. created_at is written once and then
// carried: a replacement is a new state of the same task, not a new task.
func putWorkTask(ctx context.Context, tx *sql.Tx, a workArgs, now time.Time) (any, error) {
	var current workTask
	existing := true
	row := tx.QueryRowContext(ctx, "SELECT "+workTaskColumns+" FROM sp_work_tasks WHERE id=?", a.ID)
	current, err := scanWorkTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		existing = false
	} else if err != nil {
		return nil, err
	}
	stamp := workNow(now)
	if !existing {
		if *a.ExpectedVersion != 0 {
			return nil, ledgerFail("not_found", "task not found")
		}
		var retired int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sp_work_retired WHERE id=?", a.ID).Scan(&retired); err != nil {
			return nil, err
		}
		if retired != 0 {
			return nil, ledgerFail("conflict", "deleted task ID cannot be reused")
		}
		task := workTask{
			ID: a.ID, SessionID: a.Task.SessionID, Title: *a.Task.Title,
			Description: a.Task.Description, CompletionCriteria: a.Task.CompletionCriteria,
			Status: *a.Task.Status, Version: 1, CreatedAt: &stamp, UpdatedAt: &stamp,
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO sp_work_tasks ("+workTaskColumns+") VALUES (?,?,?,?,?,?,?,?,?)",
			task.ID, task.SessionID, task.Title, task.Description, task.CompletionCriteria,
			task.Status, task.Version, task.CreatedAt, task.UpdatedAt)
		if err != nil {
			return nil, err
		}
		return map[string]any{"task": task}, nil
	}
	if *a.ExpectedVersion == 0 || *a.ExpectedVersion != current.Version {
		return nil, ledgerFail("conflict", "expected_version does not match")
	}
	task := workTask{
		ID: a.ID, SessionID: a.Task.SessionID, Title: *a.Task.Title,
		Description: a.Task.Description, CompletionCriteria: a.Task.CompletionCriteria,
		Status: *a.Task.Status, Version: current.Version + 1,
		CreatedAt: current.CreatedAt, UpdatedAt: &stamp,
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE sp_work_tasks SET session_id=?,title=?,description=?,completion_criteria=?,status=?,version=?,updated_at=?
		 WHERE id=? AND version=?`,
		task.SessionID, task.Title, task.Description, task.CompletionCriteria, task.Status,
		task.Version, task.UpdatedAt, a.ID, *a.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": task}, nil
}

// deleteWorkTask removes a task with its turns and retires the id. What a
// migration preserved about the task (sp_work_task_legacy) is left: the parent's
// own bookkeeping outlives the task row, exactly as the ledger's history does.
func deleteWorkTask(ctx context.Context, tx *sql.Tx, a workArgs) (any, error) {
	current, err := loadWorkTask(ctx, tx, a.ID)
	if err != nil {
		return nil, err
	}
	if current.Version != *a.ExpectedVersion {
		return nil, ledgerFail("conflict", "expected_version does not match")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sp_work_turns WHERE task_id=?", a.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM sp_work_tasks WHERE id=? AND version=?", a.ID, current.Version); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO sp_work_retired VALUES (?)", a.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": a.ID, "version": current.Version, "deleted": true}, nil
}

// touchWorkTask moves a task's updated_at without moving its version. A turn is
// an event on the task rather than a new state of it, so a parent holding the
// version it read can still write the task it was working on.
func touchWorkTask(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE sp_work_tasks SET updated_at=? WHERE id=?", workNow(now), id)
	return err
}
