package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// claude-code reaches the K3s MCP the way codex does (AC-E6): through the
// loopback k3s-mcp-proxy sidecar, which alone holds the token and clones the
// marketplace for the plugins the session selected (AC-L1). This container has
// neither the token nor a GitHub credential, so the agent lays the sidecar's
// seed bundle out as a local marketplace under the platform's marketplace name
// and has the pinned CLI install from there into a plugin cache outside the
// archived HOME — the cache every turn then loads from. The K3s MCP itself is
// registered by the platform (the runner's --k3s-mcp-url), not by a plugin, so
// a plugin's own K3s server is left out of what is installed.
const (
	claudePluginCacheDirEnv = "CLAUDE_CODE_PLUGIN_CACHE_DIR"
	// claudeBinEnv names the CLI the runner drives; the install uses the same.
	claudeBinEnv = "CLAUDE_CODE_BIN"
	// claudePluginWorkRoot holds the staged marketplace, the install's own
	// HOME and the plugin cache. It is outside the state tree, so nothing in it
	// is archived and a restore can never bring back another pod's plugins.
	claudePluginWorkRoot       = "/tmp/session-platform-claude-plugins"
	claudePluginInstallTimeout = 2 * time.Minute
)

// claudePlatform is what the K3s MCP proxy sidecar gave this pod. It is
// prepared once per process: boot and a later restore share one sidecar.
type claudePlatform struct {
	proxyURL string
	workRoot string
	// probe reads the sidecar; a field so tests need no server.
	probe func(proxyURL string) (codexSidecar, error)
	// install runs the CLI's marketplace add and plugin install.
	install func(marketplaceDir, home, cacheDir string, plugins []string) error
	// setenv exports the plugin cache to the turns this agent starts.
	setenv func(key, value string) error
	logger *slog.Logger

	once      sync.Once
	k3sMCPURL string
	installed map[string]bool
}

func newClaudePlatform(proxyURL string, logger *slog.Logger) *claudePlatform {
	return &claudePlatform{
		proxyURL: proxyURL,
		workRoot: claudePluginWorkRoot,
		probe: func(proxyURL string) (codexSidecar, error) {
			return fetchCodexSidecar(proxyURL, codexSidecarWait)
		},
		install: installClaudePlugins,
		setenv:  os.Setenv,
		logger:  logger,
	}
}

// claudePlatformFromEnv is AC-E6's placement rule for the K3s MCP: the proxy
// is a sidecar in this pod, reached on loopback, and this container never
// holds the token it injects.
func claudePlatformFromEnv(logger *slog.Logger) (*claudePlatform, error) {
	if os.Getenv(k3sMCPTokenEnv) != "" {
		return nil, fmt.Errorf("claude-code container must not receive %s; the K3s MCP proxy sidecar holds it", k3sMCPTokenEnv)
	}
	proxy := os.Getenv(k3sMCPProxyURLEnv)
	if err := validateLoopbackSidecarEndpoint(proxy, k3sMCPProxyPort); err != nil {
		return nil, fmt.Errorf("claude-code K3s MCP proxy endpoint: %w", err)
	}
	return newClaudePlatform(proxy, logger), nil
}

func (p *claudePlatform) log() *slog.Logger {
	if p.logger == nil {
		return slog.Default()
	}
	return p.logger
}

// prepare reads the sidecar and installs the selected plugins it seeded. What
// the sidecar cannot deliver is left out rather than failing the pod, as for
// codex: no token means no K3s MCP and no plugins, which is a session.
func (p *claudePlatform) prepare(selected []string) {
	p.once.Do(func() {
		p.k3sMCPURL, p.installed = p.load(selected)
	})
}

// mcpURL is the sidecar's K3s MCP endpoint, or "" when it holds no token.
func (p *claudePlatform) mcpURL() string {
	if p == nil {
		return ""
	}
	return p.k3sMCPURL
}

// enabled keeps the selected plugins this pod actually installed, so the
// managed settings never enable one the CLI cannot resolve.
func (p *claudePlatform) enabled(selected []string) []string {
	var out []string
	for _, plugin := range selected {
		if p.installed[plugin] {
			out = append(out, plugin)
		}
	}
	return out
}

