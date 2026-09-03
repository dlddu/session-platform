// Session MCP (AC-F4). This is the helper pod container an approval-gated
// session's workload pod talks to for anything that leaves its own pod: AC-F6
// makes it that type's *only* outward tool surface, and AC-F3 puts a human
// approval gate in front of every external tool it will offer.
//
// This file lands the container's own contract — it starts, reports ready, and
// speaks the MCP endpoint the workload agent registers — and stops there. The
// external tools and the approval gate they must pass through are NOT
// implemented: tools/list is empty and nothing here calls the approval gateway
// or the network. That is deliberate and visible rather than stubbed behind a
// flag: an agent that registers this server today discovers a server with no
// tools, which is exactly the state docs/doc-tracker.md records.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

const (
	// sessionMCPProtocolVersion is the MCP revision this endpoint speaks. It is
	// the version the handshake settles on; clients that ask for another one
	// are told what this server supports rather than being refused.
	sessionMCPProtocolVersion = "2025-06-18"
	sessionMCPServerName      = "session-platform-session-mcp"
	sessionMCPPath            = "/mcp"

	// JSON-RPC 2.0 error codes used by this endpoint.
	jsonRPCParseError     = -32700
	jsonRPCInvalidRequest = -32600
	jsonRPCMethodNotFound = -32601
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// sessionMCPRoutes serves the helper pod's MCP container: the readiness
// endpoint the kubelet polls and the MCP endpoint the session's workload agent
// registers. It holds no session state — the helper pod is discarded on freeze
// and rebuilt on restore (AC-F4).
func sessionMCPRoutes(logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// Readiness. The MCP container is ready as soon as it can answer, because
	// it has nothing to load: no session state, no upstream connection.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc(sessionMCPPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "session MCP accepts POST", http.StatusMethodNotAllowed)
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
		// A notification carries no id and takes no response body.
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "initialize":
			writeJSONRPCResult(w, req.ID, map[string]any{
				"protocolVersion": sessionMCPProtocolVersion,
				// Only tools. This server exposes no resources and no prompts,
				// and it never will: everything it offers has to pass the
				// approval gate, which is a tool-call boundary (AC-F3).
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{
					"name":    sessionMCPServerName,
					"version": sessionMCPServerVersion,
				},
			})
		case "tools/list":
			// Empty until AC-F3 lands. The gate has to exist before a tool that
			// reaches outside the pod may be offered, so an approval-gated
			// session currently has no way out of its pod at all — which is the
			// safe end of that missing piece, not the dangerous one.
			writeJSONRPCResult(w, req.ID, map[string]any{"tools": []any{}})
		case "ping":
			writeJSONRPCResult(w, req.ID, map[string]any{})
		default:
			logger.Info("session MCP method not implemented", "method", req.Method)
			writeJSONRPCError(w, req.ID, jsonRPCMethodNotFound, "session MCP does not implement "+req.Method)
		}
	})

	return mux
}

// maxSessionMCPRequestBytes bounds a single JSON-RPC frame. The endpoint's only
// client is the session's own agent, so this is a guard against a runaway
// request rather than a product limit.
const maxSessionMCPRequestBytes = 1 << 20

// sessionMCPServerVersion is reported in the handshake. It stays at 0 while the
// server has no tools, so a client can tell an empty gate from a real one.
const sessionMCPServerVersion = "0"

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSONRPC(w, jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSONRPC(w, jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &jsonRPCError{Code: code, Message: message}})
}

// writeJSONRPC always answers 200: a JSON-RPC error is carried in the body, not
// in the HTTP status, so a transport-level failure stays distinguishable from a
// method that ran and refused.
func writeJSONRPC(w http.ResponseWriter, response jsonRPCResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
