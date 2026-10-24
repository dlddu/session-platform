package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testMarketplaceManifest = `{
  "name": "dlddu-plugins",
  "plugins": [
    {"name": "development", "source": "./", "description": "dev", "skills": ["./skillset/x"],
     "hooks": "./hooks/hooks.json"},
    {"name": "mcp-servers", "description": "MCP servers", "source": "./", "author": {"name": "dlddu"},
     "skills": [], "mcpServers": ["./mcp/k3s.json"]},
    {"name": "escapes", "source": "./", "skills": ["../outside"]},
    {"name": "remote", "source": {"source": "github", "repo": "x/y"}}
  ]
}`

func writeTestMarketplace(t *testing.T, dir string) {
	t.Helper()
	for path, content := range map[string]string{
		".claude-plugin/marketplace.json": testMarketplaceManifest,
		"skillset/x/SKILL.md":             "# x\n",
		"skillset/x/ref/notes.md":         "notes\n",
		"mcp/k3s.json":                    `{"mcpServers":{"k3s":{"type":"http","url":"https://homelab-k3s-mcp.llkm.nl/mcp"}}}`,
		"hooks/hooks.json":                `{"hooks":{}}`,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func decodedFiles(t *testing.T, plugin miniMaxSeedPlugin) map[string]string {
	t.Helper()
	files := map[string]string{}
	for path, encoded := range plugin.Files {
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		files[path] = string(content)
	}
	return files
}

// AC-G5: each selected plugin carries its manifest and the files its path
// fields name — never hooks, never anything outside its source.
func TestBuildMiniMaxSeedCarriesTheSelectedPlugins(t *testing.T) {
	clone := t.TempDir()
	writeTestMarketplace(t, clone)
	plugins, err := buildMiniMaxSeed(clone, []string{"mcp-servers", "development"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(plugins) != 2 || plugins[0].Name != "mcp-servers" || plugins[1].Name != "development" {
		t.Fatalf("plugins = %+v", plugins)
	}
	mcp := decodedFiles(t, plugins[0])
	if _, ok := mcp["mcp/k3s.json"]; !ok || len(mcp) != 2 {
		t.Errorf("mcp-servers files = %v, want its manifest and server file", keys(mcp))
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(mcp[".claude-plugin/plugin.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["name"] != "mcp-servers" || manifest["version"] != "1.0.0" {
		t.Errorf("manifest = %v", manifest)
	}
	dev := decodedFiles(t, plugins[1])
	if dev["skillset/x/SKILL.md"] != "# x\n" || dev["skillset/x/ref/notes.md"] != "notes\n" {
		t.Errorf("development files = %v, want the whole skill directory", keys(dev))
	}
	if _, ok := dev["hooks/hooks.json"]; ok {
		t.Error("a hook was carried into the bundle")
	}
	if err := json.Unmarshal([]byte(dev[".claude-plugin/plugin.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest["hooks"]; ok {
		t.Errorf("manifest still declares hooks: %v", manifest)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBuildMiniMaxSeedRefusesBadEntries(t *testing.T) {
	clone := t.TempDir()
	writeTestMarketplace(t, clone)
	for _, name := range []string{"missing", "escapes", "remote"} {
		if _, err := buildMiniMaxSeed(clone, []string{name}); err == nil {
			t.Errorf("plugin %s was accepted", name)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(clone, "skillset", "x", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildMiniMaxSeed(clone, []string{"development"}); err == nil {
		t.Error("a symlink in a plugin was followed")
	}
}

func newTestSeeder(t *testing.T, token func(context.Context) (string, error)) *miniMaxMarketplaceSeeder {
	t.Helper()
	s := newMiniMaxMarketplaceSeeder(http.NotFoundHandler(), "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.marketplaceURL = "https://github.com/dlddu/plugin-marketplace.git"
	s.plugins = []string{"mcp-servers"}
	s.workDir = t.TempDir()
	s.backoff = 0
	s.githubToken = token
	s.clone = func(_ context.Context, url, header, dir string) error {
		if url != s.marketplaceURL {
			t.Errorf("cloned %q", url)
		}
		want := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_test"))
		if header != want {
			t.Errorf("clone header = %q, want %q", header, want)
		}
		writeTestMarketplace(t, dir)
		return nil
	}
	return s
}

func seedResponse(t *testing.T, s *miniMaxMarketplaceSeeder) (int, miniMaxSeedBundle) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.serveSeed(rec, httptest.NewRequest(http.MethodGet, miniMaxPluginSeedPath, nil))
	var bundle miniMaxSeedBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return rec.Code, bundle
}

func TestMiniMaxSeederServesBootstrappingThenReady(t *testing.T) {
	s := newTestSeeder(t, func(context.Context) (string, error) { return "ghs_test", nil })
	if code, bundle := seedResponse(t, s); code != http.StatusServiceUnavailable || bundle.State != miniMaxSeedStateBootstrapping {
		t.Fatalf("before run: %d %s, want 503 bootstrapping", code, bundle.State)
	}
	s.run(context.Background())
	code, bundle := seedResponse(t, s)
	if code != http.StatusOK || bundle.State != miniMaxSeedStateReady || len(bundle.Plugins) != 1 {
		t.Fatalf("after run: %d %+v", code, bundle)
	}
	if entries, _ := os.ReadDir(s.workDir); len(entries) != 0 {
		t.Errorf("clone left behind: %v", entries)
	}
}

func TestMiniMaxSeederReportsAFailedBootstrap(t *testing.T) {
	attempts := 0
	s := newTestSeeder(t, func(context.Context) (string, error) {
		attempts++
		return "", errors.New("K3s MCP GitHub token tool returned an error")
	})
	s.run(context.Background())
	code, bundle := seedResponse(t, s)
	if code != http.StatusOK || bundle.State != miniMaxSeedStateFailed || len(bundle.Plugins) != 0 || attempts != miniMaxBootstrapAttempts {
		t.Fatalf("failed run: %d %+v after %d attempts", code, bundle, attempts)
	}
}

// The bootstrap's K3s MCP call goes through the proxy's own handler, which is
// what injects the token; the bootstrap sends none of its own.
func TestGithubTokenThroughProxy(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       string
	}{
		{"token", `{"result":{"content":[{"type":"resource","resource":{"text":"GITHUB_TOKEN=ghs_abc\nEXPIRES=x"}}]}}`, "ghs_abc"},
		{"tool error", `{"result":{"isError":true,"content":[]}}`, ""},
		{"rpc error", `{"error":{"code":-1}}`, ""},
		{"no token", `{"result":{"content":[{"type":"text","text":"GITHUB_TOKEN=nope"}]}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "" {
					t.Errorf("request %s with Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "github_app_installation_token") {
					t.Errorf("body = %s", body)
				}
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := githubTokenThroughProxy(context.Background(), upstream, "/mcp")
			if tc.want == "" {
				if err == nil {
					t.Fatalf("token = %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("token = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestK3SMCPProxySeedRoute(t *testing.T) {
	unconfigured, err := newK3SMCPProxyWithTransport("", "", nil, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured.seeder != nil {
		t.Error("an unconfigured proxy has a marketplace bootstrap")
	}
	rec := httptest.NewRecorder()
	unconfigured.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, miniMaxPluginSeedPath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), miniMaxSeedStateUnconfigured) {
		t.Errorf("unconfigured seed = %d %s", rec.Code, rec.Body.String())
	}

	// The approval-gated helper pod: a token for the session MCP's token mint,
	// and no marketplace to stage.
	t.Setenv(miniMaxMarketplaceDirEnv, "")
	tokenOnly, err := newK3SMCPProxyWithTransport("", "k3s-token", nil, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	if tokenOnly.seeder != nil {
		t.Error("a proxy without a marketplace directory has a marketplace bootstrap")
	}
	rec = httptest.NewRecorder()
	tokenOnly.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, miniMaxPluginSeedPath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), miniMaxSeedStateUnconfigured) {
		t.Errorf("seed without a marketplace directory = %d %s", rec.Code, rec.Body.String())
	}

	t.Setenv(miniMaxMarketplaceDirEnv, t.TempDir())
	configured, err := newK3SMCPProxyWithTransport("", "k3s-token", nil, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	if configured.seeder == nil {
		t.Fatal("a configured proxy has no marketplace bootstrap")
	}
	rec = httptest.NewRecorder()
	configured.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, miniMaxPluginSeedPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("seed before the bootstrap ran = %d, want 503", rec.Code)
	}
}

// The real clone: scoped header, no credential helper, and a failure message
// that never echoes the header.
func TestGitCloneMarketplaceFromALocalRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	writeTestMarketplace(t, src)
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	dst := filepath.Join(t.TempDir(), "clone")
	header := "Authorization: Basic c2VjcmV0"
	if err := gitCloneMarketplace(context.Background(), "file://"+src, header, dst); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".claude-plugin", "marketplace.json")); err != nil {
		t.Errorf("clone is missing the manifest: %v", err)
	}
	err := gitCloneMarketplace(context.Background(), "file://"+filepath.Join(src, "missing"), header, filepath.Join(t.TempDir(), "x"))
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Errorf("failed clone error = %v, want one without the header", err)
	}
}
