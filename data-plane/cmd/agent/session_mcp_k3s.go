// The read side of roadmap entry 2 ("K3s MCP 재노출",
// docs/session-mcp-tool-surface.md): the session MCP offers the K3s MCP
// server's read tools as its own tools, each behind the approval gate, and
// delegates exactly one upstream call per approved call.
//
// It is not a passthrough. Handing the agent a route to the K3s MCP would put
// that server within reach without the gate, which is what AC-F6's C2 forbids
// ("도구별 위임이 아니라 패스스루·터널은 게이트 없이 밖에 닿게 하므로 금지다").
// Every call here is validated before it becomes an approval request (R3),
// shows a person what it would do (R4), and carries the session's external
// identifier (R5).
//
// The token is not here. It lives in the helper pod's dedicated k3s-mcp-proxy
// container, which installs its own bearer; this container holds only the
// loopback address of that proxy (AC-F6 C1 — the value never reaches it — and
// C4 — the value never reaches a result, a log or an approval context).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// k3sResourceListTool and k3sResourceGetTool are the session-side names of
	// the upstream K3s MCP read tools. The upstream names (resource_list,
	// resource_get) are too generic to sit beside web_fetch_get and git_clone
	// in this surface, and the prefix names the target system for the person
	// who approves the call (R4). The upstream `ping` is not re-exported: this
	// server already has a `ping` of its own, and the document keeps the
	// protocol's ping and the tool's ping apart.
	k3sResourceListTool = "k3s_resource_list"
	k3sResourceGetTool  = "k3s_resource_get"

	// k3sMCPTimeout bounds one delegated upstream call.
	k3sMCPTimeout = 60 * time.Second
	// maxK3SMCPResponseBytes bounds what this container reads from the proxy.
	maxK3SMCPResponseBytes = 4 << 20
	// maxK3sLimit is the upstream's own ceiling for one list page.
	maxK3sLimit = 500
)

// k3sMCPClient calls the helper pod's K3s MCP proxy. It sends no Authorization
// header of its own: the proxy holds the token and installs its bearer.
type k3sMCPClient struct {
	endpoint string
	client   *http.Client
}

// newK3sMCPClient returns nil for an empty endpoint, which is how a
// deployment without the proxy stays without these tools.
func newK3sMCPClient(endpoint string) *k3sMCPClient {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil
	}
	return &k3sMCPClient{endpoint: endpoint, client: &http.Client{Timeout: k3sMCPTimeout}}
}

// call performs one upstream tools/call and returns the tool's text.
func (c *k3sMCPClient) call(ctx context.Context, upstreamTool string, arguments map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": upstreamTool, "arguments": arguments},
	})
	if err != nil {
		return "", fmt.Errorf("could not encode the upstream call")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", fmt.Errorf("could not build the upstream request")
	}
	req.Header.Set("Content-Type", "application/json")
	// The K3s MCP answers MCP Streamable HTTP, so it may answer with JSON or
	// with an event stream. Ask for both and read whichever comes.
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the upstream call failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxK3SMCPResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading the upstream answer failed")
	}
	if len(raw) > maxK3SMCPResponseBytes {
		return "", fmt.Errorf("the upstream answer is too large")
	}
	return k3sMCPResultText(resp.Header.Get("Content-Type"), raw)
}

// k3sMCPResultText pulls the tool result's text out of an upstream answer. The
// body is either one JSON-RPC message or an event stream whose data lines
// carry it; the proxy forwards whichever the K3s MCP sent.
func k3sMCPResultText(contentType string, raw []byte) (string, error) {
	var messages []string
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			messages = append(messages, payload)
		}
	} else if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
		messages = append(messages, trimmed)
	}
	for _, message := range messages {
		var envelope struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(message), &envelope); err != nil {
			continue
		}
		if envelope.Error != nil {
			return "", fmt.Errorf("the upstream refused the call: %s", envelope.Error.Message)
		}
		if len(envelope.Result) == 0 {
			continue
		}
		var result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			continue
		}
		var text strings.Builder
		for _, part := range result.Content {
			if part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
		if result.IsError {
			return "", fmt.Errorf("the upstream tool failed: %s", strings.TrimSpace(text.String()))
		}
		return text.String(), nil
	}
	return "", fmt.Errorf("the upstream answer carried no tool result")
}

