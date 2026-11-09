package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The minimax-code workload pod cannot reach github.com (AC-G6), so its
// plugin-marketplace bootstrap runs in the helper pod's k3s-mcp-proxy
// container, which already holds the K3s MCP token and whose egress is open
// (AC-G5). The container clones the marketplace once at start and serves the
// selected plugins at miniMaxPluginSeedPath as a bundle with no secret in it;
// the workload agent installs that bundle and points the K3s MCP server at
// this proxy. A POST to miniMaxPluginRefreshPath clones it again on request
// (AC-J7) — nothing re-clones on its own.
const (
	miniMaxPluginSeedPath    = "/plugin-seed"
	miniMaxPluginRefreshPath = "/plugin-seed/refresh"

	// Keep in sync with control-plane/internal/adapter/k8s
	// (MiniMaxMarketplaceDirEnvVar and the env it may pass through).
	miniMaxMarketplaceURLEnv = "MINIMAX_CODE_PLUGIN_MARKETPLACE_URL"
	miniMaxPluginsEnv        = "MINIMAX_CODE_PLUGINS"
	miniMaxMarketplaceDirEnv = "MINIMAX_CODE_MARKETPLACE_DIR"

	defaultMiniMaxMarketplaceURL = "https://github.com/dlddu/plugin-marketplace.git"
	defaultMiniMaxPlugins        = "mcp-servers"

	miniMaxSeedStateBootstrapping = "bootstrapping"
	miniMaxSeedStateReady         = "ready"
	miniMaxSeedStateFailed        = "failed"
	miniMaxSeedStateUnconfigured  = "unconfigured"

	// A plugin bundle is instructions and small config files. The bounds keep
	// a mistaken marketplace entry (a source of "./" listing a whole tree) from
	// turning the seed route into a bulk copy.
	miniMaxSeedMaxBytes = 8 << 20
	miniMaxSeedMaxFiles = 2000

	miniMaxBootstrapAttempts = 3
	miniMaxBootstrapBackoff  = 10 * time.Second
	miniMaxCloneTimeout      = 2 * time.Minute
)

// miniMaxPluginPathFields are the manifest fields that name files or
// directories inside a plugin's source. Hooks are left out on purpose: a hook
// is a command the CLI runs on its own, and this bundle only carries what the
// model or the CLI reads.
var miniMaxPluginPathFields = []string{"skills", "commands", "agents", "mcpServers"}

// miniMaxSeedBundle is the wire shape of miniMaxPluginSeedPath. Files are keyed
// by their slash-separated path inside the plugin and carried as base64.
type miniMaxSeedBundle struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Revision is the marketplace commit the plugins were read from, when git
	// could tell.
	Revision string              `json:"revision,omitempty"`
	Plugins  []miniMaxSeedPlugin `json:"plugins"`
}

// miniMaxRefreshResult is the wire shape of miniMaxPluginRefreshPath: the
// bundle's state and revision after the request, without its files.
type miniMaxRefreshResult struct {
	State    string `json:"state"`
	Revision string `json:"revision,omitempty"`
	Error    string `json:"error,omitempty"`
}

var (
	errMiniMaxSeedBootstrapping = errors.New("plugin marketplace is still on its first clone")
	errMiniMaxRefreshRunning    = errors.New("a plugin marketplace refresh is already running")
)

type miniMaxSeedPlugin struct {
	Name  string            `json:"name"`
	Files map[string]string `json:"files"`
}

// miniMaxMarketplaceSeeder runs the bootstrap and holds its result.
type miniMaxMarketplaceSeeder struct {
	marketplaceURL string
	plugins        []string
	workDir        string
	logger         *slog.Logger

	// githubToken asks the K3s MCP for a short-lived marketplace read token.
	githubToken func(context.Context) (string, error)
	// clone fetches the marketplace into dir with the given auth header.
	clone func(ctx context.Context, url, authHeader, dir string) error
	// revision reads the commit a clone checked out.
	revision func(ctx context.Context, dir string) (string, error)
	backoff  time.Duration

	// refreshing serialises refreshes; one that finds it held is refused.
	refreshing sync.Mutex

	mu     sync.Mutex
	bundle miniMaxSeedBundle
}

func newMiniMaxMarketplaceSeeder(upstream http.Handler, mcpPath string, logger *slog.Logger) *miniMaxMarketplaceSeeder {
	if logger == nil {
		logger = slog.Default()
	}
	s := &miniMaxMarketplaceSeeder{
		marketplaceURL: env(miniMaxMarketplaceURLEnv, defaultMiniMaxMarketplaceURL),
		plugins:        splitPluginList(env(miniMaxPluginsEnv, defaultMiniMaxPlugins)),
		workDir:        os.Getenv(miniMaxMarketplaceDirEnv),
		logger:         logger,
		clone:          gitCloneMarketplace,
		revision:       gitCloneRevision,
		backoff:        miniMaxBootstrapBackoff,
		bundle:         miniMaxSeedBundle{State: miniMaxSeedStateBootstrapping, Plugins: []miniMaxSeedPlugin{}},
	}
	s.githubToken = func(ctx context.Context) (string, error) {
		return githubTokenThroughProxy(ctx, upstream, mcpPath)
	}
	return s
}

