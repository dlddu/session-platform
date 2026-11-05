package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func newCodexTestCLI(t *testing.T, sidecar codexSidecar, probeErr error) codexCLI {
	t.Helper()
	cli := newCodexCLI("http://127.0.0.1:8093", testLogger())
	cli.agentBinary = "/agent"
	cli.probeSidecar = func(proxyURL string) (codexSidecar, error) {
		if proxyURL != "http://127.0.0.1:8093" {
			t.Errorf("probed %q, want the loopback sidecar", proxyURL)
		}
		return sidecar, probeErr
	}
	return cli
}

func newCodexTestServer(t *testing.T, runner commandRunner, model string, restoreMode bool, stateDir string, cli codexCLI) (*claudeWorkload, *httptest.Server) {
	t.Helper()
	c, err := newClaudeWorkload(claudeConfig{
		StateDir:    stateDir,
		Model:       model,
		Binary:      defaultCodexRunnerBinary,
		RestoreMode: restoreMode,
		Runner:      runner,
		Logger:      testLogger(),
		CLI:         cli,
	})
	if err != nil {
		t.Fatalf("new codex workload: %v", err)
	}
	srv := httptest.NewServer(routes(testLogger(), &agent{claude: c}))
	t.Cleanup(func() {
		srv.Close()
		c.Close()
	})
	return c, srv
}

// AC-J2: each write is one runner invocation in the session workspace with
// the session HOME; the model is always named, --resume follows AC-E4, and a
// prompt that looks like a flag stays a prompt.
func TestCodexArgvAndResume(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.runErr = func(index int) error {
		if index == 0 {
			return errors.New("first run failed")
		}
		return nil
	}
	c, srv := newCodexTestServer(t, runner, platformDefaultModel, false, filepath.Join(t.TempDir(), "state"), newCodexTestCLI(t, codexSidecar{}, nil))

	want := func(resume bool, prompt string) []string {
		argv := []string{defaultCodexRunnerBinary, "--model", platformDefaultModel}
		if resume {
			argv = append(argv, "--resume")
		}
		return append(argv, "--", prompt)
	}
	for i, tc := range []struct {
		prompt string
		resume bool
	}{{"--help me", false}, {"again", false}, {"third", true}} {
		writeViaHTTP(t, srv, tc.prompt, http.StatusOK)
		run := receiveClaudeRun(t, runner.started)
		if got := run.argv; !reflect.DeepEqual(got, want(tc.resume, tc.prompt)) {
			t.Fatalf("run %d argv = %q, want %q", i, got, want(tc.resume, tc.prompt))
		}
		if run.dir != c.workDir || envValue(run.env, "HOME") != c.homeDir {
			t.Fatalf("run dir/HOME = %q/%q", run.dir, envValue(run.env, "HOME"))
		}
		waitClaudeIdle(t, c)
	}
	if got := (codexCLI{}).argv("r", "gpt-6", "p", false); !reflect.DeepEqual(got, []string{"r", "--model", "gpt-6", "--", "p"}) {
		t.Fatalf("concrete model argv = %q", got)
	}
	if got := (codexCLI{}).argv("r", "", "p", false); got[2] != platformDefaultModel {
		t.Fatalf("empty model argv = %q, want the platform-default sentinel", got)
	}
}

// tomlCheck is a small TOML reader for what renderCodexConfig emits: comments,
// [dotted.tables], and key = value lines whose values are strings, booleans,
// integers, string arrays and inline tables of strings. It fails on anything
// else, which is the point: the rendering must stay inside that subset.
func tomlCheck(t *testing.T, content string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	table := ""
	keyRe := regexp.MustCompile(`^([A-Za-z0-9_-]+|"(?:[^"\\]|\\.)*")\s*=\s*(.*)$`)
	tableRe := regexp.MustCompile(`^\[([A-Za-z0-9_.-]+)\]$`)
	seenTables := map[string]bool{}
	for i, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := tableRe.FindStringSubmatch(line); m != nil {
			if seenTables[m[1]] {
				t.Fatalf("line %d: table [%s] declared twice", i+1, m[1])
			}
			seenTables[m[1]] = true
			table = m[1]
			continue
		}
		m := keyRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("line %d is not TOML this renderer emits: %q", i+1, line)
		}
		key := m[1]
		if strings.HasPrefix(key, `"`) {
			key = tomlUnquote(t, key)
		}
		full := key
		if table != "" {
			full = table + "." + key
		}
		if _, dup := doc[full]; dup {
			t.Fatalf("line %d: key %s set twice", i+1, full)
		}
		doc[full] = tomlValue(t, m[2])
	}
	return doc
}

