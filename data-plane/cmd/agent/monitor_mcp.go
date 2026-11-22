package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// The monitor-pair bridge: a stdio MCP server the agent itself runs inside the
// workload container, so the model gets real tools for the separated monitor.
//
// It is local on purpose. The pair token is a projected file in *this*
// container (the sidecars have none), and the shared K3s MCP runs elsewhere and
// cannot read it. So the bridge is the agent binary, started by the model's own
// MCP config the same way `skills-mcp` is, and it is confined to the
// monitor-pair routes: it builds every request itself and never takes a URL or
// a session id from the model.
//
// The token is read per call because the kubelet rotates the file in place, and
// it only ever goes into the Authorization header — never into a tool result,
// an error message or a log line.
const (
	monitorMCPCommand = "monitor-mcp"
	// monitorPairURLEnv and monitorPairTokenFileEnv are what the control plane
	// puts on the workload container (k8s.MonitorPairURLEnvVar,
	// k8s.MonitorPairTokenFileEnvVar). Keep the names in sync.
	monitorPairURLEnv       = "MONITOR_PAIR_URL"
	monitorPairTokenFileEnv = "MONITOR_PAIR_TOKEN_FILE"
	monitorMCPTimeout       = 30 * time.Second
)

type monitorMCPServer struct {
	base      string
	tokenFile string
	client    *http.Client
	logger    *slog.Logger
	// role is what the pair says this session is. The tool list follows it, so
	// a parent is not even offered the monitor's write tools.
	role string
}

func serveMonitorMCP(stdin io.Reader, stdout io.Writer, base, tokenFile string, logger *slog.Logger) error {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || strings.TrimSpace(tokenFile) == "" {
		return fmt.Errorf("%s and %s must be set", monitorPairURLEnv, monitorPairTokenFileEnv)
	}
	s := &monitorMCPServer{
		base: base, tokenFile: tokenFile,
		client: &http.Client{Timeout: monitorMCPTimeout},
		logger: logger,
	}
	s.role = s.resolveRole()
	in := bufio.NewReader(stdin)
	out := bufio.NewWriter(stdout)
	for {
		line, err := readCodexMCPLine(in)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		response, reply := s.handle(line)
		if !reply {
			continue
		}
		if err := writeCodexMCP(out, response); err != nil {
			return err
		}
	}
}

// resolveRole asks the pair once at startup. An unknown role still serves the
// read tools, so a session whose monitor has not bound yet gets a clear tool
// error rather than an empty tool list.
func (s *monitorMCPServer) resolveRole() string {
	body, _, err := s.do(http.MethodGet, "/api/v1/internal/monitor/pair", nil)
	if err != nil {
		s.logger.Warn("monitor pair role unknown at startup", "err", err)
		return ""
	}
	var view struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(body, &view) != nil {
		return ""
	}
	return view.Role
}

func (s *monitorMCPServer) handle(line []byte) (jsonRPCResponse, bool) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return jsonRPCResponse{JSONRPC: "2.0", Error: &jsonRPCError{
			Code: jsonRPCParseError, Message: "invalid JSON"}}, true
	}
	if len(req.ID) == 0 {
		return jsonRPCResponse{}, false
	}
	ok := func(result any) (jsonRPCResponse, bool) {
		return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	}
	fail := func(code int, message string) (jsonRPCResponse, bool) {
		return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: &jsonRPCError{
			Code: code, Message: message}}, true
	}
	switch req.Method {
	case "initialize":
		return ok(map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "monitor", "version": "1"},
		})
	case "tools/list":
		return ok(map[string]any{"tools": monitorPairToolDefinitions(s.role)})
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &call) != nil || call.Name == "" {
			return fail(jsonRPCInvalidParams, "tools/call needs a name and arguments")
		}
		text, isError := s.call(call.Name, call.Arguments)
		return ok(codexToolResult(text, isError))
	default:
		return fail(jsonRPCMethodNotFound, "unknown method "+req.Method)
	}
}

