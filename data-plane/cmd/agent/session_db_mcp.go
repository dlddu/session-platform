// Session DB MCP (docs/prd/agent-auth-pool.md AC-N6): a codex-manager session's
// SQLite database, served by the loopback k3s-mcp-proxy sidecar from the
// database directory of the session's slot PVC. The workload container never
// mounts that directory; it reaches the database only through this endpoint,
// which the managed config.toml registers as the `db` MCP server.
//
// The versioned work records are the whole surface a chat session gets. Raw SQL
// is a debug tool: it is neither listed nor callable unless the deployment
// opts the sidecar in with SESSION_DB_RAW_SQL=debug.
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
	sessionDBRawSQLEnv = "SESSION_DB_RAW_SQL"
	sessionDBMCPPath   = "/session-db/mcp"
	// sessionDBLanePathPrefix is where a manager's two lanes reach the same
	// database as themselves: <prefix>/<role>/<incarnation> (AC-N11). The role
	// is in the address rather than in an argument because the address is in the
	// lane's own managed config.toml, which the platform rewrites on every boot
	// and the model can neither read nor change — and because the only way the
	// model reaches this sidecar at all is a server that config registered.
	// So a parent lane cannot call as the monitor: it has no address to call.
	sessionDBLanePathPrefix = sessionDBMCPPath + "/lane"

	// sessionDBRawSQLDebug is the only value that turns raw SQL on. A spelling
	// the operator meant as "on" (true, 1, yes) is refused rather than read as
	// off, so a debug session never starts without the tools it was given.
	sessionDBRawSQLDebug = "debug"

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
	path string
	// rawSQL is the debug opt-in: false leaves raw_query and raw_exec out of
	// tools/list and refuses them in tools/call. The record tools do not read
	// it — their statements are this type's methods, not model-supplied SQL.
	rawSQL bool
	logger *slog.Logger
	// monitorLane is the lane this sidecar hands the parent's requests to
	// (session_work_lane.go). Nil for a pod that does not run the pair, and
	// nil is not a degraded mode — it is a sidecar with no second lane to hand
	// anything to, which is what every unpaired manager is.
	monitorLane *monitorLaneClient

	mu sync.Mutex
	db *sql.DB
}

func newSessionDB(dir string, rawSQL bool, logger *slog.Logger) (*sessionDB, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("%s must be an absolute path", sessionDBDirEnv)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionDB{
		path:   filepath.Join(filepath.Clean(dir), sessionDBFileName),
		rawSQL: rawSQL,
		logger: logger,
	}, nil
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

// sessionDBToolDefinitions lists the work records, and the raw SQL pair only
// for a sidecar opted into debug: a tool the model cannot see is one it cannot
// be talked into using.
//
// The same principle decides what a manager lane is shown. A parent lane reads
// the work records and does not write them (AC-N11), so the writes are not in
// its list at all — being told "that is not yours" on every attempt would be a
// worse way to learn a rule than never being offered it. The guard behind the
// list stays, for a client holding an older one.
func sessionDBToolDefinitions(rawSQL bool, caller workCaller) []map[string]any {
	if !rawSQL {
		return laneToolDefinitions(caller.lane)
	}
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
			"description": fmt.Sprintf("Debug tool (this sidecar is opted in with %s=%s). Run one SQL statement on this session's SQLite database "+
				"and return its columns and rows (at most %d rows): SELECT and other statements that return rows.",
				sessionDBRawSQLEnv, sessionDBRawSQLDebug, sessionDBMaxRows),
			"inputSchema": statement("The SQLite statement to run."),
		},
		{
			"name": "raw_exec",
			"description": "Debug tool. Run SQL that changes this session's SQLite database (CREATE, INSERT, UPDATE, DELETE, ...) and " +
				"return the rows affected and the last insert rowid. These are SQLite connection counters, NOT committed change totals (DDL/ROLLBACK can retain old values). Without params, several statements separated by ; run in order. Open transactions are rolled back at call end, so a transaction begins and commits within one call.",
			"inputSchema": statement("The SQLite statement or statements to run."),
		},
	}, laneToolDefinitions(caller.lane)...)
}

// laneToolDefinitions is the work surface one lane is offered: everything for a
// manager that runs a single lane or for the monitor that owns the records, and
// the reads plus the relay for the parent that does not.
func laneToolDefinitions(lane string) []map[string]any {
	all := workToolDefinitions(lane)
	if lane != laneParent {
		return all
	}
	offered := make([]map[string]any, 0, len(all))
	for _, definition := range all {
		if name, _ := definition["name"].(string); isWorkLedgerWrite(name) {
			continue
		}
		offered = append(offered, definition)
	}
	return offered
}

// toolDefinitions is what this sidecar's tools/list answers.
//
// It used to depend on the database: while a slot could still be mid-move, the
// transition's own tools were listed only where there was something left to
// move, so the model was not shown a call whose only possible answer was a
// refusal. Those tools are gone (session_work_state.go), and with them the one
// reason this list differed between slots — so it is now the same surface
// everywhere, read from no state at all. A sidecar whose volume is not ready
// yet therefore answers the same list as a healthy one instead of a shorter one
// that looks like a deployment without these tools.
//
// The refusals stay in place for a client holding an older list: it is told what
// to call instead (session_work.go), because a list is a hint and a guard is a
// guarantee.
func (s *sessionDB) toolDefinitions(_ context.Context, caller workCaller) []map[string]any {
	return sessionDBToolDefinitions(s.rawSQL, caller)
}

