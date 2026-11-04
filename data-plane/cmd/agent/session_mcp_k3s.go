// K3s MCP re-export (docs/session-mcp-tool-surface.md, 등재 2번): the session
// MCP hands a small, read-only slice of the K3s MCP server's tools to the
// agent, one approval gate per tool.
//
// It is a delegation, not a corridor. The session MCP calls the K3s MCP proxy
// container beside it — the same proxy the repo access token mint uses
// (AC-H5) — so the K3s MCP token stays in that container (AC-F6 C1) and the
// workload pod has no route to the server (AC-F2). Every call waits for a
// person first (AC-F3), and the approval context says what is asked in a form
// a person can read (R4).
//
// This is the read half of the roadmap item: logs, resource lookups and
// listings. The write half — restarts, scales, document indexing — has a
// different approval context shape and is not here; the roadmap item's own
// scope note keeps the two apart.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// The tools this slice re-exports.
	k3sAPIResourcesTool = "k3s_api_resources"
	k3sResourceListTool = "k3s_resource_list"
	k3sResourceGetTool  = "k3s_resource_get"

	// The K3s MCP tools behind them. The session MCP names one of these only
	// after a person approved the request that asked for it.
	k3sMCPAPIResourcesTool = "api_resources"
	k3sMCPResourceListTool = "resource_list"
	k3sMCPResourceGetTool  = "resource_get"

	// k3sMCPTimeout bounds the delegated call itself. The wait before it is
	// bounded by the gateway's own timeout.
	k3sMCPTimeout = 60 * time.Second
	// maxK3sMCPResponseBytes bounds what one delegated call brings back.
	maxK3sMCPResponseBytes = 4 << 20

	// The subresources a read may ask for. `log` is a pod's output and
	// `status` is an object's status; both are reads. The write
	// subresources of the K3s MCP are not in this slice.
	k3sMCPGetSubresourceLog    = "log"
	k3sMCPGetSubresourceStatus = "status"

	// k3sMCPMaxListLimit and k3sMCPMaxTailLines bound the two arguments that
	// can ask for a lot: a listing and a log tail.
	k3sMCPMaxListLimit = 500
	k3sMCPMaxTailLines = 5000

	// k3sMCPSpillSubdir keeps a spilled K3s MCP answer apart from a spilled
	// web fetch, so a later reader does not mistake one for the other.
	k3sMCPSpillSubdir = "k3s_mcp"
)

// k3sMCPClient calls the K3s MCP through the proxy container beside this one.
type k3sMCPClient struct {
	url    string
	client *http.Client
}

// newK3sMCPClient returns nil without a proxy URL, which is how a deployment
// that has not enabled the re-export keeps the tool surface as it was.
func newK3sMCPClient(rawURL string) *k3sMCPClient {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	return &k3sMCPClient{url: rawURL, client: &http.Client{Timeout: k3sMCPTimeout}}
}

// call makes one tools/call against the K3s MCP and returns the text of its
// tool result. A refusal from the server (isError) is an error here too, so
// the agent reads one shape: a text or a failure.
func (c *k3sMCPClient) call(ctx context.Context, tool string, arguments map[string]any) (string, error) {
	params := map[string]any{"name": tool}
	if len(arguments) > 0 {
		params["arguments"] = arguments
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  params,
	})
	if err != nil {
		return "", errors.New("the K3s MCP request could not be encoded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("the K3s MCP proxy URL is invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", errors.New("the K3s MCP proxy could not be reached")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxK3sMCPResponseBytes+1))
	if err != nil {
		return "", errors.New("the K3s MCP proxy response could not be read")
	}
	if len(raw) > maxK3sMCPResponseBytes {
		return "", errors.New("the K3s MCP response is too large")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the K3s MCP proxy returned status %d", resp.StatusCode)
	}
	var decoded struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *jsonRPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", errors.New("the K3s MCP returned a non-JSON body")
	}
	if decoded.Error != nil {
		return "", fmt.Errorf("the K3s MCP refused the call: %s",
			truncateText(decoded.Error.Message, maxRepoAccessErrorTextBytes))
	}
	if decoded.Result == nil {
		return "", errors.New("the K3s MCP returned no result")
	}
	var text strings.Builder
	for _, part := range decoded.Result.Content {
		if part.Type == "" || part.Type == "text" {
			text.WriteString(part.Text)
		}
	}
	if decoded.Result.IsError {
		reason := strings.TrimSpace(text.String())
		if reason == "" {
			reason = "the K3s MCP refused the call"
		}
		return "", errors.New(reason)
	}
	return text.String(), nil
}

// rejectUnknownArguments refuses arguments the tool does not have. The gate
// shows the request to a person (R4), so a request that does not fit the tool
// must not reach it (R3).
func rejectUnknownArguments(arguments json.RawMessage, tool string) error {
	if len(arguments) == 0 || string(arguments) == "null" {
		return nil
	}
	var given map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &given); err != nil {
		return fmt.Errorf("%s takes no arguments", tool)
	}
	if len(given) == 0 {
		return nil
	}
	keys := make([]string, 0, len(given))
	for key := range given {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return fmt.Errorf("%s takes no arguments, got %s", tool, strings.Join(keys, ", "))
}