func splitPluginList(raw string) []string {
	var names []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// run bootstraps with a few attempts; the last failure is what the route
// reports. It never exits the process: a broken marketplace costs the session
// its plugins, not its proxy.
func (s *miniMaxMarketplaceSeeder) run(ctx context.Context) {
	var err error
	for attempt := 0; attempt < miniMaxBootstrapAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.backoff):
			}
		}
		var bundle miniMaxSeedBundle
		if bundle, err = s.bootstrap(ctx); err == nil {
			s.set(bundle)
			s.logger.Info("plugin marketplace staged", "plugins", len(bundle.Plugins), "revision", bundle.Revision)
			return
		}
		s.logger.Warn("minimax plugin marketplace bootstrap failed", "attempt", attempt+1, "err", err)
	}
	s.set(miniMaxSeedBundle{State: miniMaxSeedStateFailed, Error: err.Error(), Plugins: []miniMaxSeedPlugin{}})
}

func (s *miniMaxMarketplaceSeeder) set(bundle miniMaxSeedBundle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundle = bundle
}

func (s *miniMaxMarketplaceSeeder) current() miniMaxSeedBundle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundle
}

// bootstrap is one clone, read into a ready bundle.
func (s *miniMaxMarketplaceSeeder) bootstrap(ctx context.Context) (miniMaxSeedBundle, error) {
	token, err := s.githubToken(ctx)
	if err != nil {
		return miniMaxSeedBundle{}, err
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	if err := os.MkdirAll(s.workDir, 0o700); err != nil {
		return miniMaxSeedBundle{}, fmt.Errorf("create marketplace work dir: %w", err)
	}
	dir, err := os.MkdirTemp(s.workDir, "clone-")
	if err != nil {
		return miniMaxSeedBundle{}, fmt.Errorf("create marketplace clone dir: %w", err)
	}
	defer os.RemoveAll(dir)
	cloneCtx, cancel := context.WithTimeout(ctx, miniMaxCloneTimeout)
	defer cancel()
	if err := s.clone(cloneCtx, s.marketplaceURL, header, dir); err != nil {
		return miniMaxSeedBundle{}, err
	}
	plugins, err := buildMiniMaxSeed(dir, s.plugins)
	if err != nil {
		return miniMaxSeedBundle{}, err
	}
	revision, err := s.revision(cloneCtx, dir)
	if err != nil {
		// The revision names what was read; the plugins are what matters.
		s.logger.Warn("could not read the plugin marketplace revision", "err", err)
	}
	return miniMaxSeedBundle{State: miniMaxSeedStateReady, Revision: revision, Plugins: plugins}, nil
}

// refresh clones the marketplace once more and serves what it read (AC-J7).
// A failed refresh leaves the bundle being served as it was, so a session never
// loses skills it had to a marketplace it cannot reach right now. It is refused
// while the first clone is still running and while another refresh is.
func (s *miniMaxMarketplaceSeeder) refresh(ctx context.Context) (miniMaxSeedBundle, error) {
	if s.current().State == miniMaxSeedStateBootstrapping {
		return s.current(), errMiniMaxSeedBootstrapping
	}
	if !s.refreshing.TryLock() {
		return s.current(), errMiniMaxRefreshRunning
	}
	defer s.refreshing.Unlock()
	bundle, err := s.bootstrap(ctx)
	if err != nil {
		s.logger.Warn("plugin marketplace refresh failed; keeping the plugins already staged", "err", err)
		return s.current(), err
	}
	s.set(bundle)
	s.logger.Info("plugin marketplace refreshed", "plugins", len(bundle.Plugins), "revision", bundle.Revision)
	return bundle, nil
}

func (s *miniMaxMarketplaceSeeder) serveRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	bundle, err := s.refresh(r.Context())
	result := miniMaxRefreshResult{State: bundle.State, Revision: bundle.Revision}
	status := http.StatusOK
	switch {
	case errors.Is(err, errMiniMaxSeedBootstrapping), errors.Is(err, errMiniMaxRefreshRunning):
		status = http.StatusConflict
		result.Error = err.Error()
	case err != nil:
		status = http.StatusBadGateway
		result.Error = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}

func (s *miniMaxMarketplaceSeeder) serveSeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	bundle := s.bundle
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if bundle.State == miniMaxSeedStateBootstrapping {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(bundle)
}

// serveUnconfiguredSeed answers the route for a proxy without a token or
// without a marketplace directory: there is no marketplace to read, which is a
// state, not an outage.
func serveUnconfiguredSeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(miniMaxSeedBundle{State: miniMaxSeedStateUnconfigured, Plugins: []miniMaxSeedPlugin{}})
}

