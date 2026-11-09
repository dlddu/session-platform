// Session DB MCP (docs/prd/agent-auth-pool.md AC-N6): a codex-chat session's
// SQLite database, served by the loopback k3s-mcp-proxy sidecar from the
// database directory of the session's slot PVC. The workload container never
// mounts that directory; it reaches the database only through this endpoint,
// which the managed config.toml registers as the `db` MCP server.
//
// Raw SQL remains available alongside the versioned task ledger.
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	// Keep in sync with control-plane/internal/adapter/k8s (SessionDBDirEnvVar,
	// SessionDBMCPURLEnvVar, sessionDBMCPPath).
	sessionDBDirEnv    = "SESSION_DB_DIR"
	sessionDBMCPURLEnv = "SESSION_DB_MCP_URL"
	sessionDBMCPPath   = "/session-db/mcp"

	sessionDBFileName      = "session.db"
	sessionDBMCPServerName = "session-platform-session-db"

	// sessionDBMaxRows bounds one raw_query result; a larger result says it
	// was truncated rather than flooding the conversation.
	sessionDBMaxRows = 1000
	// sessionDBMaxResultBytes bounds the encoded rows for the same reason.
	sessionDBMaxResultBytes = 1 << 20
	// sessionDBCallTimeout bounds one statement, busy waits included.
	sessionDBCallTimeout = 30 * time.Second
)

// sessionDB opens the database on first use and again after a failed open, so
// a directory that was not ready when the sidecar started costs a tool error,
// not the sidecar (which also carries the K3s MCP).
type sessionDB struct {
	path   string
	logger *slog.Logger

	mu sync.Mutex
	db *sql.DB
}

func newSessionDB(dir string, logger *slog.Logger) (*sessionDB, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("%s must be an absolute path", sessionDBDirEnv)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionDB{path: filepath.Join(filepath.Clean(dir), sessionDBFileName), logger: logger}, nil
}

// handle returns the open database. journal_mode stays DELETE: the slot lives
// on NFS (EFS) in production, where WAL's shared memory does not work, and the
// sidecar is the only process that opens the file.
func (s *sessionDB) handle(ctx context.Context) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, nil
	}
	if info, err := os.Stat(filepath.Dir(s.path)); err != nil {
		return nil, fmt.Errorf("session database directory: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("session database directory %s is not a directory", filepath.Dir(s.path))
	}
	db, err := sql.Open("sqlite", "file:"+s.path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open session database: %w", err)
	}
	s.db = db
	s.logger.Info("session database open", "path", s.path)
	return db, nil
}

func (s *sessionDB) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

type sessionDBStatement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

type sessionDBQueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated,omitempty"`
}

type sessionDBExecResult struct {
	RowsAffected int64 `json:"rowsAffected"`
	LastInsertID int64 `json:"lastInsertId"`
}

func (s *sessionDB) query(ctx context.Context, stmt sessionDBStatement) (sessionDBQueryResult, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return sessionDBQueryResult{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return sessionDBQueryResult{}, err
	}
	defer conn.Close()
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	rows, err := conn.QueryContext(ctx, stmt.SQL, stmt.Params...)
	if err != nil {
		return sessionDBQueryResult{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return sessionDBQueryResult{}, err
	}
	result := sessionDBQueryResult{Columns: columns, Rows: [][]any{}}
	size := 0
	for rows.Next() {
		if len(result.Rows) == sessionDBMaxRows || size > sessionDBMaxResultBytes {
			result.Truncated = true
			break
		}
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return sessionDBQueryResult{}, err
		}
		for i, v := range values {
			values[i] = sessionDBValue(v)
		}
		if encoded, err := json.Marshal(values); err == nil {
			size += len(encoded)
		}
		result.Rows = append(result.Rows, values)
	}
	return result, rows.Err()
}

func (s *sessionDB) exec(ctx context.Context, stmt sessionDBStatement) (sessionDBExecResult, error) {
	db, err := s.handle(ctx)
	if err != nil {
		return sessionDBExecResult{}, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return sessionDBExecResult{}, err
	}
	defer conn.Close()
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	res, err := conn.ExecContext(ctx, stmt.SQL, stmt.Params...)
	if err != nil {
		return sessionDBExecResult{}, err
	}
	// SQLite answers both; an error here would only mean a driver that does
	// not, which is reported as zero rather than failing a statement that ran.
	affected, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId()
	return sessionDBExecResult{RowsAffected: affected, LastInsertID: lastID}, nil
}

// sessionDBValue makes a scanned value JSON-safe: text stays text, a blob
// that is not UTF-8 becomes {"base64": ...}, and a time is RFC 3339.
func sessionDBValue(v any) any {
	switch t := v.(type) {
	case []byte:
		if utf8.Valid(t) {
			return string(t)
		}
		return map[string]string{"base64": base64.StdEncoding.EncodeToString(t)}
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

func sessionDBToolDefinitions() []map[string]any {
	statement := func(what string) map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sql": map[string]any{"type": "string", "description": what},
				"params": map[string]any{
					"type":        "array",
					"description": "Values bound to the statement's ? placeholders, in order.",
					"items":       map[string]any{},
				},
			},
			"required":             []string{"sql"},
			"additionalProperties": false,
		}
	}
	return append([]map[string]any{
		{
			"name": "raw_query",
			"description": fmt.Sprintf("Run one SQL statement on this session's SQLite database and return its columns and rows "+
				"(at most %d rows). Use it for SELECT and other statements that return rows.", sessionDBMaxRows),
			"inputSchema": statement("The SQLite statement to run."),
		},
		{
			"name": "raw_exec",
			"description": "Run SQL that changes this session's SQLite database (CREATE, INSERT, UPDATE, DELETE, ...) and " +
				"return the rows affected and the last insert rowid. These are SQLite connection counters, NOT committed change totals (DDL/ROLLBACK can retain old values). Without params, several statements separated by ; run in order. Open transactions are rolled back at call end; use BEGIN/COMMIT in one call.",
			"inputSchema": statement("The SQLite statement or statements to run."),
		},
	}, ledgerToolDefinitions()...)
}

