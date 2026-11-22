// Package worktasks keeps the work ledger the monitor session owns and the
// parent (manager/chat) session only reads (docs/prd/manager-monitor.md,
// AC-Q1~Q5). It lives in the control plane, not in a session's slot PVC,
// because the parent must be able to read the ledger while the monitor is
// stopped, frozen or gone — a session DB behind that session's own sidecar
// cannot answer then (AC-N6 keeps `db/` sidecar-only).
//
// The store is one SQLite file owned by a single control plane replica, the
// same exception repoaccess records (repo-access.md AC-H1) and the same reason
// the deployment pins replicas to one.
//
// Role is a store argument, never a caller claim: every task write takes the
// monitor session ID the API proved by TokenReview, and every read takes the
// binding it resolved. Nothing here trusts a body field for identity.
package worktasks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Command states. accepted is intake only — it is not execution and not a
// result (AC-Q4): a parent that sees `accepted` has been promised nothing
// except that the request will not be registered twice.
const (
	CommandAccepted = "accepted"
	CommandRunning  = "running"
	CommandDone     = "done"
	CommandFailed   = "failed"
)

const (
	maxIDRunes   = 128
	maxBodyBytes = 64 * 1024
	maxHistory   = 200
)

var (
	ErrInvalidID       = fmt.Errorf("id must be 1 to %d characters of letters, digits, _ or -", maxIDRunes)
	ErrInvalidBody     = fmt.Errorf("body must be 1 to %d bytes", maxBodyBytes)
	ErrNotFound        = errors.New("not found")
	ErrVersionMismatch = errors.New("version mismatch")
	// ErrRequestReused is a request_id presented with a different payload. The
	// retry contract is "same arguments return the first commit"; anything else
	// would silently overwrite a committed change.
	ErrRequestReused = errors.New("request id already used for a different change")
	// ErrBound is a parent or monitor already bound to someone else. The 1:1
	// binding is stable on purpose (AC-Q1): rebinding would hand one parent's
	// ledger to another monitor.
	ErrBound = errors.New("already bound to a different session")

	idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

type Binding struct {
	ParentSessionID  string    `json:"parentSessionId"`
	MonitorSessionID string    `json:"monitorSessionId"`
	Tenant           string    `json:"tenant"`
	CreatedAt        time.Time `json:"createdAt"`
}

type Task struct {
	ID        string    `json:"id"`
	Version   int64     `json:"version"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updatedAt"`
	// ObservedAt is when the monitor last *looked*, which is not when the task
	// last changed. The parent shows both so "quiet" is distinguishable from
	// "not watched" (AC-Q2).
	ObservedAt time.Time `json:"observedAt,omitempty"`
}

type HistoryEntry struct {
	Version   int64     `json:"version"`
	At        time.Time `json:"at"`
	RequestID string    `json:"requestId"`
}

type Command struct {
	ID        string    `json:"id"`
	RequestID string    `json:"requestId"`
	State     string    `json:"state"`
	Body      string    `json:"body"`
	Result    string    `json:"result,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens or creates the ledger at path.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One replica owns the file; more connections only invite SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS bindings (
  parent_session_id  TEXT PRIMARY KEY,
  monitor_session_id TEXT NOT NULL UNIQUE,
  tenant             TEXT NOT NULL,
  created_at         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  monitor_session_id TEXT NOT NULL,
  id                 TEXT NOT NULL,
  version            INTEGER NOT NULL,
  body               TEXT NOT NULL,
  updated_at         INTEGER NOT NULL,
  observed_at        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (monitor_session_id, id)
);
CREATE TABLE IF NOT EXISTS receipts (
  request_id         TEXT PRIMARY KEY,
  monitor_session_id TEXT NOT NULL,
  task_id            TEXT NOT NULL,
  version            INTEGER NOT NULL,
  body              TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS history (
  monitor_session_id TEXT NOT NULL,
  task_id            TEXT NOT NULL,
  version            INTEGER NOT NULL,
  at                 INTEGER NOT NULL,
  request_id         TEXT NOT NULL,
  PRIMARY KEY (monitor_session_id, task_id, version)
);
CREATE TABLE IF NOT EXISTS commands (
  request_id         TEXT PRIMARY KEY,
  id                 TEXT NOT NULL UNIQUE,
  parent_session_id  TEXT NOT NULL,
  monitor_session_id TEXT NOT NULL,
  state              TEXT NOT NULL,
  body               TEXT NOT NULL,
  result             TEXT NOT NULL DEFAULT '',
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS commands_by_monitor ON commands (monitor_session_id, state);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

func validID(v string) bool {
	n := len([]rune(v))
	return n >= 1 && n <= maxIDRunes && idPattern.MatchString(v)
}

func validBody(v string) bool {
	return len(v) >= 1 && len(v) <= maxBodyBytes
}

// Bind records the 1:1 parent↔monitor pair. Re-binding the same pair is a
// no-op so a retry after a lost response does not fail; binding either side to
// a different session is ErrBound.
func (s *Store) Bind(ctx context.Context, parentSessionID, monitorSessionID, tenant string) (Binding, error) {
	if !validID(parentSessionID) || !validID(monitorSessionID) {
		return Binding{}, ErrInvalidID
	}
	if parentSessionID == monitorSessionID {
		return Binding{}, fmt.Errorf("%w: a session cannot monitor itself", ErrBound)
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Binding{}, err
	}
	defer tx.Rollback()

	var existingMonitor, existingTenant string
	err = tx.QueryRowContext(ctx, `SELECT monitor_session_id, tenant FROM bindings WHERE parent_session_id = ?`,
		parentSessionID).Scan(&existingMonitor, &existingTenant)
	switch {
	case err == nil && existingMonitor == monitorSessionID:
		return Binding{ParentSessionID: parentSessionID, MonitorSessionID: monitorSessionID, Tenant: existingTenant}, tx.Commit()
	case err == nil:
		return Binding{}, fmt.Errorf("%w: parent %s already has monitor %s", ErrBound, parentSessionID, existingMonitor)
	case !errors.Is(err, sql.ErrNoRows):
		return Binding{}, err
	}

	var otherParent string
	err = tx.QueryRowContext(ctx, `SELECT parent_session_id FROM bindings WHERE monitor_session_id = ?`,
		monitorSessionID).Scan(&otherParent)
	if err == nil {
		return Binding{}, fmt.Errorf("%w: monitor %s already serves parent %s", ErrBound, monitorSessionID, otherParent)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Binding{}, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bindings (parent_session_id, monitor_session_id, tenant, created_at) VALUES (?, ?, ?, ?)`,
		parentSessionID, monitorSessionID, tenant, now.Unix()); err != nil {
		return Binding{}, err
	}
	return Binding{ParentSessionID: parentSessionID, MonitorSessionID: monitorSessionID, Tenant: tenant, CreatedAt: now}, tx.Commit()
}

