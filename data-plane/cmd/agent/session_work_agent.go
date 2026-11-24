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
	// agentWorkWriterPath reads and moves the seal that says who may write this
	// slot's work records, and agentWorkCallPath is one work tool call made by
	// the bound monitor. Both are the control plane's: the handover is its
	// decision, and the monitor reaches these records only through it
	// (session_work_seal.go).
	agentWorkWriterPath = "/work/writer"
	agentWorkCallPath   = "/work/call"
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

func sessionWorkRequestsURLFor(mcpURL string) string {
	if mcpURL == "" {
		return ""
	}
	return strings.TrimSuffix(mcpURL, sessionDBMCPPath) + sessionDBWorkRequestsPath
}

func sessionWorkWriterURLFor(mcpURL string) string {
	if mcpURL == "" {
		return ""
	}
	return strings.TrimSuffix(mcpURL, sessionDBMCPPath) + sessionDBWorkWriterPath
}

func sessionWorkCallURLFor(mcpURL string) string {
	if mcpURL == "" {
		return ""
	}
	return strings.TrimSuffix(mcpURL, sessionDBMCPPath) + sessionDBWorkCallPath
}

// serveAgentWorkWriter relays the seal. The GET is the fact — who writes these
// records, at which epoch — and the POST is the handover or its rollback. The
// sidecar answers the POST only after the previous writer is actually refused,
// so a control plane that has this answer may open the next writer and must not
// before (session_work_seal.go).
func serveAgentWorkWriter(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	if r.Method == http.MethodGet {
		relayWorkView(w, r, c, sessionWorkWriterURLFor, nil, "the work writer view")
		return
	}
	relayWorkPost(w, r, c, sessionWorkWriterURLFor, "the work writer view")
}

// serveAgentWorkCall relays one work tool call made by the bound monitor.
func serveAgentWorkCall(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	relayWorkPost(w, r, c, sessionWorkCallURLFor, "the work call view")
}

// serveAgentWorkRequests relays one stored request to the sidecar. It is a
// POST, and still passive: it starts no turn of the session's own and wakes
// nothing — the parent picks the request up when it next reads the task's open
// turns. The body is passed through unchanged and bounded, so the agent never
// becomes a place where a request can be rewritten.
func serveAgentWorkRequests(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	relayWorkPost(w, r, c, sessionWorkRequestsURLFor, "the work request view")
}

// relayWorkPost is the one relay every write on this path uses: a stored
// request, the seal that hands the records over, and a work tool call made by
// the bound monitor. The body is passed through unchanged and bounded, so the
// agent never becomes a place where a caller's words can be rewritten, and the
// sidecar's own 200/201/400/404/409 pass through — a refusal has to stay a
// refusal, because the caller's next decision depends on which one it was.
func relayWorkPost(
	w http.ResponseWriter, r *http.Request, c *claudeWorkload,
	targetOf func(string) string, what string,
) {
	target := ""
	if reporter, ok := c.cli.(workReporter); ok {
		target = targetOf(reporter.sessionDBMCPURL())
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
			"code": "work_unavailable", "error": what + " did not answer"})
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
