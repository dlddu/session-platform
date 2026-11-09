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

func newTestSessionDB(t *testing.T) (*sessionDB, http.Handler) {
	t.Helper()
	db, err := newSessionDB(t.TempDir(), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, http.HandlerFunc(db.serveMCP)
}

// AC-N6: the two raw tools are the whole surface, and a write is readable
// back through the other one.
func TestSessionDBMCP_RawExecThenRawQuery(t *testing.T) {
	db, h := newTestSessionDB(t)

	init := sessionDBCall(t, h, "initialize", map[string]any{})
	if init.Error != nil || init.Result.(map[string]any)["serverInfo"].(map[string]any)["name"] != sessionDBMCPServerName {
		t.Fatalf("initialize = %+v", init)
	}
	list := sessionDBCall(t, h, "tools/list", map[string]any{})
	var names []string
	for _, tool := range list.Result.(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "raw_query,raw_exec" {
		t.Fatalf("tools = %v, want raw_query and raw_exec only", names)
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
	_, h := newTestSessionDB(t)
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
	_, h := newTestSessionDB(t)
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
	_, h := newTestSessionDB(t)
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
	db, err := newSessionDB(dir, testLogger())
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
	if _, err := newSessionDB("relative", testLogger()); err == nil {
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
