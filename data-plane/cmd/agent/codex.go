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
	"slices"
	"strconv"
	"strings"
	"time"
)

// The two Codex workloads run codex's app server through a small Node runner,
// codex-runner (data-plane/codex-runner), on the claude-code runner
// (AC-E2~E5): queue, bounds, cursor, archive and resume flag are
// claudeWorkload's. This file is the CLI-specific half; the managed config is
// codex_config.go's, the plugin skills codex_skills.go's, the skills MCP
// codex_skills_mcp.go's and the output projection codex_stream.go's.
//
// codex-chat (docs/prd/codex-workload.md, AC-J1~J8) is the long-lived chat
// session: no shell, a ChatGPT login made through a device code the runner
// forwards (AC-J3) and kept in the pool slot this pod mounts, and a session
// database the sidecar serves from the same slot (AC-N6). codex-task (docs/prd/codex-task-workload.md,
// AC-M1~M4) is the coding session: claude-code's tool level, and a login that
// lives in the pool slot this pod mounts (AC-M2, docs/prd/agent-auth-pool.md).
// In both, the only platform secret in the pod is the K3s MCP token, and it
// stays in the loopback k3s-mcp-proxy sidecar (AC-J6).
const (
	workloadCodexChat = "codex-chat"
	workloadCodexTask = "codex-task"

	// Keep in sync with control-plane/internal/adapter/k8s (CodexModelEnvVar).
	codexModelEnv            = "CODEX_MODEL"
	codexReasoningEffortEnv  = "CODEX_REASONING_EFFORT"
	codexRunnerBinaryEnv     = "CODEX_RUNNER_BIN"
	defaultCodexRunnerBinary = "/usr/local/bin/codex-runner"

	// codexSkillsDirEnv tells the skills MCP which directory it serves.
	codexSkillsDirEnv = "CODEX_SKILLS_DIR"
	// codexAgentURLEnv tells the skills MCP where the agent listens on
	// loopback, for refresh_skills (AC-J7).
	codexAgentURLEnv = "SESSION_PLATFORM_AGENT_URL"
	// The agent binary doubles as two helpers, selected by its first argument
	// (runAgentSubcommand): the stdio skills MCP that codex spawns, and the
	// config renderer the image build runs its surface check against.
	codexSkillsMCPCommand    = "skills-mcp"
	codexRenderConfigCommand = "codex-render-config"
	defaultCodexAgentBinary  = "/agent"

	codexK3SServerName    = "k3s"
	codexSkillsServerName = "skills"
	// codexSessionDBServerName is the sidecar's session DB MCP (AC-N6).
	codexSessionDBServerName = "db"
	// codexSchedulerServerName is the sidecar's scheduler MCP (AC-P1).
	codexSchedulerServerName = "scheduler"

	// The slot login file of both codex types and its HOME place (AC-M2,
	// AC-N6).
	codexAuthFile    = "auth.json"
	codexAuthHomeRel = codexConfigDirName + "/" + codexAuthFile

	// The sidecar starts with this container, not before it, and its
	// marketplace clone may still be running; the wait covers both. Past it
	// the session runs without what the sidecar did not deliver (AC-J5).
	codexSidecarWait = 90 * time.Second
)

// codexReasoningEfforts is AC-M4's list.
var codexReasoningEfforts = []string{"minimal", "low", "medium", "high", "xhigh"}

// codexCLI is the agentCLI for codex-runner. It takes no provider address and
// no key: the runner's codex child signs in with a ChatGPT account — the
// session's own (AC-J3) or its slot's (AC-M2).
type codexCLI struct {
	// task selects codex-task's surface (AC-M1) over codex-chat's (AC-J5).
	task bool
	// effort is codex-task's session reasoning effort (AC-M4), empty for the
	// model's default.
	effort string
	// k3sProxyURL is the loopback k3s-mcp-proxy sidecar (AC-J6).
	k3sProxyURL string
	// agentBinary is what codex spawns as the skills MCP.
	agentBinary string
	// agentURL is this agent on loopback, which the skills MCP's
	// refresh_skills calls (AC-J7); empty offers no refresh tool.
	agentURL string
	// sessionDBURL is the sidecar's session DB MCP (AC-N6), codex-chat only;
	// empty registers no `db` server.
	sessionDBURL string
	// schedulerURL is the sidecar's scheduler MCP (AC-P1); empty registers no
	// `scheduler` server.
	schedulerURL string
	// probeSidecar reads what the sidecar offers; a field so tests need no
	// server. It reports what it learned even when it also returns an error.
	probeSidecar func(proxyURL string) (codexSidecar, error)
	// refreshSidecar asks the sidecar to clone the marketplace again (AC-J7);
	// a field for the same reason.
	refreshSidecar func(proxyURL string) (miniMaxRefreshResult, error)
	logger         *slog.Logger
}

func newCodexCLI(k3sProxyURL string, logger *slog.Logger) codexCLI {
	agentBinary := defaultCodexAgentBinary
	if exe, err := os.Executable(); err == nil && exe != "" {
		agentBinary = exe
	}
	return codexCLI{k3sProxyURL: k3sProxyURL, agentBinary: agentBinary, logger: logger}
}

func (c codexCLI) workloadName() string {
	if c.task {
		return workloadCodexTask
	}
	return workloadCodexChat
}

