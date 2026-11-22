package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// bridgeControlPlane is the monitor-pair API as the bridge sees it: it records
// what the client actually sent, including how many times, which is how the
// test shows a retry does not delegate twice.
type bridgeControlPlane struct {
	mu        sync.Mutex
	role      string
	calls     []string
	auth      []string
	bodies    []string
	sendCount int
}

func (f *bridgeControlPlane) handler() http.Handler {
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		raw, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, string(raw))
	}
	mux.HandleFunc("GET /api/v1/internal/monitor/pair", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"role": f.role,
			"monitor": map[string]any{"state": "active", "reads": "available", "intake": "available"}})
	})
	mux.HandleFunc("GET /api/v1/internal/monitor/work/tasks", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"source": "slot", "tasks": []any{map[string]any{"task": map[string]any{"id": "task-1"}}}})
	})
	mux.HandleFunc("GET /api/v1/internal/monitor/work/tasks/{taskID}/turns", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"source": "slot", "task_id": r.PathValue("taskID"),
			"turns": []any{map[string]any{"id": 1, "response": "done"}}})
	})
	mux.HandleFunc("POST /api/v1/internal/monitor/work/tasks/{taskID}/requests", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		f.mu.Lock()
		f.sendCount++
		n := f.sendCount
		f.mu.Unlock()
		// The second call with the same key is a replay, as the server does.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"item":     map[string]any{"requestId": "req-1", "state": "forwarded", "turnId": 9},
			"replayed": n > 1,
		})
	})
	mux.HandleFunc("GET /api/v1/internal/monitor/requests/{requestID}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"requestId": r.PathValue("requestID"), "state": "forwarded"}})
	})
	mux.HandleFunc("GET /api/v1/internal/monitor/inbox", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{
			map[string]any{"requestId": "req-held", "taskId": "task-1", "text": "해 줘", "state": "accepted"}}})
	})
	mux.HandleFunc("POST /api/v1/internal/monitor/inbox/{requestID}/forwarded", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"requestId": r.PathValue("requestID"), "state": "forwarded", "turnId": 12}})
	})
	mux.HandleFunc("POST /api/v1/internal/monitor/work/snapshot", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"published": "tasks"})
	})
	mux.HandleFunc("POST /api/v1/internal/monitor/pair", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"role": "monitor",
			"binding": map[string]any{"parentSessionId": "p1", "monitorSessionId": "m1"}})
	})
	return mux
}

// bridge runs the real stdio MCP server against the fake control plane and
// returns a function that sends one JSON-RPC request and reads the reply, the
// way codex drives it.
func bridge(t *testing.T, role string) (*bridgeControlPlane, func(method string, params any) map[string]any) {
	t.Helper()
	cp := &bridgeControlPlane{role: role}
	srv := httptest.NewServer(cp.handler())
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected-token-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveMonitorMCP(inR, outW, srv.URL, tokenFile, slog.New(slog.NewTextHandler(io.Discard, nil)))
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		<-done
	})
	reader := bufio.NewReader(outR)
	id := 0
	return cp, func(method string, params any) map[string]any {
		t.Helper()
		id++
		req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			req["params"] = params
		}
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inW.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("no reply to %s: %v", method, err)
		}
		var resp map[string]any
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("bad reply to %s: %s", method, line)
		}
		return resp
	}
}

func bridgeToolNames(t *testing.T, resp map[string]any) []string {
	t.Helper()
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	var names []string
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

func bridgeToolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, _ := resp["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no tool content: %+v", resp)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// The parent's tool list is reads plus intake. It does not contain the
// monitor's bind, drain or publish, so the model is not offered a way around
// the separation — and the slot's own task_put/turn_dispatch are a different
// server the parent is not given.
func TestBridgeOffersTheParentReadsAndIntakeOnly(t *testing.T) {
	cp, call := bridge(t, "parent")
	names := bridgeToolNames(t, call("tools/list", nil))
	want := map[string]bool{"pair_status": true, "work_tasks": true, "work_task_turns": true,
		"request_status": true, "request_send": true}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("parent was offered %q; tool list = %v", n, names)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("parent is missing %v; got %v", want, names)
	}
	// The bridge asked the pair for its role with the projected token.
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.auth) == 0 || cp.auth[0] != "Bearer projected-token-value" {
		t.Fatalf("role lookup auth = %v", cp.auth)
	}
}

// The monitor's list carries its own writes, and not the parent's intake tool.
func TestBridgeOffersTheMonitorItsOwnWrites(t *testing.T) {
	_, call := bridge(t, "monitor")
	names := strings.Join(bridgeToolNames(t, call("tools/list", nil)), ",")
	for _, want := range []string{"pair_bind", "inbox_drain", "inbox_mark_forwarded", "inbox_mark_refused", "work_publish"} {
		if !strings.Contains(names, want) {
			t.Fatalf("monitor is missing %q; got %s", want, names)
		}
	}
	if strings.Contains(names, "request_send") {
		t.Fatalf("the monitor must not be offered the parent's intake tool: %s", names)
	}
}

