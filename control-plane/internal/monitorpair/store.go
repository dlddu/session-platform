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