// serveUnconfiguredRefresh answers a refresh with nothing to refresh: the
// state, not an error, as for the seed itself.
func serveUnconfiguredRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(miniMaxRefreshResult{State: miniMaxSeedStateUnconfigured})
}

// githubTokenThroughProxy calls the K3s MCP's github_app_installation_token
// through the proxy's own upstream handler, in process. The bootstrap never
// handles the K3s MCP token; the proxy injects it exactly as it does for the
// workload's calls.
func githubTokenThroughProxy(ctx context.Context, upstream http.Handler, mcpPath string) (string, error) {
	body, err := githubTokenCallBody([]string{"plugin-marketplace"}, githubContentsRead)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := newBufferedResponse()
	upstream.ServeHTTP(rec, req)
	if rec.status != http.StatusOK {
		return "", fmt.Errorf("K3s MCP GitHub token request returned status %d", rec.status)
	}
	return parseGitHubTokenResponse(rec.body.Bytes())
}

const githubTokenTool = "github_app_installation_token"

// The two `contents` levels a token is ever asked for. Write exists for
// `git_push` alone (session_mcp_git_push.go); every other caller reads.
const (
	githubContentsRead  = "read"
	githubContentsWrite = "write"
)

func githubTokenCallBody(repositories []string, contents string) ([]byte, error) {
	if contents != githubContentsRead && contents != githubContentsWrite {
		return nil, fmt.Errorf("unsupported contents permission %q", contents)
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": githubTokenTool,
			"arguments": map[string]any{
				"repositories": repositories,
				"permissions":  map[string]string{"contents": contents},
			},
		},
	})
}

