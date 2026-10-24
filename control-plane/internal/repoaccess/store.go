// Package repoaccess keeps the private-repository allow list and the pending
// access requests (docs/prd/repo-access.md, AC-H1). The store is one SQLite
// file owned by a single control plane replica; AC-H1 records why that is an
// exception to AC-C1 and why the deployment pins replicas to one.
package repoaccess

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	SourceManual  = "manual"
	SourceRequest = "request"

	StatusPending = "pending"
	StatusAllowed = "allowed"

	maxReasonRunes = 500
)

var (
	ErrInvalidRepo   = errors.New("repo must be owner/repo")
	ErrInvalidReason = fmt.Errorf("reason must be 1 to %d characters", maxReasonRunes)
	ErrNotFound      = errors.New("not found")

	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type Entry struct {
	Repo             string    `json:"repo"`
	AddedBy          string    `json:"addedBy"`
	Source           string    `json:"source"`
	RequestSessionID string    `json:"requestSessionId,omitempty"`
	AddedAt          time.Time `json:"addedAt"`
}

type Request struct {
	ID             string    `json:"id"`
	Repo           string    `json:"repo"`
	SessionID      string    `json:"sessionId"`
	Reason         string    `json:"reason"`
	RequestedAt    time.Time `json:"requestedAt"`
	AlreadyAllowed bool      `json:"alreadyAllowed"`
}

// NormalizeRepo returns the canonical key. GitHub owner and repository names
// are case-insensitive, so the allow list compares them lowercased.
func NormalizeRepo(raw string) (string, error) {
	repo := strings.TrimSpace(raw)
	repo = strings.TrimSuffix(repo, ".git")
	if !repoPattern.MatchString(repo) {
		return "", ErrInvalidRepo
	}
	owner, name, _ := strings.Cut(repo, "/")
	if owner == "." || owner == ".." || name == "." || name == ".." {
		return "", ErrInvalidRepo
	}
	return strings.ToLower(repo), nil
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path. journal_mode stays at
// the rollback default: the file lives on a network filesystem in production,
// where WAL's shared-memory index is not safe.
func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("repo access database path is empty")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS allowlist (
  repo               TEXT PRIMARY KEY,
  added_by           TEXT NOT NULL,
  source             TEXT NOT NULL,
  request_session_id TEXT NOT NULL DEFAULT '',
  added_at           TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS access_requests (
  id           TEXT PRIMARY KEY,
  repo         TEXT NOT NULL,
  session_id   TEXT NOT NULL,
  reason       TEXT NOT NULL,
  requested_at TEXT NOT NULL,
  UNIQUE (repo, session_id)
);`)
	if err != nil {
		return fmt.Errorf("migrate repo access database: %w", err)
	}
	return nil
}

func (s *Store) List(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, added_by, source, request_session_id, added_at FROM allowlist ORDER BY added_at DESC, repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Allowed(ctx context.Context, rawRepo string) (bool, error) {
	repo, err := NormalizeRepo(rawRepo)
	if err != nil {
		return false, err
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM allowlist WHERE repo = ?`, repo).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Add puts repo on the list directly. created is false when it was already
// there; the existing entry is returned unchanged.
func (s *Store) Add(ctx context.Context, rawRepo, by string) (Entry, bool, error) {
	repo, err := NormalizeRepo(rawRepo)
	if err != nil {
		return Entry{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, false, err
	}
	defer tx.Rollback()
	e, created, err := s.addTx(ctx, tx, repo, by, SourceManual, "")
	if err != nil {
		return Entry{}, false, err
	}
	return e, created, tx.Commit()
}

func (s *Store) Remove(ctx context.Context, rawRepo string) (bool, error) {
	repo, err := NormalizeRepo(rawRepo)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM allowlist WHERE repo = ?`, repo)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Requests lists the pending requests, oldest first.
func (s *Store) Requests(ctx context.Context) ([]Request, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, r.repo, r.session_id, r.reason, r.requested_at, a.repo IS NOT NULL
FROM access_requests r LEFT JOIN allowlist a ON a.repo = r.repo
ORDER BY r.requested_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var r Request
		var at string
		if err := rows.Scan(&r.ID, &r.Repo, &r.SessionID, &r.Reason, &at, &r.AlreadyAllowed); err != nil {
			return nil, err
		}
		if r.RequestedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Request records a session's ask for repo (AC-H4). An allowed repo records
// nothing; a second ask from the same session for the same repo returns the
// first one, so a looping agent cannot flood the queue.
func (s *Store) Request(ctx context.Context, rawRepo, sessionID, reason string) (Request, string, error) {
	repo, err := NormalizeRepo(rawRepo)
	if err != nil {
		return Request{}, "", err
	}
	reason = strings.TrimSpace(reason)
	if n := len([]rune(reason)); n == 0 || n > maxReasonRunes {
		return Request{}, "", ErrInvalidReason
	}
	if strings.TrimSpace(sessionID) == "" {
		return Request{}, "", errors.New("session id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Request{}, "", err
	}
	defer tx.Rollback()

	var one int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM allowlist WHERE repo = ?`, repo).Scan(&one)
	if err == nil {
		return Request{Repo: repo, SessionID: sessionID}, StatusAllowed, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Request{}, "", err
	}

	existing, err := requestByKey(ctx, tx, repo, sessionID)
	if err == nil {
		return existing, StatusPending, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return Request{}, "", err
	}
	id, err := newRequestID()
	if err != nil {
		return Request{}, "", err
	}
	r := Request{ID: id, Repo: repo, SessionID: sessionID, Reason: reason, RequestedAt: s.now()}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO access_requests (id, repo, session_id, reason, requested_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.Repo, r.SessionID, r.Reason, r.RequestedAt.Format(time.RFC3339Nano)); err != nil {
		return Request{}, "", err
	}
	return r, StatusPending, tx.Commit()
}

// Approve puts the request's repo on the list and clears every pending request
// for that repo — they all asked for the same thing.
func (s *Store) Approve(ctx context.Context, id, by string) (Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, err
	}
	defer tx.Rollback()
	r, err := requestByID(ctx, tx, id)
	if err != nil {
		return Entry{}, err
	}
	e, _, err := s.addTx(ctx, tx, r.Repo, by, SourceRequest, r.SessionID)
	if err != nil {
		return Entry{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM access_requests WHERE repo = ?`, r.Repo); err != nil {
		return Entry{}, err
	}
	return e, tx.Commit()
}

func (s *Store) Reject(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM access_requests WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) addTx(ctx context.Context, tx *sql.Tx, repo, by, source, sessionID string) (Entry, bool, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT repo, added_by, source, request_session_id, added_at FROM allowlist WHERE repo = ?`, repo)
	if e, err := scanEntry(row); err == nil {
		return e, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, err
	}
	e := Entry{Repo: repo, AddedBy: by, Source: source, RequestSessionID: sessionID, AddedAt: s.now()}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO allowlist (repo, added_by, source, request_session_id, added_at) VALUES (?, ?, ?, ?, ?)`,
		e.Repo, e.AddedBy, e.Source, e.RequestSessionID, e.AddedAt.Format(time.RFC3339Nano)); err != nil {
		return Entry{}, false, err
	}
	return e, true, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanEntry(row scanner) (Entry, error) {
	var e Entry
	var at string
	if err := row.Scan(&e.Repo, &e.AddedBy, &e.Source, &e.RequestSessionID, &at); err != nil {
		return Entry{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Entry{}, err
	}
	e.AddedAt = t
	return e, nil
}

func requestByID(ctx context.Context, tx *sql.Tx, id string) (Request, error) {
	return scanRequest(tx.QueryRowContext(ctx,
		`SELECT id, repo, session_id, reason, requested_at FROM access_requests WHERE id = ?`, id))
}

func requestByKey(ctx context.Context, tx *sql.Tx, repo, sessionID string) (Request, error) {
	return scanRequest(tx.QueryRowContext(ctx,
		`SELECT id, repo, session_id, reason, requested_at FROM access_requests WHERE repo = ? AND session_id = ?`,
		repo, sessionID))
}

func scanRequest(row scanner) (Request, error) {
	var r Request
	var at string
	if err := row.Scan(&r.ID, &r.Repo, &r.SessionID, &r.Reason, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Request{}, ErrNotFound
		}
		return Request{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Request{}, err
	}
	r.RequestedAt = t
	return r, nil
}

func newRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "rq_" + hex.EncodeToString(b[:]), nil
}