// k3sToolCall is one delegated call: which upstream tool to call, with which
// arguments, and what the person approving it is shown.
type k3sToolCall struct {
	upstreamTool string
	arguments    map[string]any
	// approval is the context the person reads (R4). It names the target
	// system, the operation and the resource — never a credential (AC-F6 C4).
	approval map[string]string
}

// k3sArgumentKeys are the arguments each upstream tool accepts. Anything else
// is refused rather than passed on.
var (
	k3sListArgumentKeys = []string{"apiVersion", "kind", "namespace", "labelSelector", "fieldSelector", "limit", "continue"}
	k3sGetArgumentKeys  = []string{"apiVersion", "kind", "name", "namespace", "subresource", "container", "tailLines", "previous", "timestamps", "sinceSeconds"}
)

// k3sSubresources are the upstream subresources a read may carry.
var k3sSubresources = []string{"log", "scale", "status"}

// k3sResourceNamePattern bounds the names this tool accepts. A name that
// leaves the simple identifier space is refused before it becomes an approval
// request, because the approver is shown the name (R3, R4).
func validK3sName(name string) bool {
	if name == "" {
		return true
	}
	// ".." would let a name walk out of the name space it claims to be in.
	if strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}

// decodeK3sArguments reads the caller's arguments and refuses an unknown one.
func decodeK3sArguments(raw json.RawMessage, allowed []string) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, fmt.Errorf("arguments must be an object")
	}
	for key := range given {
		known := false
		for _, name := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown argument %q", key)
		}
	}
	return given, nil
}

func k3sString(given map[string]json.RawMessage, key string) (string, error) {
	raw, ok := given[key]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return strings.TrimSpace(value), nil
}

// k3sListCall builds the upstream call for one list request.
func k3sListCall(raw json.RawMessage) (*k3sToolCall, error) {
	given, err := decodeK3sArguments(raw, k3sListArgumentKeys)
	if err != nil {
		return nil, err
	}
	apiVersion, err := k3sString(given, "apiVersion")
	if err != nil {
		return nil, err
	}
	if apiVersion == "" {
		return nil, fmt.Errorf("apiVersion is required")
	}
	kind, err := k3sString(given, "kind")
	if err != nil {
		return nil, err
	}
	if kind == "" {
		return nil, fmt.Errorf("kind is required")
	}
	namespace, err := k3sString(given, "namespace")
	if err != nil {
		return nil, err
	}
	if !validK3sName(namespace) {
		return nil, fmt.Errorf("namespace has characters that are not allowed")
	}
	labelSelector, err := k3sString(given, "labelSelector")
	if err != nil {
		return nil, err
	}
	fieldSelector, err := k3sString(given, "fieldSelector")
	if err != nil {
		return nil, err
	}
	continueToken, err := k3sString(given, "continue")
	if err != nil {
		return nil, err
	}
	arguments := map[string]any{"apiVersion": apiVersion, "kind": kind}
	if namespace != "" {
		arguments["namespace"] = namespace
	}
	if labelSelector != "" {
		arguments["labelSelector"] = labelSelector
	}
	if fieldSelector != "" {
		arguments["fieldSelector"] = fieldSelector
	}
	if continueToken != "" {
		arguments["continue"] = continueToken
	}
	if rawLimit, ok := given["limit"]; ok {
		var limit int
		if err := json.Unmarshal(rawLimit, &limit); err != nil {
			return nil, fmt.Errorf("limit must be an integer")
		}
		if limit < 1 || limit > maxK3sLimit {
			return nil, fmt.Errorf("limit must be between 1 and %d", maxK3sLimit)
		}
		arguments["limit"] = limit
	}
	return &k3sToolCall{
		upstreamTool: "resource_list",
		arguments:    arguments,
		approval: map[string]string{
			"target":        "k3s-mcp",
			"operation":     "list",
			"apiVersion":    apiVersion,
			"kind":          kind,
			"namespace":     namespace,
			"labelSelector": labelSelector,
			"fieldSelector": fieldSelector,
		},
	}, nil
}