func (p *claudePlatform) load(selected []string) (string, map[string]bool) {
	sidecar, err := p.probe(p.proxyURL)
	if err != nil {
		p.log().Warn("claude-code K3s MCP sidecar did not deliver everything; running without it",
			"configured", sidecar.configured, "plugins", len(sidecar.plugins), "err", err)
	}
	var mcpURL string
	if sidecar.configured {
		if mcpURL, err = k3sProxyMCPURL(p.proxyURL); err != nil {
			p.log().Warn("claude-code K3s MCP proxy URL is unusable; running without the K3s MCP", "err", err)
			mcpURL = ""
		}
	}
	if len(selected) == 0 {
		return mcpURL, nil
	}
	seeded := map[string]miniMaxSeedPlugin{}
	for _, plugin := range sidecar.plugins {
		seeded[plugin.Name+"@"+claudePluginMarketplace] = plugin
	}
	var bundle []miniMaxSeedPlugin
	var names []string
	for _, plugin := range selected {
		seed, ok := seeded[plugin]
		if !ok {
			p.log().Warn("selected plugin is not in the sidecar's seed; running without it", "plugin", plugin)
			continue
		}
		bundle = append(bundle, seed)
		names = append(names, plugin)
	}
	if len(names) == 0 {
		return mcpURL, nil
	}
	marketplace := filepath.Join(p.workRoot, "marketplace")
	cache := filepath.Join(p.workRoot, "cache")
	home := filepath.Join(p.workRoot, "home")
	upstream := env(k3sMCPURLEnv, defaultK3SMCPURL)
	if err := stageClaudeMarketplace(marketplace, bundle, upstream); err != nil {
		p.log().Warn("could not stage the claude-code plugin marketplace; running without plugins", "err", err)
		return mcpURL, nil
	}
	if err := p.install(marketplace, home, cache, names); err != nil {
		p.log().Warn("could not install the claude-code plugins; running without them", "err", err)
		return mcpURL, nil
	}
	if err := p.setenv(claudePluginCacheDirEnv, cache); err != nil {
		p.log().Warn("could not export the claude-code plugin cache; running without plugins", "err", err)
		return mcpURL, nil
	}
	installed := make(map[string]bool, len(names))
	for _, name := range names {
		installed[name] = true
	}
	p.log().Info("claude-code plugins installed from the sidecar seed", "plugins", names, "revision", sidecar.revision)
	return mcpURL, installed
}

// stageClaudeMarketplace writes the seeded plugins under dir as one local
// marketplace named like the platform's own, so the plugin ids stay
// "<name>@dlddu-plugins" whichever host served the clone.
func stageClaudeMarketplace(dir string, plugins []miniMaxSeedPlugin, k3sUpstream string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear claude marketplace: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o700); err != nil {
		return fmt.Errorf("create claude marketplace: %w", err)
	}
	entries := make([]map[string]string, 0, len(plugins))
	for _, plugin := range plugins {
		if err := writeMiniMaxSeedPlugin(dir, plugin); err != nil {
			return err
		}
		if err := dropK3SPluginServers(filepath.Join(dir, plugin.Name), k3sUpstream); err != nil {
			return fmt.Errorf("seed plugin %s: %w", plugin.Name, err)
		}
		entries = append(entries, map[string]string{"name": plugin.Name, "source": "./" + plugin.Name})
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"name":    claudePluginMarketplace,
		"owner":   map[string]string{"name": "session-platform"},
		"plugins": entries,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"), append(manifest, '\n'), 0o600)
}

// dropK3SPluginServers removes the K3s MCP server from a staged plugin's MCP
// files: the platform registers that server itself, and the plugin's copy
// names the upstream with a token header this container has no value for.
// Every other server is kept as the marketplace declared it.
func dropK3SPluginServers(pluginDir, k3sUpstream string) error {
	manifestPath := filepath.Join(pluginDir, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	declared, ok := manifest["mcpServers"]
	if !ok {
		return nil
	}
	var files []string
	var one string
	if json.Unmarshal(declared, &one) == nil {
		files = []string{one}
	} else if err := json.Unmarshal(declared, &files); err != nil {
		return errors.New("mcpServers must be file paths")
	}
	var kept []string
	for _, rel := range files {
		file, err := insideTree(pluginDir, filepath.FromSlash(rel))
		if err != nil {
			return fmt.Errorf("mcpServers %s: %w", rel, err)
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(content, &doc); err != nil {
			return fmt.Errorf("mcpServers %s: parse: %w", rel, err)
		}
		servers := map[string]json.RawMessage{}
		if raw, ok := doc["mcpServers"]; ok {
			if err := json.Unmarshal(raw, &servers); err != nil {
				return fmt.Errorf("mcpServers %s: parse servers: %w", rel, err)
			}
		}
		for name, raw := range servers {
			var server struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(raw, &server) == nil && server.URL != "" &&
				strings.TrimRight(server.URL, "/") == strings.TrimRight(k3sUpstream, "/") {
				delete(servers, name)
			}
		}
		if len(servers) == 0 {
			if err := os.Remove(file); err != nil {
				return err
			}
			continue
		}
		encodedServers, err := json.Marshal(servers)
		if err != nil {
			return err
		}
		doc["mcpServers"] = encodedServers
		rewritten, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(file, append(rewritten, '\n'), 0o600); err != nil {
			return err
		}
		kept = append(kept, rel)
	}
	if len(kept) == 0 {
		delete(manifest, "mcpServers")
	} else {
		encoded, err := json.Marshal(kept)
		if err != nil {
			return err
		}
		manifest["mcpServers"] = encoded
	}
	rewritten, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(rewritten, '\n'), 0o600)
}

// installClaudePlugins is the CLI half of the install: the same two commands
// the image's entrypoint once ran against the remote marketplace, now against
// the staged directory. The install gets a HOME of its own so the session
// HOME is never touched; the turns find the plugins through the cache.
func installClaudePlugins(marketplaceDir, home, cacheDir string, plugins []string) error {
	for _, dir := range []string{home, cacheDir} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	bin := env(claudeBinEnv, "claude")
	run := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), claudePluginInstallTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = home
		cmd.Env = append(environmentWithHome(home), claudePluginCacheDirEnv+"="+cacheDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run("plugin", "marketplace", "add", marketplaceDir); err != nil {
		return err
	}
	for _, plugin := range plugins {
		if err := run("plugin", "install", plugin); err != nil {
			return err
		}
	}
	return nil
}
