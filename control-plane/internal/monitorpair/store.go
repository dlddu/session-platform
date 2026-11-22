// Package monitorpair keeps the managed 1:1 binding between a parent
// (manager/chat) session and the monitor session that orchestrates its work.
//
// It deliberately stores *only* the pair. The work records themselves — tasks,
// turns and the request inbox — stay where they already are: the monitor's own
// slot, reached through `session.Manager.WorkTasks/WorkTurns/
// CreateWorkRequest`. Keeping a copy here would make two write origins for one
// task and hide records the agent already holds, so this package owns no task
// state at all.
//
// The binding is what makes role-aware access possible: the API proves which
// session is calling (TokenReview, as repo-access AC-H3 does), looks the pair
// up here, and only then delegates to the work records of the *bound* monitor.
// A caller therefore cannot name a monitor itself and cannot reach another
// slot.
//
// The store is one SQLite file owned by a single control plane replica — the
// same exception repoaccess records (repo-access.md AC-H1) and the same reason
// the deployment pins replicas to one.
package monitorpair

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	_ "modernc.org/sqlite"
)

const maxIDRunes = 128

var (
	ErrInvalidID = fmt.Errorf("id must be 1 to %d characters of letters, digits, _ or -", maxIDRunes)
	ErrNotFound  = errors.New("not found")
	// ErrInvalidBody is an empty instruction: there is nothing to deliver.
	ErrInvalidBody = errors.New("task id and text are required")
	// ErrRequestReused is a stable request id presented with different
	// content, or a second delegation of one already forwarded. Both would
	// mean running something twice, so both are refused.
	ErrRequestReused = errors.New("request id already used for different content")
	// ErrBound is a parent or monitor already bound to someone else. The pair
	// is stable on purpose: re-binding would hand one parent's work to another
	// monitor, and a restarted monitor must land on the same binding rather
	// than create a second one.
	ErrBound = errors.New("already bound to a different session")

	idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Role names which side of a binding a proved session holds.
type Role string

const (
	RoleParent  Role = "parent"
	RoleMonitor Role = "monitor"
)

type Binding struct {
	ParentSessionID  string    `json:"parentSessionId"`
	MonitorSessionID string    `json:"monitorSessionId"`
	Tenant           string    `json:"tenant"`
	CreatedAt        time.Time `json:"createdAt"`
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
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
CREATE TABLE IF NOT EXISTS mirror (
  monitor_session_id TEXT PRIMARY KEY,
  payload            TEXT NOT NULL,
  mirrored_at        INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS inbox (
  monitor_session_id TEXT NOT NULL,
  request_id         TEXT NOT NULL,
  task_id            TEXT NOT NULL,
  text               TEXT NOT NULL,
  author             TEXT NOT NULL,
  state              TEXT NOT NULL,
  turn_id            INTEGER,
  session_seq        INTEGER,
  detail             TEXT,
  accepted_at        INTEGER NOT NULL,
  forwarded_at       INTEGER,
  PRIMARY KEY (monitor_session_id, request_id)
);
CREATE TABLE IF NOT EXISTS snapshots (
  monitor_session_id TEXT NOT NULL,
  kind               TEXT NOT NULL,
  key                TEXT NOT NULL,
  payload            TEXT NOT NULL,
  published_at       INTEGER NOT NULL,
  PRIMARY KEY (monitor_session_id, kind, key)
);`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

func validID(v string) bool {
	n := len([]rune(v))
	return n >= 1 && n <= maxIDRunes && idPattern.MatchString(v)
}

// Bind records the pair. Re-binding the same pair returns the stored binding,
// so a lost response or a monitor restart lands on the same one; binding
// either side to a different session is ErrBound.
func (s *Store) Bind(ctx context.Context, parentSessionID, monitorSessionID, tenant string) (Binding, error) {
	if !validID(parentSessionID) || !validID(monitorSessionID) {
		return Binding{}, ErrInvalidID
	}
	if parentSessionID == monitorSessionID {
		return Binding{}, fmt.Errorf("%w: a session cannot monitor itself", ErrBound)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Binding{}, err
	}
	defer tx.Rollback()

	var storedMonitor, storedTenant string
	var storedAt int64
	err = tx.QueryRowContext(ctx,
		`SELECT monitor_session_id, tenant, created_at FROM bindings WHERE parent_session_id = ?`,
		parentSessionID).Scan(&storedMonitor, &storedTenant, &storedAt)
	switch {
	case err == nil && storedMonitor == monitorSessionID:
		return Binding{ParentSessionID: parentSessionID, MonitorSessionID: monitorSessionID,
			Tenant: storedTenant, CreatedAt: time.Unix(storedAt, 0).UTC()}, tx.Commit()
	case err == nil:
		return Binding{}, fmt.Errorf("%w: parent %s already has monitor %s", ErrBound, parentSessionID, storedMonitor)
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

	// Seconds is the stored resolution, so the value returned here must match
	// what a later read of the same row returns — otherwise a restart looks
	// like a different binding.
	now := s.now().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bindings (parent_session_id, monitor_session_id, tenant, created_at) VALUES (?, ?, ?, ?)`,
		parentSessionID, monitorSessionID, tenant, now.Unix()); err != nil {
		return Binding{}, err
	}
	return Binding{ParentSessionID: parentSessionID, MonitorSessionID: monitorSessionID, Tenant: tenant, CreatedAt: now}, tx.Commit()
}

// For resolves a proved session ID to its binding and the role it holds there.
// This is the only way the API learns a monitor ID: a request body naming one
// is ignored, so a client cannot point itself at another pair's work.
func (s *Store) For(ctx context.Context, sessionID string) (Binding, Role, error) {
	if !validID(sessionID) {
		return Binding{}, "", ErrInvalidID
	}
	b, err := s.lookup(ctx, `parent_session_id`, sessionID)
	if err == nil {
		return b, RoleParent, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Binding{}, "", err
	}
	b, err = s.lookup(ctx, `monitor_session_id`, sessionID)
	if err != nil {
		return Binding{}, "", err
	}
	return b, RoleMonitor, nil
}

func (s *Store) lookup(ctx context.Context, column, value string) (Binding, error) {
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

// Mirror is the last page of work records the control plane managed to read
// from a monitor's slot, with the time it read them.
//
// It is **not** a second write origin: only a successful read of the slot
// writes it, nothing here ever flows back to the slot, and it is served only
// with its own `asOf` so a reader can never mistake it for live state. It
// exists because the parent must still see its list, detail, history and last
// update time while the monitor is frozen or stopped — the slot's SQLite is
// reachable only through that session's own sidecar, so when the pod is gone
// there is nothing to proxy to.
type Mirror struct {
	Payload    string    `json:"payload"`
	MirroredAt time.Time `json:"mirroredAt"`
}

// RecordMirror stores the payload of a successful live read. The monitor ID is
// the one the API proved and resolved, so a caller cannot seed another pair's
// mirror.
func (s *Store) RecordMirror(ctx context.Context, monitorSessionID, payload string) error {
	if !validID(monitorSessionID) {
		return ErrInvalidID
	}
	if payload == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mirror (monitor_session_id, payload, mirrored_at) VALUES (?, ?, ?)
ON CONFLICT (monitor_session_id) DO UPDATE SET payload = ?, mirrored_at = ?`,
		monitorSessionID, payload, s.now().Truncate(time.Second).Unix(), payload, s.now().Truncate(time.Second).Unix())
	return err
}

// LastMirror returns the stored read, or ErrNotFound when the control plane
// has never managed to read this monitor's slot — which the parent must be
// told rather than shown an empty list.
func (s *Store) LastMirror(ctx context.Context, monitorSessionID string) (Mirror, error) {
	if !validID(monitorSessionID) {
		return Mirror{}, ErrInvalidID
	}
	var m Mirror
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT payload, mirrored_at FROM mirror WHERE monitor_session_id = ?`, monitorSessionID).Scan(&m.Payload, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Mirror{}, ErrNotFound
	}
	if err != nil {
		return Mirror{}, err
	}
	m.MirroredAt = time.Unix(at, 0).UTC()
	return m, nil
}

// Inbox states. accepted means durable intake only: the control plane has the
// instruction and will not take it twice, and nothing has run yet. forwarded
// means the monitor's slot took it as a turn, and that turn's id is the handle
// the monitor and the parent both use afterwards.
const (
	InboxAccepted  = "accepted"
	InboxForwarded = "forwarded"
	InboxRefused   = "refused"
)

// InboxItem is one instruction the parent sent. It is durable so a monitor
// that is asleep, frozen or restarting still receives it: the slot cannot hold
// a request while its pod is gone, and dropping it would be a silent loss.
//
// RequestID is the parent's stable key. The same key with the same text always
// returns the item already stored — that is the receipt, and it is what makes
// a lost response safe to retry. TurnID is filled when the slot accepted it,
// so a restarted monitor can tell "already delegated" from "still to do"
// without guessing.
type InboxItem struct {
	RequestID   string     `json:"requestId"`
	TaskID      string     `json:"taskId"`
	Text        string     `json:"text"`
	Author      string     `json:"author"`
	State       string     `json:"state"`
	TurnID      *int64     `json:"turnId,omitempty"`
	SessionSeq  *int64     `json:"sessionSeq,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	AcceptedAt  time.Time  `json:"acceptedAt"`
	ForwardedAt *time.Time `json:"forwardedAt,omitempty"`
}

// Accept stores one instruction durably, once. created is false when the same
// request id was already accepted, and the stored item comes back unchanged;
// the same id with different text is ErrRequestReused rather than an
// overwrite, because the first one may already be running.
func (s *Store) Accept(ctx context.Context, monitorSessionID, requestID, taskID, text, author string) (item InboxItem, created bool, err error) {
	if !validID(monitorSessionID) || !validID(requestID) {
		return InboxItem{}, false, ErrInvalidID
	}
	if taskID == "" || text == "" {
		return InboxItem{}, false, ErrInvalidBody
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InboxItem{}, false, err
	}
	defer tx.Rollback()

	stored, err := readInbox(ctx, tx, monitorSessionID, requestID)
	switch {
	case err == nil:
		if stored.Text != text || stored.TaskID != taskID {
			return InboxItem{}, false, ErrRequestReused
		}
		return stored, false, tx.Commit()
	case !errors.Is(err, ErrNotFound):
		return InboxItem{}, false, err
	}

	now := s.now().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO inbox (monitor_session_id, request_id, task_id, text, author, state, accepted_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		monitorSessionID, requestID, taskID, text, author, InboxAccepted, now.Unix()); err != nil {
		return InboxItem{}, false, err
	}
	return InboxItem{RequestID: requestID, TaskID: taskID, Text: text, Author: author,
		State: InboxAccepted, AcceptedAt: now}, true, tx.Commit()
}

// Pending is what the monitor has not forwarded yet, oldest first. Reading it
// changes nothing: the monitor claims an item by forwarding it, not by looking.
func (s *Store) Pending(ctx context.Context, monitorSessionID string) ([]InboxItem, error) {
	if !validID(monitorSessionID) {
		return nil, ErrInvalidID
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT request_id, task_id, text, author, state, turn_id, session_seq, detail, accepted_at, forwarded_at
FROM inbox WHERE monitor_session_id = ? AND state = ? ORDER BY accepted_at, request_id`,
		monitorSessionID, InboxAccepted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		item, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// Item is one inbox entry by its request id, which is how a parent asks "what
// happened to the thing I sent".
func (s *Store) Item(ctx context.Context, monitorSessionID, requestID string) (InboxItem, error) {
	if !validID(monitorSessionID) || !validID(requestID) {
		return InboxItem{}, ErrInvalidID
	}
	return readInbox(ctx, s.db, monitorSessionID, requestID)
}

// MarkForwarded records that the slot took this instruction as a turn. It is
// idempotent on the same turn id, so a monitor that crashed between the slot
// write and this call can repeat it; a *different* turn id for an already
// forwarded request is refused, because that would mean it ran twice.
func (s *Store) MarkForwarded(ctx context.Context, monitorSessionID, requestID string, turnID int64, sessionSeq *int64) (InboxItem, error) {
	if !validID(monitorSessionID) || !validID(requestID) {
		return InboxItem{}, ErrInvalidID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InboxItem{}, err
	}
	defer tx.Rollback()
	stored, err := readInbox(ctx, tx, monitorSessionID, requestID)
	if err != nil {
		return InboxItem{}, err
	}
	if stored.State == InboxForwarded {
		if stored.TurnID != nil && *stored.TurnID != turnID {
			return InboxItem{}, fmt.Errorf("%w: already forwarded as turn %d", ErrRequestReused, *stored.TurnID)
		}
		return stored, tx.Commit()
	}
	now := s.now().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx, `
UPDATE inbox SET state = ?, turn_id = ?, session_seq = ?, forwarded_at = ?
WHERE monitor_session_id = ? AND request_id = ?`,
		InboxForwarded, turnID, sessionSeq, now.Unix(), monitorSessionID, requestID); err != nil {
		return InboxItem{}, err
	}
	out, err := readInbox(ctx, tx, monitorSessionID, requestID)
	if err != nil {
		return InboxItem{}, err
	}
	return out, tx.Commit()
}

// MarkRefused records that this instruction will not run, with why. It is for
// a real refusal — a stale choice, a task that ended — never for an unknown
// outcome, which must stay accepted so the next drain reconciles it.
func (s *Store) MarkRefused(ctx context.Context, monitorSessionID, requestID, detail string) (InboxItem, error) {
	if !validID(monitorSessionID) || !validID(requestID) {
		return InboxItem{}, ErrInvalidID
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE inbox SET state = ?, detail = ? WHERE monitor_session_id = ? AND request_id = ? AND state = ?`,
		InboxRefused, detail, monitorSessionID, requestID, InboxAccepted); err != nil {
		return InboxItem{}, err
	}
	return readInbox(ctx, s.db, monitorSessionID, requestID)
}

type inboxScanner interface {
	Scan(dest ...any) error
}

func scanInbox(row inboxScanner) (InboxItem, error) {
	var it InboxItem
	var turnID, seq, forwardedAt sql.NullInt64
	var detail sql.NullString
	var acceptedAt int64
	if err := row.Scan(&it.RequestID, &it.TaskID, &it.Text, &it.Author, &it.State,
		&turnID, &seq, &detail, &acceptedAt, &forwardedAt); err != nil {
		return InboxItem{}, err
	}
	it.AcceptedAt = time.Unix(acceptedAt, 0).UTC()
	if turnID.Valid {
		v := turnID.Int64
		it.TurnID = &v
	}
	if seq.Valid {
		v := seq.Int64
		it.SessionSeq = &v
	}
	if detail.Valid {
		it.Detail = detail.String
	}
	if forwardedAt.Valid {
		t := time.Unix(forwardedAt.Int64, 0).UTC()
		it.ForwardedAt = &t
	}
	return it, nil
}

func readInbox(ctx context.Context, q rowQuerier, monitorSessionID, requestID string) (InboxItem, error) {
	row := q.QueryRowContext(ctx, `
SELECT request_id, task_id, text, author, state, turn_id, session_seq, detail, accepted_at, forwarded_at
FROM inbox WHERE monitor_session_id = ? AND request_id = ?`, monitorSessionID, requestID)
	it, err := scanInbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return InboxItem{}, ErrNotFound
	}
	return it, err
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// PublishSnapshot is the monitor publishing its own committed state: after it
// registers, changes or finishes a task it pushes the page (and a task's turn
// history) here, so the parent can read the latest stored state even for work
// it never looked at and even while the monitor is asleep.
//
// It is not a second write origin. The slot is still where the records are
// written and the monitor is still the only writer; this is that writer's own
// published copy, served read-only and always with the time it was published.
func (s *Store) PublishSnapshot(ctx context.Context, monitorSessionID, kind, key, payload string) error {
	if !validID(monitorSessionID) || kind == "" {
		return ErrInvalidID
	}
	if payload == "" {
		return ErrInvalidBody
	}
	now := s.now().Truncate(time.Second).Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO snapshots (monitor_session_id, kind, key, payload, published_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (monitor_session_id, kind, key) DO UPDATE SET payload = ?, published_at = ?`,
		monitorSessionID, kind, key, payload, now, payload, now)
	return err
}

// Snapshot reads a published copy. ErrNotFound means the monitor has never
// published this view, which the parent must be told rather than shown an
// empty list.
func (s *Store) Snapshot(ctx context.Context, monitorSessionID, kind, key string) (Mirror, error) {
	if !validID(monitorSessionID) {
		return Mirror{}, ErrInvalidID
	}
	var m Mirror
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT payload, published_at FROM snapshots WHERE monitor_session_id = ? AND kind = ? AND key = ?`,
		monitorSessionID, kind, key).Scan(&m.Payload, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Mirror{}, ErrNotFound
	}
	if err != nil {
		return Mirror{}, err
	}
	m.MirroredAt = time.Unix(at, 0).UTC()
	return m, nil
}
