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
	// maxInlineBodyBytes is the line AC-F5 draws between a response small enough
	// to ride the tool result and one that goes to the shared volume as a file.
	// It keeps its old value: what changes is that exceeding it now spills
	// instead of truncating, wherever the volume is configured.
	maxInlineBodyBytes = 100_000
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
	// sharedDir is the session's shared volume as this container sees it, empty
	// where the deployment configured none. The control plane sets it only in
	// the branch that mounts the volume, so a value here also says the workload
	// pod holds the same filesystem at the same path (AC-F5).
	sharedDir string
}

func newSessionMCPConfig(gateway *approvalGateway, sharedDir string) sessionMCPConfig {
	return sessionMCPConfig{
		gateway:   gateway,
		fetch:     fetchURL,
		notices:   newNoticeFeed(),
		sharedDir: strings.TrimSpace(sharedDir),
	}
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
					"description": "The http or https URL to fetch.",
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
	// One byte past the inline limit is what tells "this fits" from "this is the
	// large result AC-F5 wants in a file" without reading a big body twice.
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxInlineBodyBytes+1))
	if err != nil {
		return mcpToolError("reading the response failed: " + err.Error()), nil
	}
	result := map[string]any{
		"success": true,
		"status":  resp.StatusCode,
		"headers": map[string]any{"content-type": resp.Header.Get("Content-Type")},
	}
	switch {
	case int64(len(head)) <= maxInlineBodyBytes:
		result["body"] = string(head)
	case c.sharedDir == "":
		// The deployment configured no ReadWriteMany class, so there is no
		// volume to hand a file across and this is the shape the tool had
		// before AC-F5: cut at the inline limit and say so.
		result["body"] = string(head[:maxInlineBodyBytes])
		result["truncated"] = true
	default:
		spill, spillErr := c.spillBody(requestID, head, resp.Body)
		if spillErr != nil {
			// An approved call is not repeatable on a whim — a human said yes to
			// this one — so a volume that could not take it falls back to the
			// pre-AC-F5 shape rather than discarding the result. The reason
			// travels with it so the agent is not left guessing why the body is
			// short.
			logger.Error("could not spill the response to the shared volume",
				"tool", webFetchGetTool, "err", spillErr)
			result["body"] = string(head[:maxInlineBodyBytes])
			result["truncated"] = true
			result["spillError"] = spillErr.Error()
			break
		}
		// No "body" key: the point of the volume is that the bytes do not make
		// this round trip (AC-F5).
		result["bodyPath"] = spill.path
		result["bodyBytes"] = spill.size
		result["truncated"] = spill.truncated
		logger.Info("stored the response on the shared volume",
			"tool", webFetchGetTool, "path", spill.path, "bytes", spill.size)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	return mcpToolText(string(payload)), nil
}

// validateFetchTarget keeps the tool to what a human can meaningfully approve.
// http(s) only: an approval that reads as a URL should not be satisfiable by a
// scheme the approver did not have in mind — file:// and friends would make the
// same approval text mean something else entirely. Plaintext http is allowed
// because in-cluster origins are a real target for this tool and many of them
// do not serve TLS; the scheme stays visible in the URL the approver reads.
func validateFetchTarget(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("url is not a valid URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("url must be http or https, got %q", parsed.Scheme)
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