// argv is one runner invocation (AC-J2). The model is always passed, the
// platform-default sentinel included, so the runner — not this side — decides
// what an unset model means. The prompt follows "--", so a prompt that starts
// with "--" can never change a platform-controlled flag. --resume follows
// AC-E4: only after the first successful run. codex-chat's argv carries no
// mode, so the runner's default is the narrow surface.
func (c codexCLI) argv(binary, model, prompt string, resume bool) []string {
	if model == "" {
		model = platformDefaultModel
	}
	argv := []string{binary}
	if c.task {
		argv = append(argv, "--mode", "task")
	}
	argv = append(argv, "--model", model)
	if c.effort != "" {
		argv = append(argv, "--effort", c.effort)
	}
	if resume {
		argv = append(argv, "--resume")
	}
	return append(argv, "--", prompt)
}

func (c codexCLI) schedulerStateURL() string {
	return schedulerStateURLFor(c.schedulerURL)
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
	if _, err := syncCodexSkills(skillsDir, skills, c.log()); err != nil {
		return err
	}
	cfg := codexConfig{
		task:         c.task,
		agentBinary:  c.agentBinary,
		skillsDir:    skillsDir,
		sessionDBURL: c.sessionDBURL,
		schedulerURL: c.schedulerURL,
	}
	if sidecar.configured {
		mcpURL, err := k3sProxyMCPURL(c.k3sProxyURL)
		if err != nil {
			return err
		}
		cfg.k3sMCPURL = mcpURL
		// Only a sidecar holding a token has a marketplace to refresh.
		cfg.agentURL = c.agentURL
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

// newCodexWorkloadFromEnv builds a codex agent from the pod environment the
// control plane injects (AC-J1, AC-J6; AC-M1, AC-M2, AC-M4).
func newCodexWorkloadFromEnv(workload string, logger *slog.Logger, restoreMode bool) (*claudeWorkload, error) {
	proxy, err := validateCodexClientEnv()
	if err != nil {
		return nil, err
	}
	runTimeout, err := durationEnv("CLAUDE_CODE_RUN_TIMEOUT", defaultClaudeRunTimeout)
	if err != nil {
		return nil, err
	}
	cli := newCodexCLI(proxy, logger)
	cli.agentURL = loopbackAgentURL(env("DATA_PLANE_AGENT_ADDR", defaultAddr))
	effort := os.Getenv(codexReasoningEffortEnv)
	// Both types keep their ChatGPT login in a pool slot (AC-M2, AC-N6).
	slot, err := newAuthSlotFromEnv(codexAuthFile, codexAuthHomeRel, logger)
	if err != nil {
		return nil, err
	}
	if cli.schedulerURL, err = schedulerMCPURLFromEnv(os.Getenv); err != nil {
		return nil, err
	}
	if workload == workloadCodexTask {
		if effort != "" && !slices.Contains(codexReasoningEfforts, effort) {
			return nil, fmt.Errorf("%s must be one of %s, got %q", codexReasoningEffortEnv, strings.Join(codexReasoningEfforts, ", "), effort)
		}
		cli.task = true
		cli.effort = effort
	} else {
		if effort != "" {
			// AC-E8: codex-chat takes no effort, so one here is a spec this
			// agent was not built for.
			return nil, fmt.Errorf("codex-chat does not take %s", codexReasoningEffortEnv)
		}
		if cli.sessionDBURL, err = sessionDBMCPURLFromEnv(); err != nil {
			return nil, err
		}
	}
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
		AuthSlot:    slot,
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
		if err := serveCodexSkillsMCP(stdin, stdout, os.Getenv(codexSkillsDirEnv), os.Getenv(codexAgentURLEnv), logger); err != nil {
			fmt.Fprintln(stderr, "skills-mcp:", err)
			return 1, true
		}
		return 0, true
	case codexRenderConfigCommand:
		if len(args) < 2 || len(args) > 3 || strings.TrimSpace(args[1]) == "" ||
			(len(args) == 3 && args[2] != "chat" && args[2] != "task") {
			fmt.Fprintln(stderr, "usage: agent codex-render-config <home> [chat|task]")
			return 2, true
		}
		task := len(args) == 3 && args[2] == "task"
		if err := renderCodexConfigForHome(args[1], task); err != nil {
			fmt.Fprintln(stderr, "codex-render-config:", err)
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// renderCodexConfigForHome writes the config a session of the given mode with
// a configured K3s MCP sidecar gets, for the image build's surface check: the
// widest tool surface a session of that mode can have is the one that has to
// pass it.
func renderCodexConfigForHome(home string, task bool) error {
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
	cfg := codexConfig{task: task, k3sMCPURL: mcpURL, agentBinary: agentBinary, skillsDir: skillsDir,
		agentURL:     loopbackAgentURL(defaultAddr),
		schedulerURL: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(k3sMCPProxyPort)) + schedulerMCPPath}
	if !task {
		cfg.sessionDBURL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(k3sMCPProxyPort)) + sessionDBMCPPath
	}
	return writeCodexConfig(filepath.Join(home, codexConfigDirName, codexConfigFileName), renderCodexConfig(cfg))
}

// loopbackAgentURL is the agent's own listen address as the skills MCP reaches
// it on loopback, or "" when the agent does not listen there.
func loopbackAgentURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return ""
	}
	if host != "" {
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !(ip.IsLoopback() || ip.IsUnspecified())) {
			return ""
		}
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port)
}
