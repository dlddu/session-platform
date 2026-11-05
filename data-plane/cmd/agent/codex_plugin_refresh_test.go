package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// refreshableCodexCLI is a codex CLI whose sidecar seed the test can change, as
// a marketplace push followed by a refresh would.
type refreshableCodexCLI struct {
	mu         sync.Mutex
	seed       []miniMaxSeedPlugin
	revision   string
	refreshErr error
	probeErr   error
	refreshes  int
}

func (r *refreshableCodexCLI) set(seed []miniMaxSeedPlugin, revision string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seed, r.revision = seed, revision
}

func (r *refreshableCodexCLI) cli(t *testing.T) codexCLI {
	t.Helper()
	cli := newCodexTestCLI(t, codexSidecar{}, nil)
	cli.probeSidecar = func(string) (codexSidecar, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return codexSidecar{configured: true, plugins: r.seed, revision: r.revision}, r.probeErr
	}
	cli.refreshSidecar = func(proxyURL string) (miniMaxRefreshResult, error) {
		if proxyURL != "http://127.0.0.1:8093" {
			t.Errorf("refreshed %q, want the loopback sidecar", proxyURL)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.refreshes++
		return miniMaxRefreshResult{State: miniMaxSeedStateReady, Revision: r.revision}, r.refreshErr
	}
	return cli
}

func skillSeed(skills map[string]string) []miniMaxSeedPlugin {
	files := map[string]string{}
	var paths []string
	for name, body := range skills {
		files["skills/"+name+"/SKILL.md"] = body
		paths = append(paths, `"./skills/`+name+`"`)
	}
	files[".claude-plugin/plugin.json"] = `{"name":"p","skills":[` + strings.Join(paths, ",") + `]}`
	return []miniMaxSeedPlugin{seedPlugin("p", files)}
}

// AC-J7: what the conversation last saw is what a change is measured from, so
// changes made before the next turn fold into one note.
func TestRecordCodexSkillChangesFolds(t *testing.T) {
	home := t.TempDir()
	steps := []codexSkillChanges{
		{Added: []string{"p--new", "p--brief"}, Updated: []string{"p--kept"}, Removed: []string{"p--old"}},
		{Updated: []string{"p--new"}, Removed: []string{"p--brief"}},
		{Added: []string{"p--old"}},
	}
	for _, step := range steps {
		if err := recordCodexSkillChanges(home, "0123456789abcdef", step); err != nil {
			t.Fatal(err)
		}
	}
	record, err := readCodexSkillChanges(home)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"p--new": "added", "p--kept": "updated", "p--old": "updated"}
	if !reflect.DeepEqual(record.Skills, want) || record.Revision != "0123456789abcdef" {
		t.Fatalf("record = %+v, want %v", record, want)
	}
	notice, err := (codexCLI{}).turnNotice(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"0123456789ab)", "added: `p--new`", "updated: `p--kept`, `p--old`", "read_skill"} {
		if !strings.Contains(notice, part) {
			t.Errorf("notice %q lacks %q", notice, part)
		}
	}
	if err := (codexCLI{}).clearTurnNotice(home); err != nil {
		t.Fatal(err)
	}
	if notice, err := (codexCLI{}).turnNotice(home); err != nil || notice != "" {
		t.Fatalf("notice after clear = %q, %v", notice, err)
	}

	// Changes that cancel out leave nothing to tell.
	_ = recordCodexSkillChanges(home, "r", codexSkillChanges{Added: []string{"p--x"}})
	_ = recordCodexSkillChanges(home, "r", codexSkillChanges{Removed: []string{"p--x"}})
	if _, err := os.Stat(codexSkillChangesPath(home)); !os.IsNotExist(err) {
		t.Errorf("a cancelled-out change left a record: %v", err)
	}

	// An unreadable record is advice lost, not a broken session.
	if err := os.WriteFile(codexSkillChangesPath(home), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if notice, err := (codexCLI{}).turnNotice(home); err != nil || notice != "" {
		t.Fatalf("notice from a corrupt record = %q, %v", notice, err)
	}
}

// AC-J7: a refresh asks the sidecar to clone again, syncs the skills from the
// new seed and reports what changed; when it cannot read a new seed it leaves
// the session's skills alone instead of syncing them away.
func TestCodexRefreshPlugins(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	sidecar := &refreshableCodexCLI{}
	sidecar.set(skillSeed(map[string]string{"keep": "v1\n", "edit": "v1\n", "drop": "v1\n"}), "rev-1")
	cli := sidecar.cli(t)
	if err := cli.ensureHome(home, toolSurface{}); err != nil {
		t.Fatal(err)
	}
	// As a first turn would.
	if err := cli.clearTurnNotice(home); err != nil {
		t.Fatal(err)
	}

	sidecar.set(skillSeed(map[string]string{"keep": "v1\n", "edit": "v2\n", "new": "v1\n"}), "rev-2")
	result, err := cli.refreshPlugins(home)
	if err != nil {
		t.Fatal(err)
	}
	want := pluginRefresh{State: "ready", Revision: "rev-2", Added: []string{"p--new"}, Updated: []string{"p--edit"}, Removed: []string{"p--drop"}}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("refresh = %+v, want %+v", result, want)
	}
	if got := readFile(t, filepath.Join(codexSkillsDir(home), "p--edit", "SKILL.md")); got != "v2\n" {
		t.Errorf("updated skill = %q", got)
	}
	if _, err := os.Stat(filepath.Join(codexSkillsDir(home), "p--drop")); !os.IsNotExist(err) {
		t.Error("a skill the marketplace dropped is still installed")
	}
	if notice, _ := cli.turnNotice(home); !strings.Contains(notice, "removed: `p--drop`") {
		t.Errorf("notice = %q", notice)
	}

	if result, err := cli.refreshPlugins(home); err != nil || len(result.Added)+len(result.Updated)+len(result.Removed) != 0 || result.Added == nil {
		t.Fatalf("a refresh with nothing new = %+v, %v; want empty lists", result, err)
	}

	sidecar.set(nil, "rev-3")
	sidecar.probeErr = errors.New("seed timed out")
	if _, err := cli.refreshPlugins(home); !errors.Is(err, errPluginRefreshFailed) {
		t.Fatalf("refresh with an unreadable seed = %v, want errPluginRefreshFailed", err)
	}
	if _, err := os.Stat(filepath.Join(codexSkillsDir(home), "p--keep", "SKILL.md")); err != nil {
		t.Error("an unreadable seed removed the session's skills")
	}
	sidecar.probeErr = nil
	sidecar.refreshErr = errPluginRefreshRunning
	if _, err := cli.refreshPlugins(home); !errors.Is(err, errPluginRefreshRunning) {
		t.Fatalf("refresh the sidecar refused = %v", err)
	}

	noProxy := codexCLI{logger: testLogger()}
	if result, err := noProxy.refreshPlugins(home); err != nil || result.State != miniMaxSeedStateUnconfigured {
		t.Fatalf("refresh without a sidecar = %+v, %v", result, err)
	}
}