// callTool runs one tool. A statement that fails is a tool result with
// isError, so the model sees SQLite's message; only a malformed call is a
// JSON-RPC error.
func (s *sessionDB) callTool(ctx context.Context, params json.RawMessage) (any, *jsonRPCError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "tools/call needs a name and arguments"}
	}
	if strings.HasPrefix(call.Name, "ledger_") {
		return s.callLedger(ctx, call.Name, call.Arguments)
	}
	var stmt sessionDBStatement
	if len(call.Arguments) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		if err := decoder.Decode(&stmt); err != nil {
			return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "arguments must be {sql, params?}: " + err.Error()}
		}
	}
	if strings.TrimSpace(stmt.SQL) == "" {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "sql must not be empty"}
	}
	stmt.Params = sessionDBParams(stmt.Params)
	ctx, cancel := context.WithTimeout(ctx, sessionDBCallTimeout)
	defer cancel()
	var (
		result any
		err    error
	)
	switch call.Name {
	case "raw_query":
		result, err = s.query(ctx, stmt)
	case "raw_exec":
		result, err = s.exec(ctx, stmt)
	default:
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "unknown tool " + call.Name}
	}
	if err != nil {
		s.logger.Info("session database statement failed", "tool", call.Name, "err", err)
		return sessionDBToolResult(map[string]string{"error": err.Error()}, true), nil
	}
	return sessionDBToolResult(result, false), nil
}

// sessionDBParams turns decoded JSON numbers into int64 where they are whole,
// so an integer parameter binds as an INTEGER rather than a REAL.
func sessionDBParams(params []any) []any {
	out := make([]any, len(params))
	for i, p := range params {
		switch v := p.(type) {
		case json.Number:
			if n, err := v.Int64(); err == nil {
				out[i] = n
			} else if f, err := v.Float64(); err == nil {
				out[i] = f
			} else {
				out[i] = v.String()
			}
		case nil, string, bool:
			out[i] = v
		default:
			// Arrays and objects have no SQLite type; they bind as their JSON
			// text, which SQLite's json functions read.
			encoded, _ := json.Marshal(v)
			out[i] = string(encoded)
		}
	}
	return out
}

func sessionDBToolResult(payload any, isError bool) map[string]any {
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(`{"error":"result could not be encoded"}`)
		isError = true
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(encoded)}},
		"isError": isError,
	}
}

// serveMCP is the MCP endpoint: the same JSON-RPC-over-POST shape as
// the session MCP (session_mcp.go).
func (s *sessionDB) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "session DB MCP accepts POST", http.StatusMethodNotAllowed)
		return
	}
	var req jsonRPCRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionMCPRequestBytes)).Decode(&req); err != nil {
		writeJSONRPCError(w, nil, jsonRPCParseError, "malformed JSON-RPC request")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSONRPCError(w, req.ID, jsonRPCInvalidRequest, "not a JSON-RPC 2.0 request")
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		writeJSONRPCResult(w, req.ID, map[string]any{
			"protocolVersion": sessionMCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": sessionDBMCPServerName, "version": sessionMCPServerVersion},
		})
	case "tools/list":
		writeJSONRPCResult(w, req.ID, map[string]any{"tools": sessionDBToolDefinitions()})
	case "tools/call":
		result, rpcErr := s.callTool(r.Context(), req.Params)
		if rpcErr != nil {
			writeJSONRPCError(w, req.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		writeJSONRPCResult(w, req.ID, result)
	case "ping":
		writeJSONRPCResult(w, req.ID, map[string]any{})
	default:
		writeJSONRPCError(w, req.ID, jsonRPCMethodNotFound, "session DB MCP does not implement "+req.Method)
	}
}

// withSessionDBMCP puts the database endpoint in front of the K3s MCP proxy.
// The path is matched exactly and is not the K3s MCP's, so every other request
// still reaches the proxy and its deny filter unchanged.
func withSessionDBMCP(proxy http.Handler, db *sessionDB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == sessionDBMCPPath {
			db.serveMCP(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// errSessionDBMCPURL is a SESSION_DB_MCP_URL the workload agent will not
// register: the database is the sidecar's, so only a loopback address is one.
var errSessionDBMCPURL = errors.New(sessionDBMCPURLEnv + " must be an http URL on 127.0.0.1")

func sessionDBMCPURLFromEnv() (string, error) {
	raw := strings.TrimSpace(os.Getenv(sessionDBMCPURLEnv))
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "http://127.0.0.1:") {
		return "", errSessionDBMCPURL
	}
	return raw, nil
}
