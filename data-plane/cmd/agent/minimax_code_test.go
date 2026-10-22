package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newMiniMaxTestServerAt(t *testing.T, runner commandRunner, model string, restoreMode bool, stateDir string) (*claudeWorkload, miniMaxCLI, *httptest.Server) {
	t.Helper()
	cli, err := newMiniMaxCLI(stateDir, model, claudeProxyBaseURL, claudeProxyPlaceholderToken)
	if err != nil {
		t.Fatalf("new minimax cli: %v", err)
	}
	c, err := newClaudeWorkload(claudeConfig{
		StateDir:    stateDir,
		Model:       model,
		Binary:      defaultMiniMaxBinary,
		RestoreMode: restoreMode,
		Runner:      runner,
		Logger:      testLogger(),
		CLI:         cli,
	})
	if err != nil {
		t.Fatalf("new minimax workload: %v", err)
	}
	srv := httptest.NewServer(routes(testLogger(), &agent{claude: c}))
	t.Cleanup(func() {
		srv.Close()
		c.Close()
	})
	return c, cli, srv
}

func miniMaxArgv(cli miniMaxCLI, model string, resume bool, prompt string) []string {
	argv := []string{"mcode", "exec", "--config", cli.configPath,
		"--model", "custom_provider:session-platform/" + model,
		"--permission", "smart", "--output-format", "stream-json"}
	if resume {
		argv = append(argv, "--continue")
	}
	return append(argv, "--", prompt)
}

// AC-G2: each write is one `mcode exec` run in the session workspace with the
// session HOME, resuming only after a successful first run (AC-E4), and a
// prompt that looks like a flag stays a prompt.
func TestMiniMaxArgvAndResume(t *testing.T) {
	runner := newFakeClaudeRunner()
	stateDir := filepath.Join(t.TempDir(), "state")
	c, cli, srv := newMiniMaxTestServerAt(t, runner, "MiniMax-M3", false, stateDir)

	writeViaHTTP(t, srv, "--help me", http.StatusOK)
	first := receiveClaudeRun(t, runner.started)
	if want := miniMaxArgv(cli, "MiniMax-M3", false, "--help me"); !reflect.DeepEqual(first.argv, want) {
		t.Fatalf("first argv = %q, want %q", first.argv, want)
	}
	if first.dir != c.workDir || envValue(first.env, "HOME") != c.homeDir {
		t.Fatalf("run dir/HOME = %q/%q, want %q/%q", first.dir, envValue(first.env, "HOME"), c.workDir, c.homeDir)
	}
	waitClaudeIdle(t, c)

	writeViaHTTP(t, srv, "again", http.StatusOK)
	second := receiveClaudeRun(t, runner.started)
	if want := miniMaxArgv(cli, "MiniMax-M3", true, "again"); !reflect.DeepEqual(second.argv, want) {
		t.Fatalf("second argv = %q, want %q", second.argv, want)
	}
}

// AC-G1: platform-default (with no Secret model) runs the built-in default,
// and the managed config declares exactly the model the argv names.
func TestMiniMaxPlatformDefaultModel(t *testing.T) {
	runner := newFakeClaudeRunner()
	_, cli, srv := newMiniMaxTestServerAt(t, runner, platformDefaultModel, false, filepath.Join(t.TempDir(), "state"))

	writeViaHTTP(t, srv, "hi", http.StatusOK)
	run := receiveClaudeRun(t, runner.started)
	if want := miniMaxArgv(cli, miniMaxDefaultModel, false, "hi"); !reflect.DeepEqual(run.argv, want) {
		t.Fatalf("argv = %q, want %q", run.argv, want)
	}
	config := readMiniMaxConfig(t, cli.configPath)
	if config.DefaultModel != "custom_provider:session-platform/"+miniMaxDefaultModel {
		t.Fatalf("defaultModel = %q", config.DefaultModel)
	}
	if _, ok := config.CustomProvider["session-platform"].Models[miniMaxDefaultModel]; !ok {
		t.Fatalf("managed config does not declare %s: %+v", miniMaxDefaultModel, config)
	}
}

type miniMaxConfigShape struct {
	DefaultModel     string `json:"defaultModel"`
	DataContribution struct {
		Enabled bool `json:"enabled"`
	} `json:"dataContribution"`
	CustomProvider map[string]struct {
		API     string `json:"api"`
		Options struct {
			BaseURL string `json:"baseURL"`
			APIKey  string `json:"apiKey"`
		} `json:"options"`
		Models map[string]json.RawMessage `json:"models"`
	} `json:"custom_provider"`
}

func readMiniMaxConfig(t *testing.T, path string) miniMaxConfigShape {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read managed config: %v", err)
	}
	var config miniMaxConfigShape
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("managed config is not JSON (and so not the YAML mcode reads): %v\n%s", err, raw)
	}
	return config
}

