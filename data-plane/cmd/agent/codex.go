package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The codex workload (docs/prd/codex-workload.md, AC-J1~J6) runs the Codex SDK
// through a small Node runner, codex-runner (data-plane/codex-runner), on the
// claude-code runner (AC-E2~E5): queue, bounds, cursor, archive and resume flag
// are claudeWorkload's. This file is the CLI-specific half; the managed config
// is codex_config.go's, the plugin skills codex_skills.go's, the skills MCP
// codex_skills_mcp.go's and the output projection codex_stream.go's.
//
// There is no provider credential to keep out of this pod: the session signs
// in to ChatGPT itself, through a device code the runner forwards (AC-J3), and
// that login lives in $HOME/.codex, which is archived with the rest of HOME.
// The only platform secret in the pod is the K3s MCP token, and it stays in
// the loopback k3s-mcp-proxy sidecar (AC-J6).
const (
	workloadCodex = "codex"

	// Keep in sync with control-plane/internal/adapter/k8s (CodexModelEnvVar).
	codexModelEnv            = "CODEX_MODEL"
	codexRunnerBinaryEnv     = "CODEX_RUNNER_BIN"
	defaultCodexRunnerBinary = "/usr/local/bin/codex-runner"

	// codexSkillsDirEnv tells the skills MCP which directory it serves.
	codexSkillsDirEnv = "CODEX_SKILLS_DIR"
	// The agent binary doubles as two helpers, selected by its first argument
	// (runAgentSubcommand): the stdio skills MCP that codex spawns, and the
	// config renderer the image build runs its surface check against.
	codexSkillsMCPCommand    = "skills-mcp"
	codexRenderConfigCommand = "codex-render-config"
	defaultCodexAgentBinary  = "/agent"

	codexK3SServerName    = "k3s"
	codexSkillsServerName = "skills"

	// The sidecar starts with this container, not before it, and its
	// marketplace clone may still be running; the wait covers both. Past it
	// the session runs without what the sidecar did not deliver (AC-J5).
	codexSidecarWait = 90 * time.Second
)

// codexCLI is the agentCLI for codex-runner. It takes no provider address and
// no key: the runner's codex child signs in with the session's own ChatGPT
// account (AC-J3).
type codexCLI struct {
	// k3sProxyURL is the loopback k3s-mcp-proxy sidecar (AC-J6).
	k3sProxyURL string
	// agentBinary is what codex spawns as the skills MCP.
	agentBinary string
	// probeSidecar reads what the sidecar offers; a field so tests need no
	// server. It reports what it learned even when it also returns an error.
	probeSidecar func(proxyURL string) (codexSidecar, error)
	logger       *slog.Logger
}

func newCodexCLI(k3sProxyURL string, logger *slog.Logger) codexCLI {
	agentBinary := defaultCodexAgentBinary
	if exe, err := os.Executable(); err == nil && exe != "" {
		agentBinary = exe
	}
	return codexCLI{k3sProxyURL: k3sProxyURL, agentBinary: agentBinary, logger: logger}
}

func (codexCLI) workloadName() string { return workloadCodex }

// argv is one runner invocation (AC-J2). The model is always passed, the
// platform-default sentinel included, so the runner — not this side — decides
// what an unset model means. The prompt follows "--", so a prompt that starts
// with "--" can never change a platform-controlled flag. --resume follows
// AC-E4: only after the first successful run.
func (codexCLI) argv(binary, model, prompt string, resume bool) []string {
	if model == "" {
		model = platformDefaultModel
	}
	argv := []string{binary, "--model", model}
	if resume {
		argv = append(argv, "--resume")
	}
	return append(argv, "--", prompt)
}

func (codexCLI) newProjector(dst io.Writer) io.WriteCloser {
	return newCodexStreamProjector(dst)
}

