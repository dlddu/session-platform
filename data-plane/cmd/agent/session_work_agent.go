// The workload agent's relay onto the sidecar's read-only work views (AC-N9).
//
// Why a relay at all is the reason the ledger relay beside it exists: the
// database belongs to the slot and is mounted only in the sidecar, which binds
// loopback, and the agent is the one container on the pod network. A reader
// outside the session reaches the work records through here.
//
// The relay adds nothing and decides nothing — GET in, GET out, a page size, a
// cursor and a task id, and no path to the model or to the session's SQL.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// agentWorkTasksPath and agentWorkTurnsPath are the control plane's reads of
	// the agreed work records. Keep in sync with
	// control-plane/internal/adapter/agent (WorkTasks, WorkTurns).
	agentWorkTasksPath = "/work/tasks"
	agentWorkTurnsPath = "/work/turns"
)

func sessionWorkTasksURLFor(mcpURL string) string {
	if mcpURL == "" {
		return ""
	}
	return strings.TrimSuffix(mcpURL, sessionDBMCPPath) + sessionDBWorkTasksPath
}

func sessionWorkTurnsURLFor(mcpURL string) string {
	if mcpURL == "" {
		return ""
	}
	return strings.TrimSuffix(mcpURL, sessionDBMCPPath) + sessionDBWorkTurnsPath
}

// serveAgentWorkTasks relays one page of this slot's work tasks.
func serveAgentWorkTasks(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	relayWorkView(w, r, c, sessionWorkTasksURLFor,
		[]string{"limit", "after_id", "status", "session_id"}, "the work task view")
}

// serveAgentWorkTurns relays one page of one task's turns.
func serveAgentWorkTurns(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	relayWorkView(w, r, c, sessionWorkTurnsURLFor,
		[]string{"task_id", "after_turn_id", "limit", "open"}, "the work turn view")
}

// relayWorkView is the one relay both work reads use. A workload that keeps no
// session database says so with 501 rather than an empty page, which would
// claim the session has no work. The sidecar's own 200/400/404/409 pass
// through — "no such task" and "the migration holds the database" are answers a
// caller acts on differently — and anything else is reported as a bad gateway,
// so a sidecar fault is never read as work data.
func relayWorkView(
	w http.ResponseWriter, r *http.Request, c *claudeWorkload,
	targetOf func(string) string, forward []string, what string,
) {
	target := ""
	if reporter, ok := c.cli.(ledgerReporter); ok {
		target = targetOf(reporter.sessionDBMCPURL())
	}
	if target == "" {
		http.Error(w, "this session keeps no work records", http.StatusNotImplemented)
		return
	}
	query := url.Values{}
	for _, name := range forward {
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
		http.Error(w, what+" could not be reached", http.StatusBadGateway)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, what+" could not be reached", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLedgerViewBytes))
	if err != nil {
		http.Error(w, what+" page could not be read", http.StatusBadGateway)
		return
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	default:
		// The sidecar's own message can name its database; the class is what
		// leaves the pod.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "work_unavailable", "error": "the work records could not be read",
		})
	}
}
