// The workload agent's relay onto the sidecar's work records (AC-N9, AC-N10).
//
// Why a relay at all: the database belongs to the slot and is mounted only in
// the sidecar, which binds loopback, and the agent is the one container on the
// pod network. A reader outside the session reaches the work records through
// here.
//
// The relay adds nothing and decides nothing — a page size, a cursor and a task
// id on the way in, the sidecar's own answer on the way out, and no path to the
// model or to the session's SQL. The one write it carries is a person's
// request, which the sidecar turns into the next turn of a task.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// agentWorkTasksPath and agentWorkTurnsPath are the control plane's reads of
	// the agreed work records, and agentWorkRequestsPath its one write. Keep in
	// sync with control-plane/internal/adapter/agent (WorkTasks, WorkTurns,
	// CreateWorkRequest).
	agentWorkTasksPath    = "/work/tasks"
	agentWorkTurnsPath    = "/work/turns"
	agentWorkRequestsPath = "/work/requests"
)

func sessionWorkTasksURLFor(mcpURL string) string {
	return sessionWorkURLFor(mcpURL, sessionDBWorkTasksPath)
}

func sessionWorkTurnsURLFor(mcpURL string) string {
	return sessionWorkURLFor(mcpURL, sessionDBWorkTurnsPath)
}

func sessionWorkRequestsURLFor(mcpURL string) string {
	return sessionWorkURLFor(mcpURL, sessionDBWorkRequestsPath)
}

// sessionWorkURLFor keeps only the sidecar's host: a manager lane's MCP address
// carries the lane in its path, and the work views are the slot's, not a lane's.
func sessionWorkURLFor(mcpURL, path string) string {
	if mcpURL == "" {
		return ""
	}
	parsed, err := url.Parse(mcpURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: path}).String()
}

// serveAgentWorkRequests relays one stored request to the sidecar. It is a
// POST, and still passive: it starts no turn of the session's own and wakes
// nothing — the parent picks the request up when it next reads the task's open
// turns. The body is passed through unchanged and bounded, so the agent never
// becomes a place where a request can be rewritten.
func serveAgentWorkRequests(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	target := ""
	if reporter, ok := c.cli.(workReporter); ok {
		target = sessionWorkRequestsURLFor(reporter.sessionDBMCPURL())
	}
	if target == "" {
		http.Error(w, "this session keeps no work records", http.StatusNotImplemented)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWorkViewBytes))
	if err != nil {
		http.Error(w, "the request could not be read", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), workViewCall)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "the work records could not be reached", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "the work records could not be reached", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxWorkViewBytes))
	if err != nil {
		http.Error(w, "the reply could not be read", http.StatusBadGateway)
		return
	}
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(answer)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "work_unavailable", "error": "the request could not be stored",
		})
	}
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
	if reporter, ok := c.cli.(workReporter); ok {
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
	ctx, cancel := context.WithTimeout(r.Context(), workViewCall)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWorkViewBytes))
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