// ensureHome makes the platform-owned parts of HOME match this pod: the
// plugin skills the sidecar seeds, and a config.toml naming exactly the tool
// surface AC-J5 allows. Both are redone on every boot and restore, so an
// archive can never bring back a previous pod's skills or MCP servers. What
// the sidecar cannot deliver in time is left out rather than failing the pod.
func (c codexCLI) ensureHome(homeDir string, _ toolSurface) error {
	sidecar := c.sidecar()
	skillsDir := codexSkillsDir(homeDir)
	skills := codexSkillsFromSeed(sidecar.plugins, c.log())
	if err := syncCodexSkills(skillsDir, skills, c.log()); err != nil {
		return err
	}
	cfg := codexConfig{
		agentBinary: c.agentBinary,
		skillsDir:   skillsDir,
	}
	if sidecar.configured {
		mcpURL, err := k3sProxyMCPURL(c.k3sProxyURL)
		if err != nil {
			return err
		}
		cfg.k3sMCPURL = mcpURL
	}
	return writeCodexConfig(filepath.Join(homeDir, codexConfigDirName, codexConfigFileName), renderCodexConfig(cfg))
}

// sidecar asks the K3s MCP proxy what it has. No proxy means no K3s MCP and
// no plugin skills, which is a session, not a failure (AC-J5, AC-J6).
func (c codexCLI) sidecar() codexSidecar {
	if c.k3sProxyURL == "" {
		return codexSidecar{}
	}
	probe := c.probeSidecar
	if probe == nil {
		probe = func(proxyURL string) (codexSidecar, error) {
			return fetchCodexSidecar(proxyURL, codexSidecarWait)
		}
	}
	sidecar, err := probe(c.k3sProxyURL)
	if err != nil {
		c.log().Warn("codex K3s MCP sidecar did not deliver everything; running without it",
			"configured", sidecar.configured, "plugins", len(sidecar.plugins), "err", err)
	}
	return sidecar
}

func (c codexCLI) log() *slog.Logger {
	if c.logger == nil {
		return slog.Default()
	}
	return c.logger
}

// validateState is the type-neutral integrity check (AC-E5), after dropping
// codex's per-process helper links. codex links its own binary into
// $CODEX_HOME/tmp/arg0-linux on start and removes the links on exit, but a run
// the agent killed (timeout, kill switch) leaves them behind, and they are
// absolute links the archive refuses. This is the one hook between a drained
// run and the archive; on the restore path the archive's own symlink check has
// already run, so there is nothing for it to drop.
func (codexCLI) validateState(root string) error {
	if err := os.RemoveAll(filepath.Join(root, "home", codexConfigDirName, "tmp", "arg0-linux")); err != nil {
		return fmt.Errorf("drop codex helper links: %w", err)
	}
	return validateRestoredAgentState(root)
}

// codexCredentialEnv is every credential variable codex (0.166.0), the Claude
// CLI in the same image, or any CLI the platform ships could pick up on its
// own. The control plane injects none of them; refusing to start on any is
// defence in depth, and the runner hands its codex child a fixed minimal
// environment besides (AC-J3). The same list feeds output redaction.
var codexCredentialEnv = append([]string{
	"ANTHROPIC_AUTH_TOKEN",
	"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_GITHUB_PERSONAL_ACCESS_TOKEN",
	"AZURE_OPENAI_API_KEY", "GITHUB_TOKEN", "GH_TOKEN",
}, miniMaxCredentialEnv...)

// validateCodexClientEnv is AC-J6's placement rule: the K3s MCP proxy is a
// sidecar in this pod, so its address must be loopback on its own port, and
// the workload container holds no credential under any name a CLI would read.
func validateCodexClientEnv() (string, error) {
	proxy := os.Getenv(k3sMCPProxyURLEnv)
	if err := validateLoopbackSidecarEndpoint(proxy, k3sMCPProxyPort); err != nil {
		return "", fmt.Errorf("codex K3s MCP proxy endpoint: %w", err)
	}
	for _, name := range codexCredentialEnv {
		if os.Getenv(name) != "" {
			return "", fmt.Errorf("codex container must not receive %s", name)
		}
	}
	return proxy, nil
}