// A real client call: the parent reads, then sends an instruction with a
// stable key, and a retry with the same key reaches the server again as the
// same request — the server replies replayed, and the worker is not delegated
// twice. The token never appears in a tool result.
func TestBridgeParentReadThenSendIsIdempotent(t *testing.T) {
	cp, call := bridge(t, "parent")

	tasks := bridgeToolText(t, call("tools/call", map[string]any{"name": "work_tasks", "arguments": map[string]any{}}))
	if !strings.Contains(tasks, "task-1") || !strings.Contains(tasks, `"source":"slot"`) {
		t.Fatalf("work_tasks text = %s", tasks)
	}
	turns := bridgeToolText(t, call("tools/call", map[string]any{
		"name": "work_task_turns", "arguments": map[string]any{"task_id": "task-1"}}))
	if !strings.Contains(turns, `"response":"done"`) {
		t.Fatalf("work_task_turns text = %s", turns)
	}

	args := map[string]any{"request_id": "req-1", "task_id": "task-1", "text": "코멘트 반영해 줘"}
	first := bridgeToolText(t, call("tools/call", map[string]any{"name": "request_send", "arguments": args}))
	if !strings.Contains(first, `"replayed":false`) {
		t.Fatalf("first send = %s", first)
	}
	second := bridgeToolText(t, call("tools/call", map[string]any{"name": "request_send", "arguments": args}))
	if !strings.Contains(second, `"replayed":true`) {
		t.Fatalf("retry must come back replayed: %s", second)
	}

	cp.mu.Lock()
	defer cp.mu.Unlock()
	// Two client calls, both carrying the same stable key: the server is what
	// collapses them, and the bridge did not invent a second key.
	if cp.sendCount != 2 {
		t.Fatalf("send count = %d", cp.sendCount)
	}
	var sends int
	for i, c := range cp.calls {
		if !strings.HasPrefix(c, "POST /api/v1/internal/monitor/work/tasks/task-1/requests") {
			continue
		}
		sends++
		if !strings.Contains(cp.bodies[i], `"id":"req-1"`) {
			t.Fatalf("send body lost the stable key: %s", cp.bodies[i])
		}
	}
	if sends != 2 {
		t.Fatalf("sends = %d", sends)
	}
	for _, text := range []string{first, second, tasks, turns} {
		if strings.Contains(text, "projected-token-value") {
			t.Fatal("the token leaked into a tool result")
		}
	}
}

// The monitor's own path: bind, drain what the parent sent, then record the
// turn the slot made for it. The bridge sends the turn id the monitor gives,
// not one it invents.
func TestBridgeMonitorBindsDrainsAndRecordsTheTurn(t *testing.T) {
	cp, call := bridge(t, "monitor")
	if text := bridgeToolText(t, call("tools/call", map[string]any{"name": "pair_bind", "arguments": map[string]any{}})); !strings.Contains(text, "monitorSessionId") {
		t.Fatalf("pair_bind = %s", text)
	}
	drained := bridgeToolText(t, call("tools/call", map[string]any{"name": "inbox_drain", "arguments": map[string]any{}}))
	if !strings.Contains(drained, "req-held") || !strings.Contains(drained, `"state":"accepted"`) {
		t.Fatalf("inbox_drain = %s", drained)
	}
	marked := bridgeToolText(t, call("tools/call", map[string]any{
		"name": "inbox_mark_forwarded", "arguments": map[string]any{"request_id": "req-held", "turn_id": 12, "session_seq": 4}}))
	if !strings.Contains(marked, `"turnId":12`) {
		t.Fatalf("inbox_mark_forwarded = %s", marked)
	}
	published := bridgeToolText(t, call("tools/call", map[string]any{
		"name": "work_publish", "arguments": map[string]any{"kind": "tasks", "payload": map[string]any{"tasks": []any{}}}}))
	if !strings.Contains(published, "published") {
		t.Fatalf("work_publish = %s", published)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var body string
	for i, c := range cp.calls {
		if strings.Contains(c, "/forwarded") {
			body = cp.bodies[i]
		}
	}
	if !strings.Contains(body, `"turnId":12`) || !strings.Contains(body, `"sessionSeq":4`) {
		t.Fatalf("forwarded body = %s", body)
	}
}

// The parent cannot reach the monitor's writes even by naming the tool: the
// bridge refuses on role before it builds a request.
func TestBridgeRefusesCrossRoleToolCalls(t *testing.T) {
	cp, call := bridge(t, "monitor")
	text := bridgeToolText(t, call("tools/call", map[string]any{
		"name": "request_send", "arguments": map[string]any{"request_id": "x", "task_id": "t", "text": "y"}}))
	if !strings.Contains(text, "only the parent") {
		t.Fatalf("cross-role call = %s", text)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for _, c := range cp.calls {
		if strings.Contains(c, "/requests") {
			t.Fatal("a refused cross-role call still reached the control plane")
		}
	}
}
