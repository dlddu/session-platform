// The session MCP's tool surface (AC-F3): one tool, `web_fetch_get`, and the
// approval gate it has to pass through.
//
// The tool's name, arguments and response shape are the reference
// implementation's (dlddu/pure-agent, mcp-server/src/tools/web-fetch-get.ts),
// which is also what docs/mockups/gated-workspace.html draws.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// webFetchGetTool is the one external tool an approval-gated session has.
	// Adding a second one means adding a second gated handler, never a bypass.
	webFetchGetTool = "web_fetch_get"
	// maxFetchedBodyBytes bounds what a fetch may hand back inline, standing in
	// for AC-F5's shared volume until it lands (docs/doc-tracker.md).
	maxFetchedBodyBytes = 100_000
	// fetchTimeout bounds the approved call itself. The wait before it is
	// bounded separately by the gateway's own timeout.
	fetchTimeout = 60 * time.Second
	// requestIDPrefix and requestIDBytes shape the request half of AC-F3's
	// external identifier. It is generated here rather than taken from the
	// JSON-RPC id, which restarts at 1 with every new agent process and would
	// collide across two invocations of the same session.
	requestIDPrefix = "req-"
	requestIDBytes  = 4
)

// sessionMCPConfig is what the MCP container was started with.
type sessionMCPConfig struct {
	gateway *approvalGateway
	// fetch performs the approved outbound call.
	fetch func(ctx context.Context, target string) (*http.Response, error)
	// notices carries the wait and its decision to whoever is tailing, which in
	// a real session is the workload pod's agent (session_mcp_notice_tail.go).
	notices *noticeFeed
}

func newSessionMCPConfig(gateway *approvalGateway) sessionMCPConfig {
	return sessionMCPConfig{gateway: gateway, fetch: fetchURL, notices: newNoticeFeed()}
}

// gated reports whether this container can offer external tools at all.
func (c sessionMCPConfig) gated() bool { return c.gateway != nil }

// toolDefinitions is what tools/list answers. It is empty without a gate, for
// the reason this file's header gives.
func (c sessionMCPConfig) toolDefinitions() []any {
	if !c.gated() {
		return []any{}
	}
	return []any{map[string]any{
		"name": webFetchGetTool,
		"description": "Fetch a URL with GET. Every call needs human approval before " +
			"it leaves the session, and the response is returned only if the request was approved.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The https URL to fetch.",
				},
			},
			"required":             []any{"url"},
			"additionalProperties": false,
		},
	}}
}

// callTool runs one tools/call. The returned value is normally an MCP tool
// result (mcpToolText/mcpToolError). The JSON-RPC error return is reserved for
// faults that are not tool outcomes at all: a call this server cannot parse,
// and a failure inside the server itself.
func (c sessionMCPConfig) callTool(ctx context.Context, logger *slog.Logger, params json.RawMessage) (any, *jsonRPCError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: "tools/call params are not an object"}
		}
	}
	if call.Name != webFetchGetTool {
		return nil, &jsonRPCError{Code: jsonRPCInvalidParams, Message: fmt.Sprintf("no tool named %q", call.Name)}
	}
	if !c.gated() {
		// Reachable only if a client calls a tool this server never listed.
		return mcpToolError("the approval gate is not configured, so no external call can be made"), nil
	}

	var args struct {
		URL string `json:"url"`
	}
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return mcpToolError("arguments must be an object with a url"), nil
		}
	}
	target, err := validateFetchTarget(args.URL)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}

	requestID, err := newApprovalRequestID()
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not generate an approval request id"}
	}
	approvalContext, err := json.Marshal(map[string]string{"url": target, "method": http.MethodGet})
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the approval context"}
	}

	externalID := c.gateway.externalID(requestID)
	logger.Info("awaiting approval", "tool", webFetchGetTool, "external_id", externalID)
	// Announce the wait before entering it, not after: the marker's purpose is
	// to tell a watching client that this session is blocked on a human, which
	// is only useful while the block is still happening (AC-F3).
	c.notices.publish(approvalNotice{Kind: noticeAwaiting, Tool: webFetchGetTool, ExternalID: externalID})
	outcome, err := c.gateway.await(ctx, requestID, string(approvalContext))
	if err != nil {
		// The gateway could not be asked — see approvalGateway.do for why that is
		// not a refusal. It still reaches the agent as a tool failure (AC-F3).
		logger.Error("approval could not be obtained", "tool", webFetchGetTool, "err", err)
		c.notices.publish(approvalNotice{Kind: noticeUnavailable, Tool: webFetchGetTool, ExternalID: externalID})
		return mcpToolError("approval could not be obtained: " + err.Error()), nil
	}
	c.notices.publish(approvalNotice{
		Kind: noticeDecided, Tool: webFetchGetTool, ExternalID: externalID,
		Decision: string(outcome.Decision),
	})
	if outcome.Decision != approvalApproved {
		logger.Info("approval refused", "tool", webFetchGetTool, "decision", string(outcome.Decision))
		return mcpToolError(fmt.Sprintf("web fetch request %s", strings.ToLower(string(outcome.Decision)))), nil
	}

	logger.Info("approved, performing the outbound call", "tool", webFetchGetTool)
	resp, err := c.fetch(ctx, target)
	if err != nil {
		return mcpToolError("fetch failed: " + err.Error()), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchedBodyBytes))
	if err != nil {
		return mcpToolError("reading the response failed: " + err.Error()), nil
	}
	payload, err := json.Marshal(map[string]any{
		"success": true,
		"status":  resp.StatusCode,
		"headers": map[string]any{"content-type": resp.Header.Get("Content-Type")},
		"body":    string(body),
	})
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	return mcpToolText(string(payload)), nil
}

// validateFetchTarget keeps the tool to what a human can meaningfully approve.
// https only: an approval that reads as a URL should not be satisfiable by a
// plaintext or non-HTTP scheme the approver did not have in mind.
func validateFetchTarget(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("url is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("url must be https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("url has no host")
	}
	return parsed.String(), nil
}

// newApprovalRequestID mints the request half of the external identifier.
func newApprovalRequestID() (string, error) {
	buf := make([]byte, requestIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return requestIDPrefix + hex.EncodeToString(buf), nil
}

func fetchURL(ctx context.Context, target string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose ties the fetch's timeout context to the body's lifetime, so the
// context is released when the caller is done reading rather than at return.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// mcpToolText and mcpToolError are the two shapes of an MCP tool result. A
// refusal is `isError` on a successful JSON-RPC response, which is what lets the
// agent read it as "the tool said no" instead of "the call broke".
func mcpToolText(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

func mcpToolError(text string) map[string]any {
	result := mcpToolText(text)
	result["isError"] = true
	return result
}
