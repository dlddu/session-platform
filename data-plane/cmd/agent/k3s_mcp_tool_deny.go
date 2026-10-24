package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// k3sMCPDeniedTools are the K3s MCP tools no session may call: the list
// dlddu/reconciler-local denies in its Claude settings, kept identical on
// purpose. A trailing `*` matches a prefix, as it does in a Claude permission
// rule. Every other K3s MCP tool — the reads, token mints and the AWS config
// fetch — stays callable.
var k3sMCPDeniedTools = []string{
	"opensearch_*",
	"resource_create",
	"resource_update",
	"resource_patch",
	"resource_delete_collection",
	"resource_delete",
	"resource_exec",
	"resource_attach",
	"resource_port_forward",
	"resource_proxy",
	"session_*",
	"dear_baby_reset_user",
	"github_commit_status_create",
}

// k3sMCPClaudeToolPrefixes are the names under which the K3s MCP can reach a
// Claude session: a server registered as `k3s`, the claude.ai connector, and
// the marketplace's `mcp-servers` plugin, which the CLI namespaces by plugin.
var k3sMCPClaudeToolPrefixes = []string{
	"mcp__k3s__",
	"mcp__claude_ai_k3s__",
	"mcp__plugin_mcp-servers_k3s__",
}

// claudeDeniedK3SMCPTools is k3sMCPDeniedTools as Claude permission rules.
// The claude-code type installs no K3s MCP server today, so these rules are
// what keeps a later plugin change from silently granting the tools.
func claudeDeniedK3SMCPTools() []string {
	out := make([]string, 0, len(k3sMCPClaudeToolPrefixes)*len(k3sMCPDeniedTools))
	for _, prefix := range k3sMCPClaudeToolPrefixes {
		for _, tool := range k3sMCPDeniedTools {
			out = append(out, prefix+tool)
		}
	}
	return out
}

func k3sMCPToolDenied(name string) bool {
	for _, rule := range k3sMCPDeniedTools {
		if prefix, ok := strings.CutSuffix(rule, "*"); ok {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		} else if name == rule {
			return true
		}
	}
	return false
}

// maxK3SMCPRequestBody bounds what the deny filter buffers. MCP requests are
// small JSON-RPC envelopes; the largest the agent sends is a resource manifest.
const maxK3SMCPRequestBody = 4 << 20

type k3sMCPRequestMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// denyK3SMCPTools refuses tools/call for a denied tool before the proxy adds
// its bearer. mcode runs with --permission full and has no deny rules of its
// own, so for minimax-code this proxy is the one place the list can hold: the
// workload pod has no token and no other route to the K3s MCP (AC-G5, AC-G6).
//
// A body that does not parse is refused rather than forwarded, since what the
// upstream would make of it is exactly what this filter cannot see.
func denyK3SMCPTools(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxK3SMCPRequestBody+1))
		if err != nil {
			http.Error(w, "k3s mcp request body could not be read", http.StatusBadRequest)
			return
		}
		if len(body) > maxK3SMCPRequestBody {
			http.Error(w, "k3s mcp request body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		messages, batch, err := decodeK3SMCPRequest(body)
		if err != nil {
			http.Error(w, "k3s mcp request is not a JSON-RPC message", http.StatusBadRequest)
			return
		}
		for _, message := range messages {
			name, err := k3sMCPCalledTool(message)
			if err != nil {
				http.Error(w, "k3s mcp tools/call has malformed params", http.StatusBadRequest)
				return
			}
			if name == "" || !k3sMCPToolDenied(name) {
				continue
			}
			if batch {
				http.Error(w, fmt.Sprintf("k3s mcp tool %s is disabled in session-platform sessions", name), http.StatusForbidden)
				return
			}
			writeK3SMCPDenied(w, message.ID, name)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, r)
	})
}

func decodeK3SMCPRequest(body []byte) ([]k3sMCPRequestMessage, bool, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var messages []k3sMCPRequestMessage
		if err := json.Unmarshal(trimmed, &messages); err != nil {
			return nil, true, err
		}
		return messages, true, nil
	}
	var message k3sMCPRequestMessage
	if err := json.Unmarshal(trimmed, &message); err != nil {
		return nil, false, err
	}
	return []k3sMCPRequestMessage{message}, false, nil
}

// k3sMCPCalledTool is the tool a tools/call names, or "" for any other method.
func k3sMCPCalledTool(message k3sMCPRequestMessage) (string, error) {
	if message.Method != "tools/call" {
		return "", nil
	}
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return "", err
	}
	return params.Name, nil
}

// writeK3SMCPDenied answers with a tool result rather than a protocol error,
// so the model reads why the call failed instead of the CLI reporting a broken
// server.
func writeK3SMCPDenied(w http.ResponseWriter, id json.RawMessage, name string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	response, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{
			"isError": true,
			"content": []map[string]string{{
				"type": "text",
				"text": fmt.Sprintf("K3s MCP tool %s is disabled in session-platform sessions.", name),
			}},
		},
	})
	if err != nil {
		http.Error(w, "k3s mcp denial could not be encoded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(response)
}