// BindingForParent and BindingForMonitor resolve a proved session ID to its
// pair. The API calls these before any read so a caller can only ever reach
// its own side of one binding.
func (s *Store) BindingForParent(ctx context.Context, parentSessionID string) (Binding, error) {
	return s.binding(ctx, `parent_session_id`, parentSessionID)
}

func (s *Store) BindingForMonitor(ctx context.Context, monitorSessionID string) (Binding, error) {
	return s.binding(ctx, `monitor_session_id`, monitorSessionID)
}

func (s *Store) binding(ctx context.Context, column, value string) (Binding, error) {
	var b Binding
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT parent_session_id, monitor_session_id, tenant, created_at FROM bindings WHERE `+column+` = ?`,
		value).Scan(&b.ParentSessionID, &b.MonitorSessionID, &b.Tenant, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, err
	}
	b.CreatedAt = time.Unix(created, 0).UTC()
	return b, nil
}

// PutTask is the only way a task row changes, and it takes the monitor session
// the API proved — the parent has no path here (AC-Q3). expectedVersion is 0
// for a new task and the version from GetTask for an update; a mismatch is
// ErrVersionMismatch, never a silent overwrite of the whole record.
func (s *Store) PutTask(ctx context.Context, monitorSessionID, taskID, requestID string, expectedVersion int64, body string) (Task, error) {
	if !validID(monitorSessionID) || !validID(taskID) || !validID(requestID) {
		return Task{}, ErrInvalidID
	}
	if !validBody(body) {
		return Task{}, ErrInvalidBody
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()

	// The retry receipt comes first: a repeated request_id with the same body
	// returns the commit it already made instead of bumping the version again.
	var rMonitor, rTask, rBody string
	var rVersion int64
	err = tx.QueryRowContext(ctx, `SELECT monitor_session_id, task_id, version, body FROM receipts WHERE request_id = ?`,
		requestID).Scan(&rMonitor, &rTask, &rVersion, &rBody)
	switch {
	case err == nil:
		if rMonitor != monitorSessionID || rTask != taskID || rBody != body {
			return Task{}, ErrRequestReused
		}
		t, err := readTask(ctx, tx, monitorSessionID, taskID)
		if err != nil {
			return Task{}, err
		}
		return t, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return Task{}, err
	}

	var current int64
	err = tx.QueryRowContext(ctx, `SELECT version FROM tasks WHERE monitor_session_id = ? AND id = ?`,
		monitorSessionID, taskID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Task{}, err
	}
	if expectedVersion != current {
		return Task{}, fmt.Errorf("%w: expected %d, stored %d", ErrVersionMismatch, expectedVersion, current)
	}

	next := current + 1
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tasks (monitor_session_id, id, version, body, updated_at, observed_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (monitor_session_id, id) DO UPDATE SET version = ?, body = ?, updated_at = ?, observed_at = ?`,
		monitorSessionID, taskID, next, body, now.Unix(), now.Unix(), next, body, now.Unix(), now.Unix()); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO receipts (request_id, monitor_session_id, task_id, version, body) VALUES (?, ?, ?, ?, ?)`,
		requestID, monitorSessionID, taskID, next, body); err != nil {
		return Task{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO history (monitor_session_id, task_id, version, at, request_id) VALUES (?, ?, ?, ?, ?)`,
		monitorSessionID, taskID, next, now.Unix(), requestID); err != nil {
		return Task{}, err
	}
	t := Task{ID: taskID, Version: next, Body: body, UpdatedAt: now, ObservedAt: now}
	return t, tx.Commit()
}