// k3sGetCall builds the upstream call for one read of a single resource.
func k3sGetCall(raw json.RawMessage) (*k3sToolCall, error) {
	given, err := decodeK3sArguments(raw, k3sGetArgumentKeys)
	if err != nil {
		return nil, err
	}
	apiVersion, err := k3sString(given, "apiVersion")
	if err != nil {
		return nil, err
	}
	if apiVersion == "" {
		return nil, fmt.Errorf("apiVersion is required")
	}
	kind, err := k3sString(given, "kind")
	if err != nil {
		return nil, err
	}
	if kind == "" {
		return nil, fmt.Errorf("kind is required")
	}
	name, err := k3sString(given, "name")
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if !validK3sName(name) {
		return nil, fmt.Errorf("name has characters that are not allowed")
	}
	namespace, err := k3sString(given, "namespace")
	if err != nil {
		return nil, err
	}
	if !validK3sName(namespace) {
		return nil, fmt.Errorf("namespace has characters that are not allowed")
	}
	subresource, err := k3sString(given, "subresource")
	if err != nil {
		return nil, err
	}
	if subresource != "" {
		known := false
		for _, candidate := range k3sSubresources {
			if subresource == candidate {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("subresource must be one of log, scale or status")
		}
	}
	container, err := k3sString(given, "container")
	if err != nil {
		return nil, err
	}
	arguments := map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name}
	if namespace != "" {
		arguments["namespace"] = namespace
	}
	if subresource != "" {
		arguments["subresource"] = subresource
	}
	if container != "" {
		arguments["container"] = container
	}
	for _, key := range []string{"tailLines", "sinceSeconds"} {
		rawValue, ok := given[key]
		if !ok {
			continue
		}
		var value int
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, fmt.Errorf("%s must be an integer", key)
		}
		if value < 0 {
			return nil, fmt.Errorf("%s must not be negative", key)
		}
		arguments[key] = value
	}
	for _, key := range []string{"previous", "timestamps"} {
		rawValue, ok := given[key]
		if !ok {
			continue
		}
		var value bool
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, fmt.Errorf("%s must be a boolean", key)
		}
		arguments[key] = value
	}
	return &k3sToolCall{
		upstreamTool: "resource_get",
		arguments:    arguments,
		approval: map[string]string{
			"target":      "k3s-mcp",
			"operation":   "get",
			"apiVersion":  apiVersion,
			"kind":        kind,
			"namespace":   namespace,
			"name":        name,
			"subresource": subresource,
		},
	}, nil
}

// callK3sTool runs one gated K3s MCP call: validate, ask a person, then
// delegate exactly one upstream call. A refusal, an expiry, a timeout and a
// gateway outage are all tool results, never protocol errors, and none of them
// kills the session (R7).
func (c sessionMCPConfig) callK3sTool(ctx context.Context, logger *slog.Logger, tool string, raw json.RawMessage) (any, *jsonRPCError) {
	if c.k3s == nil {
		return mcpToolError("this session has no K3s MCP proxy"), nil
	}
	var call *k3sToolCall
	var err error
	if tool == k3sResourceListTool {
		call, err = k3sListCall(raw)
	} else {
		call, err = k3sGetCall(raw)
	}
	if err != nil {
		// R3: the check happens before the approval request exists, so a
		// wrong target never reaches a person's screen.
		return mcpToolError(err.Error()), nil
	}

	requestID, err := newApprovalRequestID()
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not generate an approval request id"}
	}
	approvalContext, err := json.Marshal(call.approval)
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the approval context"}
	}

	externalID := c.gateway.externalID(requestID)
	logger.Info("awaiting approval", "tool", tool, "external_id", externalID)
	// The marker is published before the wait, not after: it only says
	// something while the session is still blocked on a human (R6).
	c.notices.publish(approvalNotice{Kind: noticeAwaiting, Tool: tool, ExternalID: externalID})
	outcome, err := c.gateway.await(ctx, requestID, string(approvalContext))
	if err != nil {
		logger.Error("approval could not be obtained", "tool", tool, "err", err)
		c.notices.publish(approvalNotice{Kind: noticeUnavailable, Tool: tool, ExternalID: externalID})
		return mcpToolError("approval could not be obtained: " + err.Error()), nil
	}
	c.notices.publish(approvalNotice{
		Kind: noticeDecided, Tool: tool, ExternalID: externalID,
		Decision: string(outcome.Decision),
	})
	if outcome.Decision != approvalApproved {
		logger.Info("approval refused", "tool", tool, "decision", string(outcome.Decision))
		return mcpToolError(fmt.Sprintf("%s request %s", tool, strings.ToLower(string(outcome.Decision)))), nil
	}

	logger.Info("approved, delegating to the K3s MCP", "tool", tool, "upstream_tool", call.upstreamTool)
	text, err := c.k3s.call(ctx, call.upstreamTool, call.arguments)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	return c.k3sToolResult(requestID, text), nil
}