// callTool runs one tool. A statement that fails is a tool result with
// isError, so the model sees SQLite's message; only a malformed call is a
// JSON-RPC error.
func (s *sessionDB) callTool(ctx context.Context, caller workCaller, params json.RawMessage) (any, *jsonRPCError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "tools/call needs a name and arguments"}
	}
	// The lane is checked before the arguments, like the raw SQL opt-in below: a
	// parent lane writing the records has to learn which lane owns them, not
	// that the record it sent was malformed. This is the guard behind the list —
	// it answers a client holding a list from before the pair, and it is what
	// makes the role an enforced boundary rather than a filtered menu.
	if caller.lane == laneParent && isWorkLedgerWrite(call.Name) {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: call.Name +
			" is the monitor lane's to call: this manager's parent lane reads the work records and does not write them. " +
			"Hand the request to the monitor with " + laneAskTool + " and read what came of it with " + laneAnswerTool}
	}
	// The handover's own refusals are specific for the same reason — a caller
	// that cannot use these tools has to learn which lane they belong to, not
	// that its arguments were wrong (session_work_lane.go).
	if isLaneHandoverTool(call.Name) {
		return s.callLaneHandover(ctx, caller, call.Name, call.Arguments)
	}
	if isWorkTool(call.Name) {
		return s.callWork(ctx, caller, call.Name, call.Arguments)
	}
	// A name this image removed is answered with the call that does the job
	// instead. The ledger tools, the migration, the rollback export and now the
	// retirement with its snapshot tools are gone (AC-N10), and a parent that
	// still holds an older tools/list learns the successor rather than "unknown
	// tool" (session_work.go).
	if successor := workRemovedToolSuccessor(call.Name); successor != "" {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: call.Name +
			" is not a tool on this slot any more; use " + successor}
	}
	// The name is checked before the arguments: a raw call on a sidecar that is
	// not opted in has to say so, not fail on the SQL it was never going to run.
	if call.Name != "raw_query" && call.Name != "raw_exec" {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "unknown tool " + call.Name}
	}
	if !s.rawSQL {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: call.Name +
			" is off: this session database offers the work record tools, and raw SQL only where the sidecar has " +
			sessionDBRawSQLEnv + "=" + sessionDBRawSQLDebug}
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
	if call.Name == "raw_query" {
		result, err = s.query(ctx, stmt)
	} else {
		result, err = s.exec(ctx, stmt)
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
// the session MCP (session_mcp.go). caller is which lane of the manager this
// address belongs to, decided by the path and not by anything in the request.
func (s *sessionDB) serveMCP(w http.ResponseWriter, r *http.Request, caller workCaller) {
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
		writeJSONRPCResult(w, req.ID, map[string]any{"tools": s.toolDefinitions(r.Context(), caller)})
	case "tools/call":
		result, rpcErr := s.callTool(r.Context(), caller, req.Params)
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

// withSessionDBMCP puts the database endpoints in front of the K3s MCP proxy:
// the session's own `db` MCP, and the work record views the control plane reads
// (session_work_http.go). Every path is matched exactly and none is the K3s
// MCP's, so every other request still reaches the proxy and its deny filter
// unchanged.
func withSessionDBMCP(proxy http.Handler, db *sessionDB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if caller, ok, bad := sessionDBLaneCaller(r.URL.Path); ok {
			if bad != "" {
				http.Error(w, bad, http.StatusNotFound)
				return
			}
			db.serveMCP(w, r, caller)
			return
		}
		switch r.URL.Path {
		case sessionDBMCPPath:
			// The address a manager with one lane registers. It keeps the whole
			// surface: there is no other lane for the records to belong to, so
			// narrowing it would take writes away from the only caller there is.
			db.serveMCP(w, r, workCaller{})
		case sessionDBWorkRequestsPath:
			db.serveWorkRequests(w, r)
		case sessionDBWorkTasksPath:
			db.serveWorkTasks(w, r)
		case sessionDBWorkTurnsPath:
			db.serveWorkTurns(w, r)
		default:
			proxy.ServeHTTP(w, r)
		}
	})
}

// sessionDBLaneCaller reads a lane address under this prefix
// (laneCallerFromPath, manager_lane.go), so a path that is not one of this
// prefix's is left to the routes beside it and a malformed one under it is
// refused by name rather than falling through to the K3s MCP proxy.
//
// The incarnation has the id shape every record here uses, and it is not a
// secret: what it proves is only that a caller presenting a different one is a
// different run of that lane, which is what makes a claim left behind by a lane
// that is gone recoverable exactly once.
func sessionDBLaneCaller(path string) (caller workCaller, underPrefix bool, refusal string) {
	return laneCallerFromPath(sessionDBLanePathPrefix, path)
}

// sessionDBLaneMCPPath is the address one lane registers.
func sessionDBLaneMCPPath(lane, incarnation string) string {
	return laneMCPPath(sessionDBLanePathPrefix, lane, incarnation)
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

// sessionDBRawSQLFromEnv reads the debug opt-in. Unset is off; any value other
// than the one that means debug is an error, because a typo that silently
// served the record tools alone would look like raw SQL had been removed.
func sessionDBRawSQLFromEnv() (bool, error) {
	switch raw := strings.TrimSpace(os.Getenv(sessionDBRawSQLEnv)); raw {
	case "":
		return false, nil
	case sessionDBRawSQLDebug:
		return true, nil
	default:
		return false, fmt.Errorf("%s must be unset or %q, not %q", sessionDBRawSQLEnv, sessionDBRawSQLDebug, raw)
	}
}