// monitorToolDefinitions is role-shaped. The parent gets reads and intake; the
// monitor gets its own bind, drain and publish. Neither list contains the
// other's writes, so the model is not offered a way around the separation.
func monitorPairToolDefinitions(role string) []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		} else {
			schema["properties"] = props
		}
		return schema
	}
	reads := []map[string]any{
		{
			"name":        "pair_status",
			"description": "이 세션이 속한 부모↔모니터 쌍과 모니터의 현재 상태(읽기·접수 가능 여부, 복구 방법)를 읽는다.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "work_tasks",
			"description": "모니터가 소유한 작업 목록을 읽는다. 각 작업의 최신 턴과 갱신 시각이 함께 오고, 모니터가 꺼져 있으면 모니터가 발행한 최신 저장 상태를 asOf 와 함께 돌려준다. 아무것도 바꾸지 않는다.",
			"inputSchema": obj(map[string]any{
				"status":   map[string]any{"type": "string", "description": "선택: 이 상태의 작업만"},
				"after_id": map[string]any{"type": "string", "description": "선택: 이 작업 id 다음 페이지"},
				"limit":    map[string]any{"type": "integer", "description": "선택: 한 페이지 크기"},
			}),
		},
		{
			"name":        "work_task_turns",
			"description": "작업 하나의 턴 이력(요청·응답·질문·선택지)을 읽는다. 모니터가 꺼져 있으면 발행된 최신 이력을 돌려준다.",
			"inputSchema": obj(map[string]any{
				"task_id":       map[string]any{"type": "string"},
				"after_turn_id": map[string]any{"type": "integer"},
				"limit":         map[string]any{"type": "integer"},
				"open":          map[string]any{"type": "boolean", "description": "선택: 끝나지 않은 턴만"},
			}, "task_id"),
		},
		{
			"name":        "request_status",
			"description": "보낸 지시 하나가 접수만 됐는지, 모니터의 턴으로 전달됐는지, 거절됐는지 읽는다. 실행하지 않는다.",
			"inputSchema": obj(map[string]any{"request_id": map[string]any{"type": "string"}}, "request_id"),
		},
	}
	switch role {
	case "parent":
		return append(reads, map[string]any{
			"name": "request_send",
			"description": "모니터에게 지시나 사용자의 결정을 보낸다. request_id 는 이 지시를 가리키는 안정된 키로, " +
				"같은 키·같은 본문을 다시 보내면 두 번 실행되지 않고 접수 영수증이 돌아온다. 모니터가 꺼져 있으면 " +
				"보관되고 모니터가 재개할 때 처리된다. 작업 상태를 직접 바꾸지는 않는다 — 기록은 모니터가 한다.",
			"inputSchema": obj(map[string]any{
				"request_id":     map[string]any{"type": "string", "description": "이 지시의 안정된 키. 재시도에도 같은 값을 쓴다."},
				"task_id":        map[string]any{"type": "string"},
				"text":           map[string]any{"type": "string"},
				"choice_turn_id": map[string]any{"type": "integer", "description": "선택: 그 턴이 내놓은 선택지를 고른 경우"},
			}, "request_id", "task_id", "text"),
		})
	case "monitor":
		return append(reads,
			map[string]any{
				"name":        "pair_bind",
				"description": "이 세션이 맡은 부모와의 쌍을 확정한다. 부모는 플랫폼이 생성 때 기록한 것이고 인자로 받지 않는다. 재시작 뒤 다시 불러도 같은 쌍이다.",
				"inputSchema": obj(map[string]any{}),
			},
			map[string]any{
				"name":        "inbox_drain",
				"description": "부모가 보낸 지시 중 아직 전달하지 않은 것을 읽는다. 읽는 것만으로는 claim 되지 않는다 — 전달한 뒤 inbox_mark_forwarded 로 기록한다.",
				"inputSchema": obj(map[string]any{}),
			},
			map[string]any{
				"name": "inbox_mark_forwarded",
				"description": "그 지시를 작업의 턴으로 만들었음을 기록한다. turn_id 는 슬롯이 돌려준 실제 턴이다. " +
					"같은 turn_id 재기록은 안전하고, 다른 turn_id 는 거절된다(두 번 실행했다는 뜻).",
				"inputSchema": obj(map[string]any{
					"request_id":  map[string]any{"type": "string"},
					"turn_id":     map[string]any{"type": "integer"},
					"session_seq": map[string]any{"type": "integer", "description": "선택: 그 턴의 세션 seq"},
				}, "request_id", "turn_id"),
			},
			map[string]any{
				"name":        "inbox_mark_refused",
				"description": "그 지시를 실행하지 않기로 했음을 이유와 함께 기록한다. 결과가 불명한 경우에는 쓰지 않는다 — 불명은 접수 상태로 남겨 다음 드레인이 대조한다.",
				"inputSchema": obj(map[string]any{
					"request_id": map[string]any{"type": "string"},
					"detail":     map[string]any{"type": "string"},
				}, "request_id", "detail"),
			},
			map[string]any{
				"name": "work_publish",
				"description": "작업을 등록·변경·완료한 뒤 그 커밋된 상태를 발행한다. 부모는 이 발행본으로 자기가 한 번도 조회하지 않은 작업까지 " +
					"모니터가 꺼진 동안 읽는다. kind 는 \"tasks\"(목록) 또는 \"turns\"(한 작업의 이력, key 는 작업 id).",
				"inputSchema": obj(map[string]any{
					"kind":    map[string]any{"type": "string", "enum": []string{"tasks", "turns"}},
					"key":     map[string]any{"type": "string", "description": "turns 일 때 작업 id"},
					"payload": map[string]any{"type": "object", "description": "work_tasks / work_task_turns 가 돌려주는 모양 그대로"},
				}, "kind", "payload"),
			},
		)
	default:
		return reads
	}
}