// validateLoopbackSidecarEndpoint is validateHelperEndpoint's mirror image: a
// sidecar is reached on loopback, and a pod-network address would mean the
// token holder had been moved out of this pod, where anything on the network
// could reach it.
func validateLoopbackSidecarEndpoint(raw string, wantPort int) error {
	if raw == "" {
		return errors.New("must be set")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a URL")
	}
	if parsed.Scheme != "http" || parsed.User != nil {
		return errors.New("must be a plain http URL")
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return errors.New("must include a host and port")
	}
	if port, err := strconv.Atoi(portText); err != nil || port != wantPort {
		return fmt.Errorf("must use port %d", wantPort)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("must be a loopback IP; the proxy is a sidecar in this pod")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be a bare host and port; its clients append their own paths")
	}
	return nil
}

// newCodexWorkloadFromEnv builds the codex agent from the pod environment the
// control plane injects (AC-J1, AC-J6).
func newCodexWorkloadFromEnv(logger *slog.Logger, restoreMode bool) (*claudeWorkload, error) {
	proxy, err := validateCodexClientEnv()
	if err != nil {
		return nil, err
	}
	runTimeout, err := durationEnv("CLAUDE_CODE_RUN_TIMEOUT", defaultClaudeRunTimeout)
	if err != nil {
		return nil, err
	}
	cli := newCodexCLI(proxy, logger)
	return newClaudeWorkload(claudeConfig{
		StateDir:    env("CLAUDE_CODE_STATE_DIR", defaultClaudeStateDir),
		Model:       env(codexModelEnv, platformDefaultModel),
		Binary:      env(codexRunnerBinaryEnv, defaultCodexRunnerBinary),
		RestoreMode: restoreMode,
		Runner:      execCommandRunner{},
		Logger:      logger,
		Redact:      codexRedactionLiterals(),
		RunTimeout:  runTimeout,
		CLI:         cli,
	})
}

func codexRedactionLiterals() []string {
	literals := credentialLiteralsFromEnv()
	for _, name := range codexCredentialEnv {
		literals = append(literals, os.Getenv(name))
	}
	return normaliseRedactionLiterals(literals)
}

// runAgentSubcommand runs the helper modes selected by the first argument and
// reports whether it handled one. Neither starts the HTTP agent.
func runAgentSubcommand(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case codexSkillsMCPCommand:
		logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
		if err := serveCodexSkillsMCP(stdin, stdout, os.Getenv(codexSkillsDirEnv), logger); err != nil {
			fmt.Fprintln(stderr, "skills-mcp:", err)
			return 1, true
		}
		return 0, true
	case codexRenderConfigCommand:
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			fmt.Fprintln(stderr, "usage: agent codex-render-config <home>")
			return 2, true
		}
		if err := renderCodexConfigForHome(args[1]); err != nil {
			fmt.Fprintln(stderr, "codex-render-config:", err)
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// renderCodexConfigForHome writes the config a session with a configured K3s
// MCP sidecar gets, for the image build's surface check: the widest tool
// surface a codex session can have is the one that has to pass it.
func renderCodexConfigForHome(home string) error {
	home, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	skillsDir := codexSkillsDir(home)
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return err
	}
	mcpURL, err := k3sProxyMCPURL("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(k3sMCPProxyPort)))
	if err != nil {
		return err
	}
	agentBinary := defaultCodexAgentBinary
	if exe, err := os.Executable(); err == nil && exe != "" {
		agentBinary = exe
	}
	cfg := codexConfig{k3sMCPURL: mcpURL, agentBinary: agentBinary, skillsDir: skillsDir}
	return writeCodexConfig(filepath.Join(home, codexConfigDirName, codexConfigFileName), renderCodexConfig(cfg))
}