// callK3sAPIResources lists the API resource kinds the cluster serves.
func (c sessionMCPConfig) callK3sAPIResources(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	if err := rejectUnknownArguments(arguments, k3sAPIResourcesTool); err != nil {
		return mcpToolError(err.Error()), nil
	}
	return c.delegateToK3sMCP(ctx, logger, k3sAPIResourcesTool, k3sMCPAPIResourcesTool, nil, map[string]string{})
}

// callK3sResourceList lists objects of one kind.
func (c sessionMCPConfig) callK3sResourceList(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	var args struct {
		APIVersion    string `json:"apiVersion"`
		Kind          string `json:"kind"`
		Namespace     string `json:"namespace"`
		LabelSelector string `json:"labelSelector"`
		FieldSelector string `json:"fieldSelector"`
		Limit         *int   `json:"limit"`
	}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return mcpToolError("arguments must be an object with an apiVersion and a kind"), nil
		}
	}
	apiVersion := strings.TrimSpace(args.APIVersion)
	kind := strings.TrimSpace(args.Kind)
	if apiVersion == "" {
		return mcpToolError("apiVersion is required"), nil
	}
	if kind == "" {
		return mcpToolError("kind is required"), nil
	}
	upstream := map[string]any{"apiVersion": apiVersion, "kind": kind}
	approval := map[string]string{"apiVersion": apiVersion, "kind": kind}
	if namespace := strings.TrimSpace(args.Namespace); namespace != "" {
		upstream["namespace"] = namespace
		approval["namespace"] = namespace
	}
	if selector := strings.TrimSpace(args.LabelSelector); selector != "" {
		upstream["labelSelector"] = selector
		approval["labelSelector"] = selector
	}
	if selector := strings.TrimSpace(args.FieldSelector); selector != "" {
		upstream["fieldSelector"] = selector
		approval["fieldSelector"] = selector
	}
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > k3sMCPMaxListLimit {
			return mcpToolError(fmt.Sprintf("limit must be between 1 and %d", k3sMCPMaxListLimit)), nil
		}
		upstream["limit"] = *args.Limit
		approval["limit"] = strconv.Itoa(*args.Limit)
	}
	return c.delegateToK3sMCP(ctx, logger, k3sResourceListTool, k3sMCPResourceListTool, upstream, approval)
}

// callK3sResourceGet reads one object, or one pod's logs.
func (c sessionMCPConfig) callK3sResourceGet(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	var args struct {
		APIVersion   string `json:"apiVersion"`
		Kind         string `json:"kind"`
		Namespace    string `json:"namespace"`
		Name         string `json:"name"`
		Subresource  string `json:"subresource"`
		Container    string `json:"container"`
		TailLines    *int   `json:"tailLines"`
		SinceSeconds *int   `json:"sinceSeconds"`
		Previous     *bool  `json:"previous"`
	}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return mcpToolError("arguments must be an object with an apiVersion, a kind and a name"), nil
		}
	}
	apiVersion := strings.TrimSpace(args.APIVersion)
	kind := strings.TrimSpace(args.Kind)
	name := strings.TrimSpace(args.Name)
	if apiVersion == "" {
		return mcpToolError("apiVersion is required"), nil
	}
	if kind == "" {
		return mcpToolError("kind is required"), nil
	}
	if name == "" {
		return mcpToolError("name is required"), nil
	}
	subresource := strings.TrimSpace(args.Subresource)
	switch subresource {
	case "", k3sMCPGetSubresourceLog, k3sMCPGetSubresourceStatus:
	default:
		return mcpToolError(fmt.Sprintf("subresource must be %s or %s", k3sMCPGetSubresourceLog, k3sMCPGetSubresourceStatus)), nil
	}
	container := strings.TrimSpace(args.Container)
	if container != "" && subresource != k3sMCPGetSubresourceLog {
		return mcpToolError("container is only meaningful with the log subresource"), nil
	}
	if args.TailLines != nil && (*args.TailLines < 1 || *args.TailLines > k3sMCPMaxTailLines) {
		return mcpToolError(fmt.Sprintf("tailLines must be between 1 and %d", k3sMCPMaxTailLines)), nil
	}
	if args.SinceSeconds != nil && *args.SinceSeconds < 1 {
		return mcpToolError("sinceSeconds must be at least 1"), nil
	}
	upstream := map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name}
	approval := map[string]string{"apiVersion": apiVersion, "kind": kind, "name": name}
	if namespace := strings.TrimSpace(args.Namespace); namespace != "" {
		upstream["namespace"] = namespace
		approval["namespace"] = namespace
	}
	if subresource != "" {
		upstream["subresource"] = subresource
		approval["subresource"] = subresource
	}
	if container != "" {
		upstream["container"] = container
		approval["container"] = container
	}
	if args.TailLines != nil {
		upstream["tailLines"] = *args.TailLines
		approval["tailLines"] = strconv.Itoa(*args.TailLines)
	}
	if args.SinceSeconds != nil {
		upstream["sinceSeconds"] = *args.SinceSeconds
		approval["sinceSeconds"] = strconv.Itoa(*args.SinceSeconds)
	}
	if args.Previous != nil {
		upstream["previous"] = *args.Previous
		approval["previous"] = strconv.FormatBool(*args.Previous)
	}
	return c.delegateToK3sMCP(ctx, logger, k3sResourceGetTool, k3sMCPResourceGetTool, upstream, approval)
}

