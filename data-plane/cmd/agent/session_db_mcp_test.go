package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionDBCall posts one JSON-RPC request to the handler and returns the
// decoded response.
func sessionDBCall(t *testing.T, h http.Handler, method string, params any) jsonRPCResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, sessionDBMCPPath, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", method, rec.Code, rec.Body.String())
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return resp
}

// sessionDBTool calls a tool and returns its decoded text payload and isError.
func sessionDBTool(t *testing.T, h http.Handler, name, sql string, params ...any) (map[string]any, bool) {
	t.Helper()
	args := map[string]any{"sql": sql}
	if params != nil {
		args["params"] = params
	}
	resp := sessionDBCall(t, h, "tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		t.Fatalf("%s %q: JSON-RPC error %+v", name, sql, resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	var payload map[string]any
	if err := json.Unmarshal([]byte(content["text"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	return payload, result["isError"].(bool)
}

// newTestSessionDB builds the default sidecar: the ledger alone, raw SQL off.
func newTestSessionDB(t *testing.T) (*sessionDB, http.Handler) {
	t.Helper()
	return newTestSessionDBWithRawSQL(t, false)
}

// newTestSessionDBWithRawSQL builds the debug sidecar (SESSION_DB_RAW_SQL=debug).
func newTestSessionDBWithRawSQL(t *testing.T, rawSQL bool) (*sessionDB, http.Handler) {
	t.Helper()
	db, err := newSessionDB(t.TempDir(), rawSQL, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, http.HandlerFunc(db.serveMCP)
}

// workToolNames is the agreed task/turn surface, offered first: it is what the
// parent writes to, and the ledger behind it is what a migrated slot keeps for
// reading (docs/session-ledger.md).
// The bookkeeping pair comes last because it is not part of the agreed model:
// it reads and writes what a migration preserved beside a task, which is the
// parent's own and is not in anything a person reads.
const workToolNames = "task_get,task_list,task_put,task_delete," +
	"turn_append,turn_patch,turn_list,turn_dispatch," +
	"work_migrate,work_migration_report," +
	"work_legacy_get,work_legacy_put,work_rollback_export"

// ledgerToolNames is the whole default surface in offer order: the work tools,
// then the ledger's task, user-request and choice tools.
const ledgerToolNames = workToolNames + "," +
	"ledger_get,ledger_list,ledger_put,ledger_delete," +
	"ledger_requests_list,ledger_requests_claim,ledger_requests_dispatch,ledger_requests_ack," +
	"ledger_choices_put,ledger_choices_clear,ledger_events_list"

// sessionDBToolNames is what the model is offered by one sidecar.
func sessionDBToolNames(t *testing.T, h http.Handler) []string {
	t.Helper()
	list := sessionDBCall(t, h, "tools/list", map[string]any{})
	var names []string
	for _, tool := range list.Result.(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

// AC-N6: raw SQL is neither listed nor callable without the debug opt-in; the
// ledger is the whole surface a chat session gets.
func TestSessionDBMCP_RawSQLIsDebugOnly(t *testing.T) {
	_, h := newTestSessionDB(t)
	if names := sessionDBToolNames(t, h); strings.Join(names, ",") != ledgerToolNames {
		t.Fatalf("tools = %v, want the ledger alone", names)
	}
	for _, name := range []string{"raw_query", "raw_exec"} {
		resp := sessionDBCall(t, h, "tools/call", map[string]any{
			"name": name, "arguments": map[string]any{"sql": "SELECT 1"},
		})
		if resp.Error == nil || resp.Error.Code != jsonRPCInvalidParams || !strings.Contains(resp.Error.Message, sessionDBRawSQLEnv) {
			t.Fatalf("%s = %+v, want a refusal naming the opt-in", name, resp.Error)
		}
	}
	// A raw call carrying no arguments is refused for being raw, not for the
	// SQL it never carried.
	resp := sessionDBCall(t, h, "tools/call", map[string]any{"name": "raw_exec"})
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "raw_exec is off") {
		t.Fatalf("argument-less raw_exec = %+v", resp.Error)
	}
	if r := sessionDBCall(t, h, "tools/call", map[string]any{
		"name": "ledger_get", "arguments": map[string]any{"id": "absent"},
	}); r.Error != nil {
		t.Fatalf("ledger_get = %+v, want a tool result: the ledger does not need the opt-in", r.Error)
	}
}

// The opt-in is the deployment's exact value: unset is off, and a value that
// was meant as "on" is an error rather than a silent off.
func TestSessionDBRawSQLFromEnv(t *testing.T) {
	for raw, want := range map[string]bool{"": false, " debug ": true} {
		t.Setenv(sessionDBRawSQLEnv, raw)
		got, err := sessionDBRawSQLFromEnv()
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v, no error", raw, got, err, want)
		}
	}
	for _, raw := range []string{"true", "1", "yes", "DEBUG", "off"} {
		t.Setenv(sessionDBRawSQLEnv, raw)
		if _, err := sessionDBRawSQLFromEnv(); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
}

// AC-N6: with the debug opt-in the raw pair is back, ahead of the ledger.
func TestSessionDBMCP_RawExecThenRawQuery(t *testing.T) {
	db, h := newTestSessionDBWithRawSQL(t, true)

	init := sessionDBCall(t, h, "initialize", map[string]any{})
	if init.Error != nil || init.Result.(map[string]any)["serverInfo"].(map[string]any)["name"] != sessionDBMCPServerName {
		t.Fatalf("initialize = %+v", init)
	}
	if names := sessionDBToolNames(t, h); strings.Join(names, ",") != "raw_query,raw_exec,"+ledgerToolNames {
		t.Fatalf("tools = %v, want raw and ledger tools", names)
	}

	if out, isErr := sessionDBTool(t, h, "raw_exec",
		"CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT, score REAL, meta TEXT); "+
			"CREATE INDEX notes_body ON notes(body)"); isErr {
		t.Fatalf("create: %v", out)
	}
	out, isErr := sessionDBTool(t, h, "raw_exec", "INSERT INTO notes (body, score, meta) VALUES (?, ?, ?)",
		"hello", 1.5, map[string]any{"k": "v"})
	if isErr || out["rowsAffected"] != float64(1) || out["lastInsertId"] != float64(1) {
		t.Fatalf("insert = %v (isError %v)", out, isErr)
	}
	out, isErr = sessionDBTool(t, h, "raw_query", "SELECT id, body, score, json_extract(meta, '$.k') AS k FROM notes WHERE id = ?", 1)
	if isErr {
		t.Fatalf("select: %v", out)
	}
	if cols, _ := json.Marshal(out["columns"]); string(cols) != `["id","body","score","k"]` {
		t.Errorf("columns = %s", cols)
	}
	if rows, _ := json.Marshal(out["rows"]); string(rows) != `[[1,"hello",1.5,"v"]]` {
		t.Errorf("rows = %s", rows)
	}

	// The file is the slot's: it is where the next pod finds it.
	if _, err := os.Stat(db.path); err != nil {
		t.Fatalf("database file: %v", err)
	}
	if filepath.Base(db.path) != sessionDBFileName {
		t.Fatalf("database file = %s", db.path)
	}
}

// A statement SQLite refuses is a tool result the model can read, not a
// protocol failure; a malformed call is the protocol failure.
func TestSessionDBMCP_Errors(t *testing.T) {
	_, h := newTestSessionDBWithRawSQL(t, true)
	out, isErr := sessionDBTool(t, h, "raw_query", "SELECT * FROM missing")
	if !isErr || !strings.Contains(out["error"].(string), "missing") {
		t.Fatalf("missing table = %v (isError %v), want SQLite's message", out, isErr)
	}
	for name, params := range map[string]any{
		"empty sql":     map[string]any{"name": "raw_exec", "arguments": map[string]any{"sql": "  "}},
		"unknown field": map[string]any{"name": "raw_exec", "arguments": map[string]any{"sql": "SELECT 1", "x": 1}},
		"unknown tool":  map[string]any{"name": "drop_everything", "arguments": map[string]any{"sql": "SELECT 1"}},
	} {
		if resp := sessionDBCall(t, h, "tools/call", params); resp.Error == nil || resp.Error.Code != jsonRPCInvalidParams {
			t.Errorf("%s: response = %+v, want invalid params", name, resp)
		}
	}
}

func TestSessionDBMCP_QueryIsBounded(t *testing.T) {
	_, h := newTestSessionDBWithRawSQL(t, true)
	if out, isErr := sessionDBTool(t, h, "raw_exec",
		"CREATE TABLE n (v INTEGER); "+
			"WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 1500) INSERT INTO n SELECT x FROM c"); isErr {
		t.Fatalf("seed: %v", out)
	}
	out, isErr := sessionDBTool(t, h, "raw_query", "SELECT v FROM n")
	if isErr || len(out["rows"].([]any)) != sessionDBMaxRows || out["truncated"] != true {
		t.Fatalf("rows = %d, truncated = %v (isError %v)", len(out["rows"].([]any)), out["truncated"], isErr)
	}
}

func TestSessionDBMCP_BlobsAreJSONSafe(t *testing.T) {
	_, h := newTestSessionDBWithRawSQL(t, true)
	out, isErr := sessionDBTool(t, h, "raw_query", "SELECT x'ff00' AS b, CAST('text' AS BLOB) AS t")
	if isErr {
		t.Fatal(out)
	}
	if rows, _ := json.Marshal(out["rows"]); string(rows) != `[[{"base64":"/wA="},"text"]]` {
		t.Fatalf("rows = %s", rows)
	}
}

// A database directory that is missing when a call comes costs that call,
// and the next call opens it once it exists (the sidecar keeps serving the
// K3s MCP meanwhile).
func TestSessionDBMCP_OpensLazily(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "db")
	db, err := newSessionDB(dir, true, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := http.HandlerFunc(db.serveMCP)
	if _, isErr := sessionDBTool(t, h, "raw_query", "SELECT 1"); !isErr {
		t.Fatal("a missing directory must fail the call")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, isErr := sessionDBTool(t, h, "raw_query", "SELECT 1 AS one"); isErr {
		t.Fatalf("after mkdir: %v", out)
	}
	if _, err := newSessionDB("relative", false, testLogger()); err == nil {
		t.Fatal("a relative directory must be refused")
	}
}

// The database endpoint takes only its own path; every other request still
// reaches the K3s MCP proxy behind it.
func TestWithSessionDBMCP_Routes(t *testing.T) {
	db, _ := newTestSessionDB(t)
	var proxied []string
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	h := withSessionDBMCP(proxy, db)
	if resp := sessionDBCall(t, h, "ping", map[string]any{}); resp.Error != nil {
		t.Fatalf("ping = %+v", resp)
	}
	for _, path := range []string{"/mcp", "/healthz", sessionDBMCPPath + "/x"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s: HTTP %d, want the proxy", path, rec.Code)
		}
	}
	if len(proxied) != 3 {
		t.Fatalf("proxied = %v", proxied)
	}
}