func tomlValue(t *testing.T, raw string) any {
	t.Helper()
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "true" || raw == "false":
		return raw == "true"
	case strings.HasPrefix(raw, `"`):
		return tomlUnquote(t, raw)
	case strings.HasPrefix(raw, "["):
		inner := strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
		var out []string
		for _, part := range splitTOMLList(inner) {
			out = append(out, tomlUnquote(t, part))
		}
		return out
	case strings.HasPrefix(raw, "{"):
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "{"), "}"))
		out := map[string]string{}
		for _, part := range splitTOMLList(inner) {
			k, v, ok := strings.Cut(part, "=")
			if !ok {
				t.Fatalf("inline table entry %q", part)
			}
			out[strings.TrimSpace(k)] = tomlUnquote(t, strings.TrimSpace(v))
		}
		return out
	default:
		if regexp.MustCompile(`^-?[0-9]+$`).MatchString(raw) {
			return raw
		}
		t.Fatalf("unsupported TOML value %q", raw)
		return nil
	}
}

func splitTOMLList(s string) []string {
	var parts []string
	inString, escaped, start := false, false, 0
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		case r == ',' && !inString:
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		parts = append(parts, rest)
	}
	return parts
}

// tomlUnquote decodes a TOML basic string, rejecting raw control characters
// and any escape TOML does not define.
func tomlUnquote(t *testing.T, s string) string {
	t.Helper()
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		t.Fatalf("not a basic string: %q", s)
	}
	body := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c < 0x20 && c != '\t' || c == 0x7f {
			t.Fatalf("raw control character in %q", s)
		}
		if c == '"' {
			t.Fatalf("unescaped quote in %q", s)
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			t.Fatalf("dangling escape in %q", s)
		}
		switch body[i] {
		case '"', '\\':
			b.WriteByte(body[i])
		case 'b':
			b.WriteByte('\b')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'f':
			b.WriteByte('\f')
		case 'r':
			b.WriteByte('\r')
		case 'u':
			if i+5 > len(body) {
				t.Fatalf("short \\u escape in %q", s)
			}
			var r rune
			for _, h := range body[i+1 : i+5] {
				r <<= 4
				switch {
				case h >= '0' && h <= '9':
					r |= h - '0'
				case h >= 'A' && h <= 'F':
					r |= h - 'A' + 10
				case h >= 'a' && h <= 'f':
					r |= h - 'a' + 10
				default:
					t.Fatalf("bad \\u escape in %q", s)
				}
			}
			b.WriteRune(r)
			i += 4
		default:
			t.Fatalf("undefined escape \\%c in %q", body[i], s)
		}
	}
	return b.String()
}