func (s *monitorMCPServer) call(name string, raw json.RawMessage) (string, bool) {
	var args map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "arguments must be a JSON object", true
		}
	}
	str := func(k string) string {
		v, _ := args[k].(string)
		return strings.TrimSpace(v)
	}
	num := func(k string) (int64, bool) {
		switch v := args[k].(type) {
		case float64:
			return int64(v), true
		case int64:
			return v, true
		}
		return 0, false
	}
	query := func(keys ...string) string {
		var parts []string
		for _, k := range keys {
			if v := str(k); v != "" {
				parts = append(parts, k+"="+v)
			}
			if n, ok := num(k); ok {
				parts = append(parts, fmt.Sprintf("%s=%d", k, n))
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "?" + strings.Join(parts, "&")
	}

	switch name {
	case "pair_status":
		return s.text(http.MethodGet, "/api/v1/internal/monitor/pair", nil)
	case "work_tasks":
		return s.text(http.MethodGet, "/api/v1/internal/monitor/work/tasks"+query("status", "after_id", "limit"), nil)
	case "work_task_turns":
		task := str("task_id")
		if task == "" {
			return "task_id is required", true
		}
		return s.text(http.MethodGet,
			"/api/v1/internal/monitor/work/tasks/"+task+"/turns"+query("after_turn_id", "limit", "open"), nil)
	case "request_status":
		id := str("request_id")
		if id == "" {
			return "request_id is required", true
		}
		return s.text(http.MethodGet, "/api/v1/internal/monitor/requests/"+id, nil)
	case "request_send":
		if s.role != "" && s.role != "parent" {
			return "only the parent session sends instructions", true
		}
		id, task, text := str("request_id"), str("task_id"), str("text")
		if id == "" || task == "" || text == "" {
			return "request_id, task_id and text are required", true
		}
		body := map[string]any{"id": id, "text": text}
		if n, ok := num("choice_turn_id"); ok {
			body["choice_turn_id"] = n
		}
		return s.text(http.MethodPost, "/api/v1/internal/monitor/work/tasks/"+task+"/requests", body)
	case "pair_bind":
		return s.text(http.MethodPost, "/api/v1/internal/monitor/pair", nil)
	case "inbox_drain":
		return s.text(http.MethodGet, "/api/v1/internal/monitor/inbox", nil)
	case "inbox_mark_forwarded":
		id := str("request_id")
		turn, ok := num("turn_id")
		if id == "" || !ok {
			return "request_id and turn_id are required", true
		}
		body := map[string]any{"turnId": turn}
		if seq, ok := num("session_seq"); ok {
			body["sessionSeq"] = seq
		}
		return s.text(http.MethodPost, "/api/v1/internal/monitor/inbox/"+id+"/forwarded", body)
	case "inbox_mark_refused":
		id, detail := str("request_id"), str("detail")
		if id == "" || detail == "" {
			return "request_id and detail are required", true
		}
		return s.text(http.MethodPost, "/api/v1/internal/monitor/inbox/"+id+"/refused",
			map[string]any{"detail": detail})
	case "work_publish":
		kind := str("kind")
		payload, ok := args["payload"]
		if kind == "" || !ok {
			return "kind and payload are required", true
		}
		return s.text(http.MethodPost, "/api/v1/internal/monitor/work/snapshot",
			map[string]any{"kind": kind, "key": str("key"), "payload": payload})
	default:
		return "unknown tool " + name, true
	}
}

func (s *monitorMCPServer) text(method, path string, body any) (string, bool) {
	raw, status, err := s.do(method, path, body)
	if err != nil {
		return err.Error(), true
	}
	if status >= 400 {
		return fmt.Sprintf("the control plane refused this call (%d): %s", status, strings.TrimSpace(string(raw))), true
	}
	return string(raw), false
}

// do builds the request itself: the model supplies arguments, never a URL or a
// session id, and the token goes only into the header.
func (s *monitorMCPServer) do(method, path string, body any) ([]byte, int, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), monitorMCPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, payload)
	if err != nil {
		return nil, 0, err
	}
	token, err := os.ReadFile(s.tokenFile)
	if err != nil {
		return nil, 0, fmt.Errorf("the monitor-pair token is not readable in this container")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("the control plane is not reachable from this session")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}