func parseGitHubTokenResponse(body []byte) (string, error) {
	var resp struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type     string `json:"type"`
				Resource struct {
					Text string `json:"text"`
				} `json:"resource"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", errors.New("K3s MCP GitHub token response is not JSON")
	}
	if len(resp.Error) > 0 && string(resp.Error) != "null" {
		return "", errors.New("K3s MCP GitHub token request failed")
	}
	if resp.Result.IsError {
		return "", errors.New("K3s MCP GitHub token tool returned an error")
	}
	for _, item := range resp.Result.Content {
		if item.Type != "resource" {
			continue
		}
		for _, line := range strings.Split(item.Resource.Text, "\n") {
			if token, ok := strings.CutPrefix(line, "GITHUB_TOKEN="); ok && token != "" {
				return token, nil
			}
		}
	}
	return "", errors.New("K3s MCP response omitted GITHUB_TOKEN")
}

// bufferedResponse is the minimal ResponseWriter for an in-process call.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: http.Header{}, status: http.StatusOK}
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

// gitCloneMarketplace is the same scoped-header clone the claude-code
// entrypoint does: the header is scoped to the marketplace URL, process-only,
// with every credential helper cleared and prompting off.
func gitCloneMarketplace(ctx context.Context, url, authHeader, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "-c", "credential.helper=",
		"clone", "--quiet", "--depth", "1", "--", url, dir)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http."+url+".extraheader",
		"GIT_CONFIG_VALUE_0="+authHeader,
		"GIT_TERMINAL_PROMPT=0",
		"HOME="+filepath.Dir(dir),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(strings.ReplaceAll(stderr.String(), authHeader, redactedLiteral))
		return fmt.Errorf("clone plugin marketplace: %w: %s", err, msg)
	}
	return nil
}

// gitCloneRevision is the commit a clone checked out.
func gitCloneRevision(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	cmd.Env = append(os.Environ(), "HOME="+filepath.Dir(dir))
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read marketplace revision: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// buildMiniMaxSeed reads the selected plugins out of a marketplace clone. Every
// named plugin must exist; each carries its manifest and the files its path
// fields name, read from inside its source and nowhere else.
func buildMiniMaxSeed(clone string, names []string) ([]miniMaxSeedPlugin, error) {
	data, err := os.ReadFile(filepath.Join(clone, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, fmt.Errorf("read marketplace manifest: %w", err)
	}
	var marketplace struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(data, &marketplace); err != nil {
		return nil, fmt.Errorf("parse marketplace manifest: %w", err)
	}
	budget := &miniMaxSeedBudget{bytes: miniMaxSeedMaxBytes, files: miniMaxSeedMaxFiles}
	plugins := make([]miniMaxSeedPlugin, 0, len(names))
	for _, name := range names {
		var entry map[string]any
		for _, candidate := range marketplace.Plugins {
			if candidate["name"] == name {
				if entry != nil {
					return nil, fmt.Errorf("marketplace lists plugin %s more than once", name)
				}
				entry = candidate
			}
		}
		if entry == nil {
			return nil, fmt.Errorf("marketplace has no plugin named %s", name)
		}
		plugin, err := seedMiniMaxPlugin(clone, name, entry, budget)
		if err != nil {
			return nil, fmt.Errorf("plugin %s: %w", name, err)
		}
		plugins = append(plugins, plugin)
	}
	return plugins, nil
}

type miniMaxSeedBudget struct{ bytes, files int }

func seedMiniMaxPlugin(clone, name string, entry map[string]any, budget *miniMaxSeedBudget) (miniMaxSeedPlugin, error) {
	if !validMiniMaxPluginName(name) {
		return miniMaxSeedPlugin{}, errors.New("plugin name is not a single path segment")
	}
	source, ok := entry["source"].(string)
	if !ok {
		return miniMaxSeedPlugin{}, errors.New("source must be a path in the marketplace")
	}
	sourceDir, err := insideTree(clone, source)
	if err != nil {
		return miniMaxSeedPlugin{}, fmt.Errorf("source: %w", err)
	}
	manifest := map[string]any{
		"name":        name,
		"description": stringOr(entry["description"], ""),
		"version":     stringOr(entry["version"], "1.0.0"),
		"author":      entry["author"],
	}
	if manifest["author"] == nil {
		manifest["author"] = map[string]any{}
	}
	files := map[string]string{}
	for _, field := range miniMaxPluginPathFields {
		paths, err := pathList(entry[field])
		if err != nil {
			return miniMaxSeedPlugin{}, fmt.Errorf("%s: %w", field, err)
		}
		if paths == nil {
			continue
		}
		manifest[field] = paths
		for _, p := range paths {
			if err := collectSeedFiles(sourceDir, p, files, budget); err != nil {
				return miniMaxSeedPlugin{}, fmt.Errorf("%s %s: %w", field, p, err)
			}
		}
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return miniMaxSeedPlugin{}, err
	}
	files[".claude-plugin/plugin.json"] = base64.StdEncoding.EncodeToString(append(encoded, '\n'))
	return miniMaxSeedPlugin{Name: name, Files: files}, nil
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

// pathList accepts a manifest path field as one string or a list of strings.
func pathList(v any) ([]string, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{value}, nil
	case []any:
		paths := make([]string, 0, len(value))
		for _, item := range value {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("must be paths")
			}
			paths = append(paths, s)
		}
		return paths, nil
	default:
		return nil, errors.New("must be a path or a list of paths")
	}
}

// insideTree resolves rel under root and refuses anything that leaves it,
// through ".." or an absolute path. Symlinks are refused where files are read.
func insideTree(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", errors.New("must be relative")
	}
	joined := filepath.Join(root, rel)
	back, err := filepath.Rel(root, joined)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", errors.New("escapes the marketplace")
	}
	return joined, nil
}

func collectSeedFiles(sourceDir, rel string, files map[string]string, budget *miniMaxSeedBudget) error {
	start, err := insideTree(sourceDir, rel)
	if err != nil {
		return err
	}
	return filepath.WalkDir(start, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", entry.Name())
		}
		inside, err := filepath.Rel(sourceDir, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(inside)
		if _, done := files[key]; done {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		budget.files--
		budget.bytes -= int(info.Size())
		if budget.files < 0 || budget.bytes < 0 {
			return errors.New("plugin bundle exceeds the seed bounds")
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[key] = base64.StdEncoding.EncodeToString(content)
		return nil
	})
}

func validMiniMaxPluginName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && path.Base(name) == name
}

// writeMiniMaxSeedPlugin writes one bundled plugin under seedDir, refusing any
// file path that would land outside its own directory.
func writeMiniMaxSeedPlugin(seedDir string, plugin miniMaxSeedPlugin) error {
	if !validMiniMaxPluginName(plugin.Name) {
		return fmt.Errorf("seed plugin name %q is not a single path segment", plugin.Name)
	}
	root := filepath.Join(seedDir, plugin.Name)
	for rel, encoded := range plugin.Files {
		target, err := insideTree(root, filepath.FromSlash(rel))
		if err != nil || target == root {
			return fmt.Errorf("seed plugin %s file %q: invalid path", plugin.Name, rel)
		}
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("seed plugin %s file %q: %w", plugin.Name, rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func decodeMiniMaxSeedBundle(r io.Reader) (miniMaxSeedBundle, error) {
	var bundle miniMaxSeedBundle
	// Base64 inflates by a third; the cap leaves room for the JSON around it.
	if err := json.NewDecoder(io.LimitReader(r, 2*miniMaxSeedMaxBytes)).Decode(&bundle); err != nil {
		return miniMaxSeedBundle{}, fmt.Errorf("decode plugin seed: %w", err)
	}
	return bundle, nil
}
