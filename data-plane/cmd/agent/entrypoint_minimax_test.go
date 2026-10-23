package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const miniMaxTestMarketplace = `{
  "name": "dlddu-plugins",
  "plugins": [
    {"name": "development", "source": "./", "skills": ["./skillset/x"]},
    {"name": "mcp-servers", "description": "MCP servers", "source": "./", "author": {"name": "dlddu"},
     "skills": [], "mcpServers": ["./mcp/k3s.json"]}
  ]
}`

// The shape the real marketplace ships: an OAuth client id mcode rejects and a
// bearer header with a ${VAR} mcode would send literally.
const miniMaxTestK3SServer = `{
  "mcpServers": {
    "k3s": {
      "type": "http",
      "url": "https://homelab-k3s-mcp.llkm.nl/mcp",
      "oauthClientId": "homelab-k3s-mcp",
      "headers": {"Authorization": "Bearer ${K3S_MCP_TOKEN}"}
    }
  }
}`

type miniMaxEntrypointRun struct {
	dir, logPath, seedDir string
	output                []byte
	err                   error
}

func runMiniMaxEntrypoint(t *testing.T, configured bool, k3sServer string) miniMaxEntrypointRun {
	t.Helper()
	dir := t.TempDir()
	run := miniMaxEntrypointRun{
		dir:     dir,
		logPath: filepath.Join(dir, "calls.log"),
		seedDir: filepath.Join(dir, "seed"),
	}
	fixture := filepath.Join(dir, "marketplace-fixture")
	for path, content := range map[string]string{
		".claude-plugin/marketplace.json": miniMaxTestMarketplace,
		"mcp/k3s.json":                    k3sServer,
	} {
		full := filepath.Join(fixture, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	health := `{"status":"ok","workload":"k3s-mcp-proxy","configured":false}`
	if configured {
		health = `{"status":"ok","workload":"k3s-mcp-proxy","configured":true}`
	}
	writeEntrypointTestExecutable(t, dir, "curl", `
for target in "$@"; do :; done
case "$target" in
  */healthz)
    printf 'curl:healthz\n' >> "$CALL_LOG"
    printf '%s\n' "$HEALTH"
    ;;
  *)
    [ "$target" = "http://127.0.0.1:8093/mcp" ]
    case "$*" in *Authorization*) echo "the CLI container sent its own auth" >&2; exit 1 ;; esac
    printf 'curl:mcp\n' >> "$CALL_LOG"
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"isError":false,"content":[{"type":"resource","resource":{"uri":"file:///github-token.env","mimeType":"text/plain","text":"GITHUB_TOKEN=test-installation-token\n"}}]}}'
    ;;
esac`)
	writeEntrypointTestExecutable(t, dir, "git", `
[ "$1" = "clone" ]
[ "$GIT_CONFIG_COUNT" = "1" ]
[ "$GIT_CONFIG_KEY_0" = "http.https://github.com/dlddu/plugin-marketplace.git.extraheader" ]
[ "$GIT_CONFIG_VALUE_0" = "Authorization: Basic eC1hY2Nlc3MtdG9rZW46dGVzdC1pbnN0YWxsYXRpb24tdG9rZW4=" ]
[ "$GIT_TERMINAL_PROMPT" = "0" ]
for target in "$@"; do :; done
cp -R "$FIXTURE/." "$target"
printf 'git:clone\n' >> "$CALL_LOG"
`)
	agent := writeEntrypointTestExecutable(t, dir, "agent", `
[ "${K3S_MCP_TOKEN+x}" != "x" ]
[ "${GIT_CONFIG_COUNT+x}" != "x" ]
[ "${github_token+x}" != "x" ]
printf 'agent:seed=%s\n' "${MINIMAX_CODE_PLUGIN_SEED_DIR-unset}" >> "$CALL_LOG"
`)

	cmd := exec.Command("/bin/sh", filepath.Join("..", "..", "entrypoint.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"DATA_PLANE_WORKLOAD=minimax-code",
		"DATA_PLANE_AGENT_BIN="+agent,
		"MINIMAX_CODE_PLUGIN_SEED_DIR="+run.seedDir,
		"HEALTH="+health,
		"FIXTURE="+fixture,
		"CALL_LOG="+run.logPath,
	)
	run.output, run.err = cmd.CombinedOutput()
	return run
}

func (r miniMaxEntrypointRun) calls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(r.logPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// AC-G5: the token stays in the sidecar. The entrypoint asks the loopback proxy
// for the GitHub token, clones the marketplace with a URL-scoped header, and
// hands the agent a seed whose K3s MCP server is the loopback proxy with no
// auth of its own and no field mcode refuses.
func TestEntrypointBootstrapsMiniMaxPluginThroughTheLoopbackProxy(t *testing.T) {
	run := runMiniMaxEntrypoint(t, true, miniMaxTestK3SServer)
	if run.err != nil {
		t.Fatalf("entrypoint: %v\n%s", run.err, run.output)
	}
	want := "curl:healthz\ncurl:mcp\ngit:clone\nagent:seed=" + run.seedDir + "\n"
	if got := run.calls(t); got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}

	var manifest map[string]any
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(run.seedDir, "mcp-servers", ".claude-plugin", "plugin.json"))), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["name"] != "mcp-servers" || manifest["version"] != "1.0.0" {
		t.Errorf("manifest = %v", manifest)
	}
	if servers, _ := manifest["mcpServers"].([]any); len(servers) != 1 || servers[0] != "./mcp/k3s.json" {
		t.Errorf("manifest mcpServers = %v", manifest["mcpServers"])
	}

	var config struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(run.seedDir, "mcp-servers", "mcp", "k3s.json"))), &config); err != nil {
		t.Fatal(err)
	}
	k3s := config.MCPServers["k3s"]
	if k3s["url"] != "http://127.0.0.1:8093/mcp" || k3s["type"] != "http" {
		t.Errorf("k3s server = %v, want the loopback proxy", k3s)
	}
	for _, field := range []string{"oauthClientId", "headers"} {
		if _, ok := k3s[field]; ok {
			t.Errorf("k3s server still carries %s: %v", field, k3s)
		}
	}
}

// AC-G5: no token in the Secret is a supported deployment, not a failure.
func TestEntrypointSkipsMiniMaxPluginWhenTheProxyHasNoToken(t *testing.T) {
	run := runMiniMaxEntrypoint(t, false, miniMaxTestK3SServer)
	if run.err != nil {
		t.Fatalf("entrypoint: %v\n%s", run.err, run.output)
	}
	if got := run.calls(t); got != "curl:healthz\nagent:seed=unset\n" {
		t.Fatalf("calls = %q, want only the health check, then the agent with no seed", got)
	}
	if _, err := os.Stat(run.seedDir); !os.IsNotExist(err) {
		t.Errorf("an unconfigured pod staged a seed: %v", err)
	}
}

// A server the proxy does not front would be routed nowhere useful — or, if
// its URL were rewritten anyway, sent to the K3s MCP with the K3s token.
func TestEntrypointRefusesAMiniMaxPluginServerTheProxyDoesNotFront(t *testing.T) {
	foreign := strings.Replace(miniMaxTestK3SServer, "https://homelab-k3s-mcp.llkm.nl/mcp", "https://other.example/mcp", 1)
	run := runMiniMaxEntrypoint(t, true, foreign)
	if run.err == nil {
		t.Fatalf("entrypoint accepted a foreign MCP server\n%s", run.output)
	}
	if strings.Contains(run.calls(t), "agent:") {
		t.Fatal("the agent started after a failed plugin bootstrap")
	}
}