// Observe records that the monitor looked at a task without changing it, so
// the parent can tell a quiet task from an unwatched one. It does not move the
// version: an observation is not a change (AC-Q2).
func (s *Store) Observe(ctx context.Context, monitorSessionID, taskID string) error {
	if !validID(monitorSessionID) || !validID(taskID) {
		return ErrInvalidID
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET observed_at = ? WHERE monitor_session_id = ? AND id = ?`,
		s.now().Unix(), monitorSessionID, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readTask(ctx context.Context, q rowQuerier, monitorSessionID, taskID string) (Task, error) {
	var t Task
	var updated, observed int64
	err := q.QueryRowContext(ctx,
		`SELECT id, version, body, updated_at, observed_at FROM tasks WHERE monitor_session_id = ? AND id = ?`,
		monitorSessionID, taskID).Scan(&t.ID, &t.Version, &t.Body, &updated, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, err
	}
	t.UpdatedAt = time.Unix(updated, 0).UTC()
	if observed > 0 {
		t.ObservedAt = time.Unix(observed, 0).UTC()
	}
	return t, nil
}

// Task and Tasks are reads. They touch nothing — no claim, no cursor, no
// observation — which is what lets the parent poll them safely (AC-Q2).
func (s *Store) Task(ctx context.Context, monitorSessionID, taskID string) (Task, error) {
	if !validID(monitorSessionID) || !validID(taskID) {
		return Task{}, ErrInvalidID
	}
	return readTask(ctx, s.db, monitorSessionID, taskID)
}

func (s *Store) Tasks(ctx context.Context, monitorSessionID string) ([]Task, error) {
	if !validID(monitorSessionID) {
		return nil, ErrInvalidID
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, version, body, updated_at, observed_at FROM tasks WHERE monitor_session_id = ? ORDER BY updated_at DESC, id`,
		monitorSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var updated, observed int64
		if err := rows.Scan(&t.ID, &t.Version, &t.Body, &updated, &observed); err != nil {
			return nil, err
		}
		t.UpdatedAt = time.Unix(updated, 0).UTC()
		if observed > 0 {
			t.ObservedAt = time.Unix(observed, 0).UTC()
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) History(ctx context.Context, monitorSessionID, taskID string) ([]HistoryEntry, error) {
	if !validID(monitorSessionID) || !validID(taskID) {
		return nil, ErrInvalidID
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, at, request_id FROM history WHERE monitor_session_id = ? AND task_id = ? ORDER BY version DESC LIMIT ?`,
		monitorSessionID, taskID, maxHistory)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		var at int64
		if err := rows.Scan(&h.Version, &at, &h.RequestID); err != nil {
			return nil, err
		}
		h.At = time.Unix(at, 0).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// LastUpdated is the newest write and the newest observation across the
// monitor's tasks. Both zero means the monitor has never written here, which
// the parent must be able to say out loud rather than show an empty list as
// "nothing to do".
func (s *Store) LastUpdated(ctx context.Context, monitorSessionID string) (updated, observed time.Time, err error) {
	if !validID(monitorSessionID) {
		return time.Time{}, time.Time{}, ErrInvalidID
	}
	var u, o sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(updated_at), MAX(observed_at) FROM tasks WHERE monitor_session_id = ?`,
		monitorSessionID).Scan(&u, &o); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if u.Valid && u.Int64 > 0 {
		updated = time.Unix(u.Int64, 0).UTC()
	}
	if o.Valid && o.Int64 > 0 {
		observed = time.Unix(o.Int64, 0).UTC()
	}
	return updated, observed, nil
}

// Accept takes one instruction from the parent and is idempotent on
// requestID: a retry or a reconnect returns the command already accepted
// instead of registering a second one (AC-Q4). created is false on a retry.
func (s *Store) Accept(ctx context.Context, b Binding, requestID, body string) (cmd Command, created bool, err error) {
	if !validID(requestID) {
		return Command{}, false, ErrInvalidID
	}
	if !validBody(body) {
		return Command{}, false, ErrInvalidBody
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Command{}, false, err
	}
	defer tx.Rollback()

	existing, err := readCommand(ctx, tx, `request_id`, requestID)
	switch {
	case err == nil:
		if existing.Body != body {
			return Command{}, false, ErrRequestReused
		}
		return existing, false, tx.Commit()
	case !errors.Is(err, ErrNotFound):
		return Command{}, false, err
	}

	id := "cmd_" + strings.ReplaceAll(requestID, "-", "_")
	if _, err := tx.ExecContext(ctx, `
INSERT INTO commands (request_id, id, parent_session_id, monitor_session_id, state, body, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		requestID, id, b.ParentSessionID, b.MonitorSessionID, CommandAccepted, body, now.Unix(), now.Unix()); err != nil {
		return Command{}, false, err
	}
	return Command{ID: id, RequestID: requestID, State: CommandAccepted, Body: body, CreatedAt: now, UpdatedAt: now}, true, tx.Commit()
}

func readCommand(ctx context.Context, q rowQuerier, column, value string) (Command, error) {
	var c Command
	var created, updated int64
	err := q.QueryRowContext(ctx,
		`SELECT id, request_id, state, body, result, created_at, updated_at FROM commands WHERE `+column+` = ?`,
		value).Scan(&c.ID, &c.RequestID, &c.State, &c.Body, &c.Result, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	if err != nil {
		return Command{}, err
	}
	c.CreatedAt = time.Unix(created, 0).UTC()
	c.UpdatedAt = time.Unix(updated, 0).UTC()
	return c, nil
}

// Commands lists the binding's commands, newest first. Reading them changes
// nothing: the monitor claims one with Progress, and the parent uses the same
// list to see intake separately from the result.
func (s *Store) Commands(ctx context.Context, b Binding) ([]Command, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, request_id, state, body, result, created_at, updated_at
FROM commands WHERE monitor_session_id = ? ORDER BY created_at DESC, id`, b.MonitorSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.RequestID, &c.State, &c.Body, &c.Result, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(created, 0).UTC()
		c.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// Progress moves a command forward. Only the monitor the command was accepted
// for may call it, and only forward: accepted → running → done/failed. A
// repeated transition to the state it already holds is a no-op so a restarted
// monitor does not run the same command twice (AC-Q5).
func (s *Store) Progress(ctx context.Context, monitorSessionID, requestID, state, result string) (Command, error) {
	if !validID(monitorSessionID) || !validID(requestID) {
		return Command{}, ErrInvalidID
	}
	order := map[string]int{CommandAccepted: 0, CommandRunning: 1, CommandDone: 2, CommandFailed: 2}
	want, ok := order[state]
	if !ok {
		return Command{}, fmt.Errorf("unknown command state %q", state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Command{}, err
	}
	defer tx.Rollback()

	var owner, current string
	err = tx.QueryRowContext(ctx, `SELECT monitor_session_id, state FROM commands WHERE request_id = ?`,
		requestID).Scan(&owner, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return Command{}, ErrNotFound
	}
	if err != nil {
		return Command{}, err
	}
	if owner != monitorSessionID {
		return Command{}, ErrNotFound
	}
	if order[current] > want || (order[current] == want && current != state) {
		return Command{}, fmt.Errorf("%w: %s cannot go back to %s", ErrVersionMismatch, current, state)
	}
	if current == state {
		c, err := readCommand(ctx, tx, `request_id`, requestID)
		if err != nil {
			return Command{}, err
		}
		return c, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE commands SET state = ?, result = ?, updated_at = ? WHERE request_id = ?`,
		state, result, s.now().Unix(), requestID); err != nil {
		return Command{}, err
	}
	c, err := readCommand(ctx, tx, `request_id`, requestID)
	if err != nil {
		return Command{}, err
	}
	return c, tx.Commit()
}