// k3sToolResult carries the upstream text back. A text too large for the tool
// result goes to the shared volume as a file instead of riding inline, the
// same way web_fetch_get's body does (R11, AC-F5).
func (c sessionMCPConfig) k3sToolResult(requestID, text string) any {
	result := map[string]any{"success": true}
	switch {
	case len(text) <= maxInlineBodyBytes:
		result["body"] = text
	case c.sharedDir == "":
		// No volume to hand a file across, so the text is cut and the cut is
		// stated rather than hidden.
		result["body"] = text[:maxInlineBodyBytes]
		result["truncated"] = true
	default:
		spill, err := c.spillBody(requestID, []byte(text), strings.NewReader(""))
		if err != nil {
			result["body"] = text[:maxInlineBodyBytes]
			result["truncated"] = true
			result["spillError"] = err.Error()
			break
		}
		// No "body" key: the point of the volume is that the bytes do not make
		// this round trip.
		result["bodyPath"] = spill.path
		result["bodyBytes"] = spill.size
		result["truncated"] = spill.truncated
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return mcpToolError("could not encode the tool result")
	}
	return mcpToolText(string(payload))
}

// k3sResourceListProperties and k3sResourceGetProperties are the input schemas
// of the two tools. They live here rather than inside toolDefinitions() because
// the model's as-is extraction reads that method's body for tool names, and an
// argument called "name" would be read as one.
func k3sResourceListProperties() map[string]any {
	return map[string]any{
		"apiVersion": map[string]any{
			"type":        "string",
			"description": "The API version of the kind, for example v1.",
		},
		"kind": map[string]any{
			"type":        "string",
			"description": "The kind to list, for example Pod.",
		},
		"namespace": map[string]any{
			"type":        "string",
			"description": "The namespace to list in. Omit it for cluster-scoped kinds.",
		},
		"labelSelector": map[string]any{
			"type":        "string",
			"description": "A label selector, for example app=control-plane.",
		},
		"fieldSelector": map[string]any{
			"type":        "string",
			"description": "A field selector, for example status.phase=Running.",
		},
		"limit": map[string]any{
			"type":        "integer",
			"description": "How many items to return at most (1-500).",
		},
		"continue": map[string]any{
			"type":        "string",
			"description": "The continue token of an earlier page.",
		},
	}
}

func k3sResourceGetProperties() map[string]any {
	return map[string]any{
		"apiVersion": map[string]any{
			"type":        "string",
			"description": "The API version of the kind, for example v1.",
		},
		"kind": map[string]any{
			"type":        "string",
			"description": "The kind to read, for example Pod.",
		},
		"name": map[string]any{
			"type":        "string",
			"description": "The name of the resource.",
		},
		"namespace": map[string]any{
			"type":        "string",
			"description": "The namespace of the resource. Omit it for cluster-scoped kinds.",
		},
		"subresource": map[string]any{
			"type":        "string",
			"description": "The subresource to read: log, scale or status.",
		},
		"container": map[string]any{
			"type":        "string",
			"description": "The container to read a log from, when the pod has more than one.",
		},
		"tailLines": map[string]any{
			"type":        "integer",
			"description": "How many lines to read from the end of a log.",
		},
		"previous": map[string]any{
			"type":        "boolean",
			"description": "Read the previous terminated container instance instead.",
		},
		"timestamps": map[string]any{
			"type":        "boolean",
			"description": "Prefix each log line with its timestamp.",
		},
		"sinceSeconds": map[string]any{
			"type":        "integer",
			"description": "Read only log lines newer than this many seconds.",
		},
	}
}