// AC-G3: the managed config points mcode at the loopback proxy with the
// placeholder, lives outside the archived state tree, and is private.
func TestMiniMaxManagedConfig(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	c, cli, _ := newMiniMaxTestServerAt(t, newFakeClaudeRunner(), "MiniMax-M2.7", false, stateDir)

	if rel, err := filepath.Rel(c.stateDir, cli.configPath); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("managed config %q is inside the archived state tree %q", cli.configPath, c.stateDir)
	}
	info, err := os.Stat(cli.configPath)
	if err != nil {
		t.Fatalf("stat managed config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("managed config mode = %v, want 0600", info.Mode().Perm())
	}
	config := readMiniMaxConfig(t, cli.configPath)
	provider, ok := config.CustomProvider["session-platform"]
	if !ok {
		t.Fatalf("managed config has no session-platform provider: %+v", config)
	}
	if provider.API != "anthropic-messages" || provider.Options.BaseURL != claudeProxyBaseURL ||
		provider.Options.APIKey != claudeProxyPlaceholderToken {
		t.Fatalf("provider = %+v, want the loopback proxy with the placeholder", provider)
	}
	if config.DataContribution.Enabled {
		t.Fatal("managed config must not opt sessions into data contribution")
	}
}

// AC-G2: only assistant message text reaches the session output. Reasoning,
// tool calls and lifecycle records are dropped, deltas are not repeated by the
// completed record, and consecutive messages end on their own lines.
func TestMiniMaxStreamProjector(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"exec.started"}`,
		`{"type":"item.started","item":{"id":"r","type":"reasoning","contentDelta":"thinking..."}}`,
		`{"type":"item.started","item":{"id":"m1","type":"agent_message","contentDelta":"Let me "}}`,
		`{"type":"item.updated","item":{"id":"m1","type":"agent_message","contentDelta":"check."}}`,
		`{"type":"item.completed","item":{"id":"m1","type":"agent_message","content":"Let me check."}}`,
		`{"type":"item.started","item":{"id":"call_1","type":"tool_call","toolCall":{"name":"bash"}}}`,
		`{"type":"item.completed","item":{"id":"call_1","type":"tool_call","toolCall":{"name":"bash"}}}`,
		`{"type":"item.completed","item":{"id":"m2","type":"agent_message","content":"Done.\n"}}`,
		`plain diagnostic line`,
		`{"type":"turn.completed"}`,
		`{"type":"exec.completed","result":{"status":"succeeded","output":"Done."}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	p := newMiniMaxStreamProjector(&out)
	for _, chunk := range splitEvery(stream, 7) {
		if _, err := p.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if want := "Let me check.\nDone.\nplain diagnostic line\n"; out.String() != want {
		t.Fatalf("projected = %q, want %q", out.String(), want)
	}
}

// An interrupted run (no completed record) still ends its partial message with
// a newline on Close, like the Claude projector.
func TestMiniMaxStreamProjectorTerminatesPartialMessage(t *testing.T) {
	var out bytes.Buffer
	p := newMiniMaxStreamProjector(&out)
	_, _ = io.WriteString(p, `{"type":"item.started","item":{"id":"m","type":"agent_message","contentDelta":"partial"}}`)
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if out.String() != "partial\n" {
		t.Fatalf("projected = %q, want %q", out.String(), "partial\n")
	}
}

func splitEvery(s string, n int) []string {
	var parts []string
	for len(s) > n {
		parts = append(parts, s[:n])
		s = s[n:]
	}
	return append(parts, s)
}

// AC-G3: the CLI container refuses to start holding any credential or without
// the proxy wiring.
func TestValidateMiniMaxProxyClientEnv(t *testing.T) {
	good := func(t *testing.T) {
		t.Setenv("ANTHROPIC_BASE_URL", claudeProxyBaseURL)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", claudeProxyPlaceholderToken)
		for _, name := range miniMaxCredentialEnv {
			t.Setenv(name, "")
		}
	}
	t.Run("proxy wiring passes", func(t *testing.T) {
		good(t)
		if err := validateMiniMaxProxyClientEnv(); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("direct upstream is refused", func(t *testing.T) {
		good(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://api.minimax.io/anthropic")
		if err := validateMiniMaxProxyClientEnv(); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("real token is refused", func(t *testing.T) {
		good(t)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-cp-real")
		if err := validateMiniMaxProxyClientEnv(); err == nil {
			t.Fatal("want an error")
		}
	})
	for _, name := range miniMaxCredentialEnv {
		t.Run(name+" is refused", func(t *testing.T) {
			good(t)
			t.Setenv(name, "secret")
			if err := validateMiniMaxProxyClientEnv(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("err = %v, want one naming %s", err, name)
			}
		})
	}
}

// AC-G4: a minimax-code archive round-trips workspace, mcode's HOME data and
// the output cursor, needs no Claude settings, and the restored pod regenerates
// its own managed config and resumes the conversation.
func TestMiniMaxArchiveRestoreRoundTrip(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.output = func(_ int, opts runnerOptions) {
		_, _ = io.WriteString(opts.Stdout, `{"type":"item.completed","item":{"id":"m","type":"agent_message","content":"before-snapshot"}}`+"\n")
	}
	c1, _, srv1 := newMiniMaxTestServerAt(t, runner, "MiniMax-M3", false, filepath.Join(t.TempDir(), "state"))
	if _, err := os.Stat(filepath.Join(c1.homeDir, claudeSettingsDir)); !os.IsNotExist(err) {
		t.Fatalf("minimax-code HOME got Claude managed settings (err=%v)", err)
	}
	mcodeData := filepath.Join(c1.homeDir, ".minimax", "v2")
	if err := os.MkdirAll(mcodeData, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mcodeData, "session.json"), []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c1.workDir, "marker.txt"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeViaHTTP(t, srv1, "remember", http.StatusOK)
	before, cursor := eventuallyRead(t, srv1, 0, func(p string) bool { return strings.Contains(p, "before-snapshot") })
	waitClaudeIdle(t, c1)

	resp, err := http.Post(srv1.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	archive, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status=%d err=%v body=%s", resp.StatusCode, err, archive)
	}
	entries := tarEntryNames(t, archive)
	if !entries["state/home/.minimax/v2/session.json"] || !entries["state/workspace/marker.txt"] {
		t.Fatalf("archive is missing session state: %v", entries)
	}
	for name := range entries {
		if strings.Contains(name, miniMaxConfigDirName) {
			t.Fatalf("archive carries the managed config %q", name)
		}
	}
	if bytes.Contains(archive, []byte(claudeProxyPlaceholderToken)) {
		t.Fatal("archive carries the provider placeholder, so the managed config leaked into the state tree")
	}

	runner2 := newFakeClaudeRunner()
	c2, cli2, srv2 := newMiniMaxTestServerAt(t, runner2, "MiniMax-M3", true, filepath.Join(t.TempDir(), "restored"))
	restoreResp, err := http.Post(srv2.URL+"/restore", "application/x-tar", bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	body, _ := io.ReadAll(restoreResp.Body)
	restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", restoreResp.StatusCode, body)
	}
	if full, next := readViaHTTP(t, srv2, 0); full != before || next != cursor {
		t.Fatalf("restored output/cursor = (%q,%d), want (%q,%d)", full, next, before, cursor)
	}
	if got, err := os.ReadFile(filepath.Join(c2.homeDir, ".minimax", "v2", "session.json")); err != nil || string(got) != "history" {
		t.Fatalf("mcode data after restore = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(c2.workDir, "marker.txt")); err != nil || string(got) != "workspace" {
		t.Fatalf("workspace after restore = %q err=%v", got, err)
	}
	if rel, err := filepath.Rel(c2.stateDir, cli2.configPath); err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("restored managed config %q is not outside the state tree %q", cli2.configPath, c2.stateDir)
	}
	if got := readMiniMaxConfig(t, cli2.configPath); got.CustomProvider["session-platform"].Options.BaseURL != claudeProxyBaseURL {
		t.Fatalf("restored pod's managed config = %+v, want the loopback proxy", got)
	}

	writeViaHTTP(t, srv2, "continue", http.StatusOK)
	run := receiveClaudeRun(t, runner2.started)
	if want := miniMaxArgv(cli2, "MiniMax-M3", true, "continue"); !reflect.DeepEqual(run.argv, want) {
		t.Fatalf("post-restore argv = %q, want %q", run.argv, want)
	}
}

func tarEntryNames(t *testing.T, archive []byte) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		names[header.Name] = true
	}
}

// A record that is valid JSON but not the expected shape — here a tool item
// whose content is an object — is dropped, never echoed as raw text.
func TestMiniMaxStreamProjectorDropsUnexpectedRecords(t *testing.T) {
	var out bytes.Buffer
	p := newMiniMaxStreamProjector(&out)
	stream := `{"type":"item.completed","item":{"id":"c","type":"tool_call","content":{"stdout":"secret-ish tool output"}}}` + "\n" +
		`{"type":"item.completed","item":{"id":"m","type":"agent_message","content":["not","a","string"]}}` + "\n" +
		`["unexpected","array"]` + "\n" +
		`{"type":"item.completed","item":{"id":"m2","type":"agent_message","content":"ok"}}` + "\n"
	if _, err := io.WriteString(p, stream); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if out.String() != "ok\n" {
		t.Fatalf("projected = %q, want only %q", out.String(), "ok\n")
	}
}

// The healthz body names the workload the pod actually runs.
func TestMiniMaxHealthzNamesWorkload(t *testing.T) {
	_, _, srv := newMiniMaxTestServerAt(t, newFakeClaudeRunner(), "MiniMax-M3", false, filepath.Join(t.TempDir(), "state"))
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if body["status"] != "ok" || body["workload"] != workloadMiniMaxCode {
		t.Fatalf("healthz = %v, want ok/%s", body, workloadMiniMaxCode)
	}
}
