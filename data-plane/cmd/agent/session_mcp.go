// Session MCP (AC-F4): the helper pod container an approval-gated session's
// workload pod talks to for anything that leaves its own pod (AC-F6).
//
// This file is the endpoint itself: the readiness probe, the handshake, and the
// JSON-RPC dispatch. The tools it offers and the gate they pass through live in
// session_mcp_tools.go.
//
// One invariant crosses both files: the tool list is empty exactly when there
// is no gate. A container started without the gateway triple offers nothing, so
// a missing Secret can never turn into an ungated call — it turns into a server
// with no tools, which is the safe end of a misconfiguration.
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
	jsonRPCInvalidParams  = -32602
	jsonRPCInternalError  = -32603
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
// registers. It holds no session state (AC-F4).
func sessionMCPRoutes(logger *slog.Logger, config sessionMCPConfig) http.Handler {
	if config.notices == nil {
		config.notices = newNoticeFeed()
	}
	mux := http.NewServeMux()

	// Readiness. The MCP container is ready as soon as it can answer, because
	// it has nothing to load: no session state, no upstream connection.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// The approval notice feed (session_mcp_notices.go). It is a plain GET rather
	// than part of the JSON-RPC surface because its reader is the agent process,
	// not the model: the tail must keep running between tool calls.
	mux.HandleFunc("GET "+noticesPath, func(w http.ResponseWriter, r *http.Request) {
		serveApprovalNotices(w, r, config.notices)
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
			writeJSONRPCResult(w, req.ID, map[string]any{"tools": config.toolDefinitions()})
		case "tools/call":
			// The handler's own context: an approval wait ends when the client
			// hangs up or the pod goes away, and never outlives either.
			result, rpcErr := config.callTool(r.Context(), logger, req.Params)
			if rpcErr != nil {
				writeJSONRPCError(w, req.ID, rpcErr.Code, rpcErr.Message)
				return
			}
			writeJSONRPCResult(w, req.ID, result)
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

// sessionMCPServerVersion is reported in the handshake.
const sessionMCPServerVersion = "1"

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