// AC-J5: the managed config pins the approval, sandbox, web, login and update
// settings, turns every listed feature and the bundled skills off, and
// registers the skills MCP always and the K3s MCP only when asked to.
func TestRenderCodexConfig(t *testing.T) {
	withK3S := tomlCheck(t, string(renderCodexConfig(codexConfig{
		k3sMCPURL:   "http://127.0.0.1:8093/mcp",
		agentBinary: "/agent",
		skillsDir:   "/session/state/home/.agents/skills",
	})))
	for key, want := range map[string]any{
		"approval_policy":                                "never",
		"sandbox_mode":                                   "read-only",
		"web_search":                                     "disabled",
		"cli_auth_credentials_store":                     "file",
		"forced_login_method":                            "chatgpt",
		"check_for_update_on_startup":                    false,
		"skills.bundled.enabled":                         false,
		"mcp_servers.k3s.url":                            "http://127.0.0.1:8093/mcp",
		"mcp_servers.k3s.default_tools_approval_mode":    "approve",
		"mcp_servers.k3s.tool_timeout_sec":               "900",
		"mcp_servers.skills.command":                     "/agent",
		"mcp_servers.skills.default_tools_approval_mode": "approve",
	} {
		if got := withK3S[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if got := withK3S["mcp_servers.skills.args"]; !reflect.DeepEqual(got, []string{"skills-mcp"}) {
		t.Errorf("skills args = %#v", got)
	}
	if got := withK3S["mcp_servers.skills.env"]; !reflect.DeepEqual(got, map[string]string{codexSkillsDirEnv: "/session/state/home/.agents/skills"}) {
		t.Errorf("skills env = %#v", got)
	}
	for _, feature := range []string{"shell_tool", "unified_exec", "view_image", "image_generation", "apps", "plugins",
		"remote_plugin", "computer_use", "browser_use", "in_app_browser", "sleep_tool", "goals", "hooks", "memories",
		"tool_suggest", "skill_mcp_dependency_install"} {
		if withK3S["features."+feature] != false {
			t.Errorf("features.%s = %#v, want false", feature, withK3S["features."+feature])
		}
	}
	instructions, _ := withK3S["developer_instructions"].(string)
	if !strings.Contains(instructions, codexInstructionsMarker) || !strings.Contains(instructions, "read_skill") {
		t.Errorf("developer_instructions = %q, want the marker and the read_skill guidance", instructions)
	}

	without := tomlCheck(t, string(renderCodexConfig(codexConfig{agentBinary: "/agent", skillsDir: "/s"})))
	for key := range without {
		if strings.HasPrefix(key, "mcp_servers.k3s.") {
			t.Errorf("unconfigured sidecar still registers %s", key)
		}
	}
	if without["mcp_servers.skills.command"] != "/agent" {
		t.Error("the skills MCP must be registered without a K3s MCP too")
	}
}

// Every value goes through the encoder, so a hostile path cannot add a key.
func TestTOMLStringEscapes(t *testing.T) {
	hostile := "a\"b\\c\nd\re\tf\x00g\x1fh\x7fié\U0001F600\xffj\n[features]\nshell_tool = true"
	doc := tomlCheck(t, string(renderCodexConfig(codexConfig{agentBinary: hostile, skillsDir: hostile})))
	want := strings.ToValidUTF8(hostile, "\uFFFD")
	if doc["mcp_servers.skills.command"] != want {
		t.Fatalf("command = %q, want %q", doc["mcp_servers.skills.command"], want)
	}
	if doc["features.shell_tool"] != false {
		t.Fatal("an injected line changed a feature")
	}
	if got := tomlKey("CODEX_SKILLS_DIR"); got != "CODEX_SKILLS_DIR" {
		t.Errorf("bare key = %q", got)
	}
	if got := tomlKey("a.b"); got != `"a.b"` {
		t.Errorf("dotted key = %q, want it quoted", got)
	}
}

func TestValidateCodexClientEnv(t *testing.T) {
	good := func(t *testing.T) {
		t.Setenv(k3sMCPProxyURLEnv, "http://127.0.0.1:8093")
		for _, name := range codexCredentialEnv {
			t.Setenv(name, "")
		}
	}
	t.Run("loopback sidecar passes", func(t *testing.T) {
		good(t)
		got, err := validateCodexClientEnv()
		if err != nil || got != "http://127.0.0.1:8093" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	for _, value := range []string{
		"", "http://10.1.2.3:8093", "http://localhost:8093", "https://127.0.0.1:8093", "http://127.0.0.1:8091",
		"http://127.0.0.1:8093/mcp", "http://127.0.0.1", "http://u:p@127.0.0.1:8093", "http://[::1]:8094",
	} {
		t.Run("proxy "+value+" is refused", func(t *testing.T) {
			good(t)
			t.Setenv(k3sMCPProxyURLEnv, value)
			if _, err := validateCodexClientEnv(); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	t.Run("IPv6 loopback passes", func(t *testing.T) {
		good(t)
		t.Setenv(k3sMCPProxyURLEnv, "http://[::1]:8093")
		if _, err := validateCodexClientEnv(); err != nil {
			t.Fatal(err)
		}
	})
	for _, name := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "K3S_MCP_TOKEN",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "GITHUB_TOKEN"} {
		t.Run(name+" is refused", func(t *testing.T) {
			good(t)
			t.Setenv(name, "secret")
			if _, err := validateCodexClientEnv(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("err = %v, want one naming %s", err, name)
			}
		})
	}
}

func TestNewCodexWorkloadFromEnv(t *testing.T) {
	for _, name := range codexCredentialEnv {
		t.Setenv(name, "")
	}
	t.Setenv(k3sMCPProxyURLEnv, "http://127.0.0.1:8093")
	t.Setenv("CLAUDE_CODE_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	t.Setenv(codexModelEnv, "gpt-6")
	t.Setenv(codexRunnerBinaryEnv, "/opt/runner")
	if _, err := newCodexWorkloadFromEnv(testLogger(), true); err != nil {
		t.Fatalf("restore-mode codex workload: %v", err)
	}
	t.Setenv(codexModelEnv, "bad model")
	if _, err := newCodexWorkloadFromEnv(testLogger(), true); err == nil {
		t.Fatal("an invalid model must be refused at start")
	}
	t.Setenv(codexModelEnv, "")
	t.Setenv("OPENAI_API_KEY", "sk")
	if _, err := newCodexWorkloadFromEnv(testLogger(), true); err == nil {
		t.Fatal("a credential must be refused at start")
	}
}

func seedPlugin(name string, files map[string]string) miniMaxSeedPlugin {
	encoded := map[string]string{}
	for rel, content := range files {
		encoded[rel] = base64.StdEncoding.EncodeToString([]byte(content))
	}
	return miniMaxSeedPlugin{Name: name, Files: encoded}
}

var testCodexSeed = []miniMaxSeedPlugin{
	seedPlugin("session-platform", map[string]string{
		".claude-plugin/plugin.json":                       `{"name":"session-platform","skills":["./skillset/git-repo-cloner","./skillset/aws"]}`,
		"skillset/git-repo-cloner/SKILL.md":                "---\nname: git-repo-cloner\ndescription: \"Clone a repo.\"\n---\nbody\n",
		"skillset/aws/SKILL.md":                            "---\nname: aws\ndescription: >-\n  AWS login\n  by device code.\n---\n",
		"skillset/aws/scripts/start.sh":                    "#!/bin/sh\n",
		"skillset/unlisted/SKILL.md":                       "not named by the manifest\n",
		"mcp/k3s.json":                                     `{"mcpServers":{}}`,
		"skillset/git-repo-cloner/references/notes/one.md": "notes\n",
	}),
	seedPlugin("dev", map[string]string{
		".claude-plugin/plugin.json": `{"name":"dev"}`,
		"skills/review/SKILL.md":     "---\nname: review\ndescription: Review.\n---\n",
	}),
}

// AC-J5: only skills are installed, under <plugin>--<skill>, tagged as the
// platform's; the rest of a plugin (MCP servers, unnamed directories) is not.
func TestCodexSkillsFromSeed(t *testing.T) {
	skills := codexSkillsFromSeed(testCodexSeed, testLogger())
	names := make([]string, 0, len(skills))
	for name := range skills {
		names = append(names, name)
	}
	want := []string{"dev--review", "session-platform--aws", "session-platform--git-repo-cloner"}
	if !reflect.DeepEqual(sortedStrings(names), want) {
		t.Fatalf("skills = %v, want %v", sortedStrings(names), want)
	}
	if got := string(skills["session-platform--aws"]["scripts/start.sh"]); got != "#!/bin/sh\n" {
		t.Errorf("aws script = %q", got)
	}
	if _, ok := skills["session-platform--git-repo-cloner"]["references/notes/one.md"]; !ok {
		t.Error("nested skill files must be carried")
	}

	bad := codexSkillsFromSeed([]miniMaxSeedPlugin{
		seedPlugin("evil", map[string]string{".claude-plugin/plugin.json": `{"skills":["../outside"]}`}),
		seedPlugin("a--b", map[string]string{"skills/x/SKILL.md": "x"}),
		{Name: "broken", Files: map[string]string{"skills/x/SKILL.md": "%%%"}},
		seedPlugin("ok", map[string]string{"skills/x/SKILL.md": "x"}),
	}, testLogger())
	if len(bad) != 1 || bad["ok--x"] == nil {
		t.Fatalf("malformed plugins must cost only themselves, got %v", bad)
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestSyncCodexSkills(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	mustWrite := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("mine/SKILL.md", "user skill\n")
	mustWrite("old--gone/SKILL.md", "stale\n")
	mustWrite("old--gone/"+miniMaxManagedPluginMarker, "")
	mustWrite("p--s/SKILL.md", "previous pod's copy\n")
	mustWrite("p--s/leftover.md", "from the archive\n")
	mustWrite("p--s/"+miniMaxManagedPluginMarker, "")
	mustWrite("p--shadow/SKILL.md", "the user's own\n")

	skills := map[string]codexSkill{
		"p--s":      {"SKILL.md": []byte("new\n"), "refs/a.md": []byte("a\n")},
		"p--shadow": {"SKILL.md": []byte("seeded\n")},
	}
	if err := syncCodexSkills(dir, skills, testLogger()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, "mine", "SKILL.md")); got != "user skill\n" {
		t.Errorf("user skill = %q, want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "old--gone")); !os.IsNotExist(err) {
		t.Error("a managed skill this pod does not seed must be removed")
	}
	if got := readFile(t, filepath.Join(dir, "p--s", "SKILL.md")); got != "new\n" {
		t.Errorf("managed skill = %q, want the seed", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "p--s", "leftover.md")); !os.IsNotExist(err) {
		t.Error("an archived managed copy must be replaced, not merged")
	}
	if _, err := os.Stat(filepath.Join(dir, "p--s", miniMaxManagedPluginMarker)); err != nil {
		t.Error("an installed skill must carry the marker")
	}
	if got := readFile(t, filepath.Join(dir, "p--shadow", "SKILL.md")); got != "the user's own\n" {
		t.Errorf("shadowed skill = %q, want the user's", got)
	}

	if err := syncCodexSkills(dir, nil, testLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p--s")); !os.IsNotExist(err) {
		t.Error("an empty seed must remove every managed skill")
	}
	if _, err := os.Stat(filepath.Join(dir, "mine")); err != nil {
		t.Error("an empty seed must keep the user's skills")
	}
}

// AC-J5/AC-J6: ensureHome installs the seeded skills and the managed config,
// registers the K3s MCP only for a configured sidecar, and degrades to no K3s
// MCP and no skills rather than failing when the sidecar does not deliver.
func TestCodexEnsureHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	configPath := filepath.Join(home, ".codex", "config.toml")

	cli := newCodexTestCLI(t, codexSidecar{configured: true, plugins: testCodexSeed}, nil)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("sandbox_mode = \"danger-full-access\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cli.ensureHome(home, toolSurface{}); err != nil {
		t.Fatal(err)
	}
	doc := tomlCheck(t, readFile(t, configPath))
	if doc["sandbox_mode"] != "read-only" || doc["mcp_servers.k3s.url"] != "http://127.0.0.1:8093/mcp" {
		t.Fatalf("config = %v, want the managed read-only config with the K3s MCP", doc)
	}
	if doc["mcp_servers.skills.env"].(map[string]string)[codexSkillsDirEnv] != codexSkillsDir(home) {
		t.Errorf("skills MCP dir = %v", doc["mcp_servers.skills.env"])
	}
	if _, err := os.Stat(filepath.Join(codexSkillsDir(home), "session-platform--git-repo-cloner", "SKILL.md")); err != nil {
		t.Fatalf("seeded skill missing: %v", err)
	}

	degraded := newCodexTestCLI(t, codexSidecar{configured: true}, errors.New("seed timed out"))
	if err := degraded.ensureHome(home, toolSurface{}); err != nil {
		t.Fatal(err)
	}
	if doc := tomlCheck(t, readFile(t, configPath)); doc["mcp_servers.k3s.url"] == nil {
		t.Error("a configured sidecar whose seed failed still gives the K3s MCP")
	}
	if _, err := os.Stat(filepath.Join(codexSkillsDir(home), "session-platform--git-repo-cloner")); !os.IsNotExist(err) {
		t.Error("skills of a previous pod's seed must go when this pod has none")
	}

	unconfigured := newCodexTestCLI(t, codexSidecar{}, errors.New("unreachable"))
	if err := unconfigured.ensureHome(home, toolSurface{}); err != nil {
		t.Fatal(err)
	}
	if doc := tomlCheck(t, readFile(t, configPath)); doc["mcp_servers.k3s.url"] != nil {
		t.Error("an unreachable or unconfigured sidecar must not register the K3s MCP")
	}

	noProxy := codexCLI{agentBinary: "/agent", logger: testLogger()}
	if err := noProxy.ensureHome(home, toolSurface{}); err != nil {
		t.Fatal(err)
	}
}

func TestFetchCodexSidecar(t *testing.T) {
	seedCalls := 0
	configured := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case k3sMCPProxyHealthz:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "workload": workloadK3SMCPProxy, "configured": configured})
		case miniMaxPluginSeedPath:
			seedCalls++
			if seedCalls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(miniMaxSeedBundle{State: miniMaxSeedStateBootstrapping})
				return
			}
			_ = json.NewEncoder(w).Encode(miniMaxSeedBundle{State: miniMaxSeedStateReady, Plugins: testCodexSeed})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := srv.Client()

	got, err := fetchCodexSidecarWith(client, srv.URL, time.Now().Add(5*time.Second), time.Millisecond)
	if err != nil || !got.configured || len(got.plugins) != 2 || seedCalls != 2 {
		t.Fatalf("got %+v, %v after %d seed calls; want configured with the ready bundle", got, err, seedCalls)
	}

	configured = false
	seedCalls = 0
	got, err = fetchCodexSidecarWith(client, srv.URL, time.Now().Add(5*time.Second), time.Millisecond)
	if err != nil || got.configured || seedCalls != 0 {
		t.Fatalf("unconfigured sidecar: %+v, %v, %d seed calls", got, err, seedCalls)
	}

	got, err = fetchCodexSidecarWith(client, "http://127.0.0.1:1", time.Now(), time.Millisecond)
	if err == nil || got.configured {
		t.Fatalf("unreachable sidecar: %+v, %v", got, err)
	}
}

func projectCodex(t *testing.T, chunks ...string) string {
	t.Helper()
	var out bytes.Buffer
	p := newCodexStreamProjector(&out)
	for _, chunk := range chunks {
		if n, err := p.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// AC-J2: the projection shows notices, finished assistant messages, one line
// per MCP call and its failure, and failed turns; it drops everything else.
func TestCodexStreamProjector(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"session_platform.notice","text":"Open https://auth.openai.com/codex/device"}`,
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"r","type":"reasoning","text":"secret thoughts"}}`,
		`{"type":"item.completed","item":{"id":"r","type":"reasoning","text":"secret thoughts"}}`,
		`{"type":"item.started","item":{"id":"m","type":"agent_message","text":""}}`,
		`{"type":"item.updated","item":{"id":"m","type":"agent_message","text":"partial"}}`,
		`{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"Hello there"}}`,
		`{"type":"item.started","item":{"id":"c","type":"mcp_tool_call","server":"k3s","tool":"pod_list","arguments":{"ns":"x"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"c","type":"mcp_tool_call","server":"k3s","tool":"pod_list","result":{"content":[{"type":"text","text":"TOOL OUTPUT"}]},"status":"completed"}}`,
		`{"type":"item.started","item":{"id":"d","type":"mcp_tool_call","server":"skills","tool":"read_skill","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"d","type":"mcp_tool_call","server":"skills","tool":"read_skill","error":{"message":"no skill\nnamed x"},"status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"e","type":"mcp_tool_call","server":"k3s","tool":"x","status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"l","type":"todo_list","items":[{"text":"do","completed":false}]}}`,
		`{"type":"item.completed","item":{"id":"w","type":"web_search","query":"q"}}`,
		`{"type":"item.completed","item":{"id":"x","type":"error","message":"non-fatal"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1}}`,
		`{"type":"turn.failed","error":{"message":"usage limit reached"}}`,
		`{"type":"error","message":"stream disconnected"}`,
		`{"type":"error"}`,
	}, "\n") + "\n"
	want := "Open https://auth.openai.com/codex/device\n" +
		"Hello there\n" +
		"· k3s.pod_list\n" +
		"· skills.read_skill\n" +
		"error: skills.read_skill: no skill named x\n" +
		"error: k3s.x: failed\n" +
		"error: usage limit reached\n" +
		"error: stream disconnected\n" +
		"error: codex reported an error\n"
	if got := projectCodex(t, stream); got != want {
		t.Fatalf("projection =\n%s\nwant\n%s", got, want)
	}
	for _, size := range []int{1, 3, 7, 64} {
		if got := projectCodex(t, splitEvery(stream, size)...); got != want {
			t.Fatalf("projection split every %d bytes differs:\n%s", size, got)
		}
	}
}

func TestCodexStreamProjectorEdges(t *testing.T) {
	if got := projectCodex(t, "codex-runner: boom\n", "\n", "\r\n", `{"type":"item.completed","item":"nope"}`+"\n", `{"type":7}`+"\n", "[1,2]\n"); got != "codex-runner: boom\n" {
		t.Errorf("garbage = %q, want only the non-JSON diagnostic", got)
	}
	if got := projectCodex(t, `{"type":"item.completed","item":{"type":"agent_message","text":"cut off"}}`); got != "cut off\n" {
		t.Errorf("unterminated final record = %q", got)
	}
	if got := projectCodex(t, `{"type":"item.completed","item":{"type":"agent_mes`); got != `{"type":"item.completed","item":{"type":"agent_mes`+"\n" {
		t.Errorf("a half record from a killed runner = %q, want it as a diagnostic", got)
	}
	if got := projectCodex(t, `{"type":"item.completed","item":{"type":"agent_message","text":"é😀"}}`+"\n"); got != "é😀\n" {
		t.Errorf("unicode = %q", got)
	}

	huge := `{"type":"item.completed","item":{"type":"agent_message","text":"` + strings.Repeat("x", codexMaxRecordBytes) + `"}}`
	after := `{"type":"item.completed","item":{"type":"agent_message","text":"after"}}` + "\n"
	for _, chunks := range [][]string{{huge + "\n" + after}, splitEvery(huge+"\n"+after, 1<<20)} {
		if got := projectCodex(t, chunks...); got != codexOversizedRecordLine+"after\n" {
			t.Errorf("oversized record = %q", truncateForLog(got))
		}
	}
	long := strings.Repeat("é", codexMaxDetailBytes)
	got := projectCodex(t, `{"type":"error","message":"`+long+`"}`+"\n")
	if !strings.HasSuffix(got, "…\n") || len(got) > codexMaxDetailBytes+32 || !strings.HasPrefix(got, "error: é") {
		t.Errorf("long detail = %q", truncateForLog(got))
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// The image build's surface check runs against this rendering.
func TestRunAgentSubcommandRendersConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	var stderr bytes.Buffer
	code, handled := runAgentSubcommand([]string{codexRenderConfigCommand, home}, nil, io.Discard, &stderr)
	if !handled || code != 0 {
		t.Fatalf("render = %d/%v: %s", code, handled, stderr.String())
	}
	doc := tomlCheck(t, readFile(t, filepath.Join(home, ".codex", "config.toml")))
	if doc["mcp_servers.k3s.url"] != "http://127.0.0.1:8093/mcp" {
		t.Errorf("rendered config = %v, want the widest surface (K3s MCP included)", doc)
	}
	if out := os.Getenv("CODEX_RENDER_CONFIG_OUT"); out != "" {
		if code, _ := runAgentSubcommand([]string{codexRenderConfigCommand, out}, nil, io.Discard, &stderr); code != 0 {
			t.Fatalf("render to %s: %s", out, stderr.String())
		}
	}
	if _, handled := runAgentSubcommand(nil, nil, io.Discard, io.Discard); handled {
		t.Error("no arguments is the agent itself")
	}
	if code, handled := runAgentSubcommand([]string{codexRenderConfigCommand}, nil, io.Discard, io.Discard); !handled || code != 2 {
		t.Error("a missing home is a usage error")
	}
}

// AC-J3/AC-J4: the archive carries HOME including the ChatGPT login and the
// thread record, drops codex's stale helper links, and a restored pod rewrites
// the managed config and resumes.
func TestCodexArchiveRestoreRoundTrip(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.output = func(_ int, opts runnerOptions) {
		_, _ = io.WriteString(opts.Stdout, `{"type":"item.completed","item":{"type":"agent_message","text":"before-snapshot"}}`+"\n")
	}
	cli := newCodexTestCLI(t, codexSidecar{configured: true}, nil)
	c1, srv1 := newCodexTestServer(t, runner, platformDefaultModel, false, filepath.Join(t.TempDir(), "state"), cli)
	writeViaHTTP(t, srv1, "hi", http.StatusOK)
	receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c1)

	codexHome := filepath.Join(c1.homeDir, ".codex")
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "session-platform-thread.json"), []byte(`{"version":1,"threadId":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	links := filepath.Join(codexHome, "tmp", "arg0-linux", "codex-arg0x")
	if err := os.MkdirAll(links, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/local/bin/codex", filepath.Join(links, "apply_patch")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("sandbox_mode = \"danger-full-access\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv1.URL+"/checkpoint", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint = %d: %s", resp.StatusCode, archive)
	}
	names := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[header.Name] = true
	}
	for _, want := range []string{"state/home/.codex/auth.json", "state/home/.codex/session-platform-thread.json"} {
		if !names[want] {
			t.Errorf("archive is missing %s", want)
		}
	}
	for name := range names {
		if strings.Contains(name, "arg0-linux") {
			t.Errorf("archive carries codex helper link state %s", name)
		}
	}

	runner2 := newFakeClaudeRunner()
	c2, srv2 := newCodexTestServer(t, runner2, platformDefaultModel, true, filepath.Join(t.TempDir(), "state"), cli)
	restore, err := http.Post(srv2.URL+"/restore", "application/octet-stream", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(restore.Body)
	restore.Body.Close()
	if restore.StatusCode != http.StatusOK {
		t.Fatalf("restore = %d: %s", restore.StatusCode, body)
	}
	if got := readFile(t, filepath.Join(c2.homeDir, ".codex", "auth.json")); got != `{"tokens":{}}` {
		t.Errorf("restored login = %q", got)
	}
	if doc := tomlCheck(t, readFile(t, filepath.Join(c2.homeDir, ".codex", "config.toml"))); doc["sandbox_mode"] != "read-only" {
		t.Error("a restore must rewrite the managed config")
	}
	writeViaHTTP(t, srv2, "next", http.StatusOK)
	run := receiveClaudeRun(t, runner2.started)
	if !slicesContain(run.argv, "--resume") {
		t.Errorf("restored argv = %q, want --resume", run.argv)
	}
}

func TestCodexHealthzNamesWorkload(t *testing.T) {
	_, srv := newCodexTestServer(t, newFakeClaudeRunner(), platformDefaultModel, false, filepath.Join(t.TempDir(), "state"), newCodexTestCLI(t, codexSidecar{}, nil))
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["workload"] != workloadCodex {
		t.Fatalf("healthz = %v, want workload %s", body, workloadCodex)
	}
}