func TestPostCodexSidecarRefresh(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusOK, `{"state":"ready","revision":"abc"}`, nil},
		{http.StatusConflict, `{"state":"ready","error":"busy"}`, errPluginRefreshRunning},
		{http.StatusBadGateway, `{"state":"ready","error":"clone failed"}`, errPluginRefreshFailed},
		{http.StatusNotFound, `not json`, errPluginRefreshFailed},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != miniMaxPluginRefreshPath {
				t.Errorf("%s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		result, err := postCodexSidecarRefresh(srv.URL)
		srv.Close()
		if !errors.Is(err, tc.want) || (tc.want == nil && result.Revision != "abc") {
			t.Errorf("status %d: %+v, %v; want %v", tc.status, result, err, tc.want)
		}
	}
}

// AC-J7 on the agent: the route refreshes only between turns, leaves a line
// in the transcript, and the next resumed turn — not a first one — opens with
// the note, which a successful turn then settles.
func TestCodexPluginRefreshRoute(t *testing.T) {
	sidecar := &refreshableCodexCLI{}
	sidecar.set(skillSeed(map[string]string{"s": "v1\n"}), "rev-1")
	runner := newFakeClaudeRunner()
	c, srv := newCodexTestServer(t, runner, platformDefaultModel, false, filepath.Join(t.TempDir(), "state"), sidecar.cli(t))

	refresh := func(wantStatus int) pluginRefresh {
		t.Helper()
		resp, err := http.Post(srv.URL+pluginRefreshPath, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != wantStatus {
			t.Fatalf("refresh = %d %s, want %d", resp.StatusCode, body, wantStatus)
		}
		var result pluginRefresh
		if wantStatus == http.StatusOK {
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	lastPrompt := func(run recordedClaudeRun) string { return run.argv[len(run.argv)-1] }

	// The boot sync recorded every skill as added; a first turn reads them as
	// they are and is not told, and its success settles the note.
	writeViaHTTP(t, srv, "first", http.StatusOK)
	if got := lastPrompt(receiveClaudeRun(t, runner.started)); got != "first" {
		t.Fatalf("first turn prompt = %q, want it untold", got)
	}
	waitClaudeIdle(t, c)

	sidecar.set(skillSeed(map[string]string{"s": "v2\n"}), "rev-2")
	if result := refresh(http.StatusOK); !reflect.DeepEqual(result.Updated, []string{"p--s"}) {
		t.Fatalf("refresh = %+v", result)
	}
	if payload, _ := c.out.Since(0); !strings.Contains(string(payload), "plugin skills refreshed at rev-2 — updated p--s") {
		t.Errorf("transcript lacks the refresh line: %q", payload)
	}

	runner.runErr = func(index int) error {
		if index == 1 {
			return errors.New("turn failed")
		}
		return nil
	}
	writeViaHTTP(t, srv, "second", http.StatusOK)
	if got := lastPrompt(receiveClaudeRun(t, runner.started)); !strings.HasPrefix(got, "[session-platform] The platform's plugin skills changed") || !strings.HasSuffix(got, "\n\nsecond") {
		t.Fatalf("resumed turn prompt = %q, want the note first", got)
	}
	waitClaudeIdle(t, c)
	writeViaHTTP(t, srv, "third", http.StatusOK)
	if got := lastPrompt(receiveClaudeRun(t, runner.started)); !strings.Contains(got, "updated: `p--s`") {
		t.Fatalf("after a failed turn the note must be carried again, got %q", got)
	}
	waitClaudeIdle(t, c)
	writeViaHTTP(t, srv, "fourth", http.StatusOK)
	if got := lastPrompt(receiveClaudeRun(t, runner.started)); got != "fourth" {
		t.Fatalf("after a successful turn the note must be gone, got %q", got)
	}
	waitClaudeIdle(t, c)

	// Never under a running or queued turn.
	runner.release = make(chan struct{})
	writeViaHTTP(t, srv, "long", http.StatusOK)
	receiveClaudeRun(t, runner.started)
	refresh(http.StatusConflict)
	close(runner.release)
	waitClaudeIdle(t, c)
	sidecar.mu.Lock()
	refreshes := sidecar.refreshes
	sidecar.mu.Unlock()
	if refreshes != 1 {
		t.Errorf("sidecar refreshes = %d, want 1: a refused refresh must not reach it", refreshes)
	}

	sidecar.refreshErr = errors.Join(errPluginRefreshFailed, errors.New("clone failed"))
	refresh(http.StatusBadGateway)
}

// Workloads without platform plugins have no refresh.
func TestPluginRefreshUnsupported(t *testing.T) {
	_, srv := newClaudeTestServer(t, newFakeClaudeRunner(), platformDefaultModel, false, nil)
	resp, err := http.Post(srv.URL+pluginRefreshPath, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("claude-code refresh = %d, want 404", resp.StatusCode)
	}
}
