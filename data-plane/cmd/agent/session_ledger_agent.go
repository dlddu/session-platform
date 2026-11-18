// The workload agent's relay onto the sidecar's read-only task ledger view
// (AC-N6). The database belongs to the slot and is mounted only in the sidecar,
// which binds loopback; the agent is the one container on the pod network, so
// a reader outside the session reaches the ledger through here.
//
// The relay adds nothing: GET in, GET out, only a page size and a cursor, and
// no path to the model or to the session's SQL.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// agentLedgerPath is the control plane's: GET reads one ledger page.
	// Keep in sync with control-plane/internal/adapter/agent (LedgerTasks).
	agentLedgerPath = "/ledger/tasks"

	// ledgerViewCall bounds one sidecar read. The sidecar bounds the statement
	// itself (sessionDBCallTimeout); this is the hop in front of it, kept short
	// so a panel read can never hold the agent open.
	ledgerViewCall = 35 * time.Second

	// maxLedgerViewBytes bounds a relayed page. One ledger page is capped by
	// the sidecar at half of sessionDBMaxResultBytes, so this only guards
	// against a sidecar answering something unexpected.
	maxLedgerViewBytes = 1 << 20
)

// ledgerReporter is the agentCLI that has a session database with a task
// ledger beside it — codex-chat, and only when the sidecar serves one.
type ledgerReporter interface {
	sessionLedgerURL() string
}

// sessionLedgerURL is the sidecar's ledger view, derived from the `db` MCP URL
// the pod already hands this container. A workload with no session database
// has none.
func (c codexCLI) sessionLedgerURL() string {
	return sessionLedgerURLFor(c.sessionDBURL)
}

func sessionLedgerURLFor(mcpURL string) string {
	if mcpURL == "" {
		return ""
	}
	return strings.TrimSuffix(mcpURL, sessionDBMCPPath) + sessionDBLedgerPath
}

// serveAgentLedger relays one ledger page. A workload that keeps no ledger says
// so with 501 rather than an empty page, which would claim the session has no
// work. Only the sidecar's own 200 and its 400 pass through; anything else is
// reported as a bad gateway, so a sidecar fault is never read as ledger data.
func serveAgentLedger(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	target := ""
	if reporter, ok := c.cli.(ledgerReporter); ok {
		target = reporter.sessionLedgerURL()
	}
	if target == "" {
		http.Error(w, "this session keeps no task ledger", http.StatusNotImplemented)
		return
	}
	query := url.Values{}
	for _, name := range []string{"limit", "after_id"} {
		if v := strings.TrimSpace(r.URL.Query().Get(name)); v != "" {
			query.Set(name, v)
		}
	}
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(r.Context(), ledgerViewCall)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "the task ledger view could not be reached", http.StatusBadGateway)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "the task ledger view could not be reached", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLedgerViewBytes))
	if err != nil {
		http.Error(w, "the task ledger page could not be read", http.StatusBadGateway)
		return
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusBadRequest:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	default:
		// The sidecar's own message can name its database; the class is what
		// leaves the pod.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "ledger_unavailable", "error": "the task ledger could not be read",
		})
	}
}
