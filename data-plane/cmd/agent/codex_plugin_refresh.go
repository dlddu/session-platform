package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AC-J7: a long-lived codex session takes a plugin update without losing its
// conversation. Nothing refreshes on its own; the operator asks, between turns.
// The sidecar clones the marketplace again and the agent syncs the managed
// skills from it. The platform does not write into the conversation: the
// refresh leaves a transcript line for the user, and an in-turn refresh_skills
// call names the changes in its own tool result.
const (
	// pluginRefreshPath is the agent route the control plane calls.
	pluginRefreshPath = "/plugins/refresh"
	// pluginRefreshTurnPath is the loopback route the skills MCP's
	// refresh_skills calls from inside a running turn (AC-J7).
	pluginRefreshTurnPath = "/plugins/refresh/turn"
	// The sidecar's refresh is one clone, bounded by its own clone timeout.
	codexPluginRefreshTimeout = miniMaxCloneTimeout + 30*time.Second
	codexSkillChangeAdded     = "added"
	codexSkillChangeUpdated   = "updated"
	codexSkillChangeRemoved   = "removed"
	codexRevisionDisplayChars = 12
)

// pluginRefresh is what a refresh did, as the agent and the control plane
// report it.
type pluginRefresh struct {
	State    string   `json:"state"`
	Revision string   `json:"revision,omitempty"`
	Added    []string `json:"added"`
	Updated  []string `json:"updated"`
	Removed  []string `json:"removed"`
}

// pluginRefresher is an agentCLI whose platform plugins can be fetched again
// in a running session.
type pluginRefresher interface {
	refreshPlugins(homeDir string) (pluginRefresh, error)
}

var (
	errPluginRefreshUnsupported = errors.New("this workload has no platform plugins to refresh")
	errPluginRefreshBusy        = errors.New("a turn is running or queued; refresh plugins between turns")
	errPluginRefreshRunning     = errors.New("a plugin refresh is already running")
	errPluginRefreshFailed      = errors.New("plugin marketplace refresh failed")
	errPluginRefreshNoTurn      = errors.New("no turn is running; the in-turn refresh is for the running turn's own call")
	errPluginRefreshTurnLimit   = errors.New("this turn already refreshed the plugin skills; refresh again in a later turn")
)

var _ pluginRefresher = codexCLI{}

func (c codexCLI) refreshPlugins(homeDir string) (pluginRefresh, error) {
	unconfigured := pluginRefresh{State: miniMaxSeedStateUnconfigured, Added: []string{}, Updated: []string{}, Removed: []string{}}
	if c.k3sProxyURL == "" {
		return unconfigured, nil
	}
	refresh := c.refreshSidecar
	if refresh == nil {
		refresh = postCodexSidecarRefresh
	}
	result, err := refresh(c.k3sProxyURL)
	if err != nil {
		return pluginRefresh{}, err
	}
	if result.State == miniMaxSeedStateUnconfigured {
		return unconfigured, nil
	}
	probe := c.probeSidecar
	if probe == nil {
		probe = func(proxyURL string) (codexSidecar, error) {
			return fetchCodexSidecar(proxyURL, codexSidecarWait)
		}
	}
	// Unlike a boot, a refresh that cannot read the new seed keeps the skills
	// the session has rather than syncing them away.
	sidecar, err := probe(c.k3sProxyURL)
	if err != nil {
		return pluginRefresh{}, fmt.Errorf("%w: read the refreshed seed: %v", errPluginRefreshFailed, err)
	}
	changes, err := syncCodexSkills(codexSkillsDir(homeDir), codexSkillsFromSeed(sidecar.plugins, c.log()), c.log())
	if err != nil {
		return pluginRefresh{}, err
	}
	return pluginRefresh{
		State:    miniMaxSeedStateReady,
		Revision: sidecar.revision,
		Added:    nonNil(changes.Added),
		Updated:  nonNil(changes.Updated),
		Removed:  nonNil(changes.Removed),
	}, nil
}

func nonNil(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

func postCodexSidecarRefresh(proxyURL string) (miniMaxRefreshResult, error) {
	client := &http.Client{Timeout: codexPluginRefreshTimeout}
	resp, err := client.Post(strings.TrimRight(proxyURL, "/")+miniMaxPluginRefreshPath, "application/json", nil)
	if err != nil {
		return miniMaxRefreshResult{}, fmt.Errorf("%w: %v", errPluginRefreshFailed, err)
	}
	defer resp.Body.Close()
	var result miniMaxRefreshResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return miniMaxRefreshResult{}, fmt.Errorf("%w: sidecar answered %d without a result", errPluginRefreshFailed, resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return result, nil
	case http.StatusConflict:
		return result, fmt.Errorf("%w: %s", errPluginRefreshRunning, result.Error)
	default:
		return result, fmt.Errorf("%w: %s", errPluginRefreshFailed, result.Error)
	}
}

func shortRevision(revision string) string {
	if len(revision) > codexRevisionDisplayChars {
		return revision[:codexRevisionDisplayChars]
	}
	return revision
}

// pluginRefreshNotice is the transcript line a refresh leaves for the user.
func pluginRefreshNotice(result pluginRefresh) string {
	if result.State == miniMaxSeedStateUnconfigured {
		return "\n[session-platform: this session has no plugin marketplace to refresh]\n"
	}
	revision := ""
	if result.Revision != "" {
		revision = " at " + shortRevision(result.Revision)
	}
	var parts []string
	for _, group := range []struct {
		kind  string
		names []string
	}{{codexSkillChangeAdded, result.Added}, {codexSkillChangeUpdated, result.Updated}, {codexSkillChangeRemoved, result.Removed}} {
		if len(group.names) > 0 {
			parts = append(parts, group.kind+" "+strings.Join(group.names, ", "))
		}
	}
	if len(parts) == 0 {
		return "\n[session-platform: plugin skills refreshed" + revision + " — no changes]\n"
	}
	return "\n[session-platform: plugin skills refreshed" + revision + " — " + strings.Join(parts, "; ") + "]\n"
}
