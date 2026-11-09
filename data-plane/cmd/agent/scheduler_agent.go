// The workload agent's half of the session scheduler (docs/prd/session-scheduler.md):
// GET /queue tells the loopback sidecar when the agent is idle and how many
// turns it has finished (AC-P2), and GET /scheduler relays the sidecar's
// schedule count to the control plane's idle reaper (AC-P3).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	agentQueuePath     = "/queue"
	agentSchedulerPath = "/scheduler"

	schedulerStateCall = 3 * time.Second
)

// agentEpoch changes when the agent process restarts, which empties its
// in-memory queue; a run the sidecar was waiting on is then over.
var agentEpoch = strconv.FormatInt(time.Now().UnixNano(), 36)

// schedulerReporter is the agentCLI that has a scheduler beside it.
type schedulerReporter interface {
	schedulerStateURL() string
}

// queueState is idle only when a new prompt would start a turn right away:
// nothing running or queued, and nothing holding the worker or admission.
func (c *claudeWorkload) queueState() agentQueueState {
	c.mu.Lock()
	defer c.mu.Unlock()
	idle := c.ready && c.accepting && !c.stopping && !c.active && !c.refreshing &&
		!c.checkpointing && !c.restoring && c.queue.Len() == 0 && c.pendingLogin == nil
	return agentQueueState{Idle: idle, Finished: c.finished, Epoch: agentEpoch}
}

func serveAgentQueue(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	if !fromLoopback(r) {
		http.Error(w, "the queue state is only for the session's own scheduler", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(c.queueState())
}

// serveAgentScheduler answers {"active": n}. An agent without a scheduler
// answers 0, so the reaper applies the ordinary idle rule.
func serveAgentScheduler(w http.ResponseWriter, r *http.Request, c *claudeWorkload) {
	stateURL := ""
	if reporter, ok := c.cli.(schedulerReporter); ok {
		stateURL = reporter.schedulerStateURL()
	}
	w.Header().Set("Content-Type", "application/json")
	if stateURL == "" {
		_, _ = w.Write([]byte(`{"active":0}`))
		return
	}
	active, err := readSchedulerState(r.Context(), stateURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]int{"active": active})
}

func readSchedulerState(ctx context.Context, stateURL string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, schedulerStateCall)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stateURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read scheduler state: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("read scheduler state: sidecar returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Active *int `json:"active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Active == nil {
		return 0, fmt.Errorf("read scheduler state: no active count")
	}
	return *out.Active, nil
}

// schedulerMCPURLFromEnv is the workload side's SESSION_SCHEDULER_MCP_URL: the
// sidecar's endpoint, so only a loopback address is one.
func schedulerMCPURLFromEnv(getenv func(string) string) (string, error) {
	raw := strings.TrimSpace(getenv(schedulerMCPURLEnv))
	if raw == "" {
		return "", nil
	}
	if !strings.HasPrefix(raw, "http://127.0.0.1:") || !strings.HasSuffix(raw, schedulerMCPPath) {
		return "", fmt.Errorf("%s must be an http URL on 127.0.0.1 ending in %s", schedulerMCPURLEnv, schedulerMCPPath)
	}
	return raw, nil
}

func schedulerStateURLFor(mcpURL string) string {
	if mcpURL == "" {
		return ""
	}
	return strings.TrimSuffix(mcpURL, schedulerMCPPath) + schedulerStatePath
}