// delegateToK3sMCP is the one path every re-exported tool takes: wait for a
// person, and only then call the K3s MCP beside this container. The tool that
// named the K3s MCP tool is the only thing that can choose it, and it chooses
// it after the arguments were checked (R3).
func (c sessionMCPConfig) delegateToK3sMCP(ctx context.Context, logger *slog.Logger, tool, upstreamTool string, upstreamArguments map[string]any, approval map[string]string) (any, *jsonRPCError) {
	if c.k3sMCP == nil {
		return mcpToolError("the K3s MCP re-export is not configured on this deployment"), nil
	}
	if approval == nil {
		approval = map[string]string{}
	}
	approval["tool"] = tool
	encoded, err := json.Marshal(approval)
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the approval context"}
	}
	requestID, err := newApprovalRequestID()
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not generate an approval request id"}
	}
	externalID := c.gateway.externalID(requestID)
	logger.Info("awaiting approval", "tool", tool, "external_id", externalID)
	c.notices.publish(approvalNotice{Kind: noticeAwaiting, Tool: tool, ExternalID: externalID})
	outcome, err := c.gateway.await(ctx, requestID, string(encoded))
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
	logger.Info("approved, delegating to the K3s MCP", "tool", tool, "upstream_tool", upstreamTool)
	body, err := c.k3sMCP.call(ctx, upstreamTool, upstreamArguments)
	if err != nil {
		logger.Error("the K3s MCP call failed", "tool", tool, "err", err)
		return mcpToolError("the K3s MCP call failed: " + err.Error()), nil
	}
	return c.k3sMCPResult(tool, requestID, body)
}

// k3sMCPResult carries the delegated answer back under the size rule
// web_fetch_get uses (AC-F5): a small answer rides the tool result, a large
// one goes to the session's shared volume and the result names the path.
func (c sessionMCPConfig) k3sMCPResult(tool, requestID, body string) (any, *jsonRPCError) {
	raw := []byte(body)
	result := map[string]any{"tool": tool, "ok": true}
	switch {
	case int64(len(raw)) <= maxInlineBodyBytes:
		result["body"] = body
	case c.sharedDir == "":
		result["body"] = body[:maxInlineBodyBytes]
		result["truncated"] = true
	default:
		head := raw[:maxInlineBodyBytes+1]
		spill, err := c.spillBodyIn(requestID, k3sMCPSpillSubdir, head, bytes.NewReader(raw[maxInlineBodyBytes+1:]))
		if err != nil {
			result["body"] = body[:maxInlineBodyBytes]
			result["truncated"] = true
			result["spillError"] = err.Error()
			break
		}
		result["bodyPath"] = spill.path
		result["bodyBytes"] = spill.size
		result["truncated"] = spill.truncated
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	return mcpToolText(string(payload)), nil
}

// The three tools' input schemas. They live beside the handlers rather than
// inside toolDefinitions so the surface's list stays a list of names, and so
// the property a caller sends as "name" is written where the tool's own
// arguments are read.
var (
	k3sAPIResourcesSchema = map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}

	k3sResourceListSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
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
				"description": "The namespace to list in. Omit to list across all namespaces.",
			},
			"labelSelector": map[string]any{
				"type":        "string",
				"description": "A label selector the listed objects must match.",
			},
			"fieldSelector": map[string]any{
				"type":        "string",
				"description": "A field selector the listed objects must match.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "How many objects to list at most.",
			},
		},
		"required":             []any{"apiVersion", "kind"},
		"additionalProperties": false,
	}

	k3sResourceGetSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"apiVersion": map[string]any{
				"type":        "string",
				"description": "The API version of the kind, for example v1.",
			},
			"kind": map[string]any{
				"type":        "string",
				"description": "The kind to read, for example Pod.",
			},
			"namespace": map[string]any{
				"type":        "string",
				"description": "The namespace the object is in. Omit for a cluster-scoped kind.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "The name of the object.",
			},
			"subresource": map[string]any{
				"type":        "string",
				"description": "log to read a pod's output, status to read an object's status.",
			},
			"container": map[string]any{
				"type":        "string",
				"description": "The container to read logs from. Only with the log subresource.",
			},
			"tailLines": map[string]any{
				"type":        "integer",
				"description": "How many log lines to read from the end.",
			},
			"sinceSeconds": map[string]any{
				"type":        "integer",
				"description": "How far back to read logs from, in seconds.",
			},
			"previous": map[string]any{
				"type":        "boolean",
				"description": "Whether to read the previous terminated container's logs.",
			},
		},
		"required":             []any{"apiVersion", "kind", "name"},
		"additionalProperties": false,
	}
)
