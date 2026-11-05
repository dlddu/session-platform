package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AC-J7: a long-lived codex session takes a plugin update without losing its
// conversation. Nothing refreshes on its own; the operator asks, between turns.
// The sidecar clones the marketplace again, the agent syncs the managed skills
// from it, and the next resumed turn opens with a note naming what changed —
// the conversation may hold an earlier read of a skill that is no longer what
// the skills MCP serves.
const (
	// pluginRefreshPath is the agent route the control plane calls.
	pluginRefreshPath     = "/plugins/refresh"
	codexSkillChangesFile = "session-platform-skill-changes.json"
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
	// turnNotice is the note the next resumed turn opens with, or "".
	turnNotice(homeDir string) (string, error)
	// clearTurnNotice drops it once a turn carrying it succeeded.
	clearTurnNotice(homeDir string) error
}

var (
	errPluginRefreshUnsupported = errors.New("this workload has no platform plugins to refresh")
	errPluginRefreshBusy        = errors.New("a turn is running or queued; refresh plugins between turns")
	errPluginRefreshRunning     = errors.New("a plugin refresh is already running")
	errPluginRefreshFailed      = errors.New("plugin marketplace refresh failed")
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
	if err := recordCodexSkillChanges(homeDir, sidecar.revision, changes); err != nil {
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

// codexSkillChangeRecord is the pending note, kept in HOME so it survives an
// archive: a session frozen between a refresh and its next turn is still told.
type codexSkillChangeRecord struct {
	Version  int               `json:"version"`
	Revision string            `json:"revision,omitempty"`
	Skills   map[string]string `json:"skills"`
}

func codexSkillChangesPath(homeDir string) string {
	return filepath.Join(homeDir, codexConfigDirName, codexSkillChangesFile)
}

// recordCodexSkillChanges folds changes into the pending record. What it keeps
// is each skill's change relative to what the conversation last saw, so a skill
// added and then removed before any turn is no change at all.
func recordCodexSkillChanges(homeDir, revision string, changes codexSkillChanges) error {
	if changes.empty() {
		return nil
	}
	record, err := readCodexSkillChanges(homeDir)
	if err != nil {
		return err
	}
	for kind, names := range map[string][]string{
		codexSkillChangeAdded:   changes.Added,
		codexSkillChangeUpdated: changes.Updated,
		codexSkillChangeRemoved: changes.Removed,
	} {
		for _, name := range names {
			if folded := foldCodexSkillChange(record.Skills[name], kind); folded == "" {
				delete(record.Skills, name)
			} else {
				record.Skills[name] = folded
			}
		}
	}
	record.Revision = revision
	if len(record.Skills) == 0 {
		return removeCodexSkillChanges(homeDir)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := codexSkillChangesPath(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("record codex skill changes: %w", err)
	}
	return os.Rename(tmp, path)
}

func foldCodexSkillChange(earlier, later string) string {
	switch {
	case earlier == "":
		return later
	case earlier == codexSkillChangeAdded && later == codexSkillChangeRemoved:
		return ""
	case earlier == codexSkillChangeAdded:
		return codexSkillChangeAdded
	case earlier == codexSkillChangeRemoved && later == codexSkillChangeAdded:
		return codexSkillChangeUpdated
	default:
		return later
	}
}

func readCodexSkillChanges(homeDir string) (codexSkillChangeRecord, error) {
	record := codexSkillChangeRecord{Version: 1, Skills: map[string]string{}}
	data, err := os.ReadFile(codexSkillChangesPath(homeDir))
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return record, fmt.Errorf("read codex skill changes: %w", err)
	}
	var stored codexSkillChangeRecord
	// An unreadable record is dropped, not fatal: the note is advice.
	if json.Unmarshal(data, &stored) != nil || stored.Version != 1 || stored.Skills == nil {
		return record, nil
	}
	return stored, nil
}

func removeCodexSkillChanges(homeDir string) error {
	if err := os.Remove(codexSkillChangesPath(homeDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear codex skill changes: %w", err)
	}
	return nil
}

func (codexCLI) turnNotice(homeDir string) (string, error) {
	record, err := readCodexSkillChanges(homeDir)
	if err != nil || len(record.Skills) == 0 {
		return "", err
	}
	return renderCodexSkillNotice(record), nil
}

func (codexCLI) clearTurnNotice(homeDir string) error {
	return removeCodexSkillChanges(homeDir)
}

func renderCodexSkillNotice(record codexSkillChangeRecord) string {
	byKind := map[string][]string{}
	for name, kind := range record.Skills {
		byKind[kind] = append(byKind[kind], "`"+name+"`")
	}
	var parts []string
	for _, kind := range []string{codexSkillChangeAdded, codexSkillChangeUpdated, codexSkillChangeRemoved} {
		if names := byKind[kind]; len(names) > 0 {
			sort.Strings(names)
			parts = append(parts, kind+": "+strings.Join(names, ", "))
		}
	}
	revision := ""
	if record.Revision != "" {
		revision = " (marketplace revision " + shortRevision(record.Revision) + ")"
	}
	return "[session-platform] The platform's plugin skills changed since your last turn" + revision + " — " +
		strings.Join(parts, "; ") + ". Anything you read from these skills earlier in this conversation may be " +
		"out of date: read a skill again with `read_skill` before following it, and do not use a removed one. " +
		"The user's message follows."
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
