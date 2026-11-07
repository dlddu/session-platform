package main

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// Keep the replaceable state tree below the Kubernetes volume mount. Linux
	// refuses to rename an active mount point (EBUSY), while archive restore
	// atomically swaps this directory with a staged sibling.
	defaultClaudeStateDir = "/session/state"
	defaultClaudeBinary   = "claude"
	platformDefaultModel  = "platform-default"
	claudePermissionMode  = "auto"

	claudeRuntimeStateFile      = ".session-platform-claude.json"
	claudeSettingsDir           = ".claude"
	claudeSettingsFile          = "settings.json"
	claudeSessionPlatformPlugin = "session-platform@dlddu-plugins"
	// claudePluginMarketplace is the marketplace every managed plugin comes
	// from; entrypoint.sh installs from it under this name.
	claudePluginMarketplace = "dlddu-plugins"
	// claudePluginsEnv is a session's own plugin selection (AC-L1), bare
	// names, comma-separated. Unset means the platform plugin alone.
	// Keep in sync with control-plane/internal/adapter/k8s
	// (ClaudeCodePluginsEnvVar) and entrypoint.sh.
	claudePluginsEnv = "CLAUDE_CODE_PLUGINS"
	// sessionMCPPermission names every tool of the session MCP server.
	sessionMCPPermission       = "mcp__" + sessionMCPServerName
	maxClaudePromptBytes       = 1 << 20
	maxClaudeQueuedPrompts     = 64
	maxClaudeQueuedBytes       = 8 << 20
	maxClaudeRunOutputBytes    = 16 << 20
	maxClaudeArchiveBytes      = int64(4 << 30)
	maxClaudeScrollbackBytes   = int64(256 << 20)
	maxClaudeArchiveEntries    = 200_000
	maxClaudeArchivePathBytes  = 4 << 10
	redactedLiteral            = "[REDACTED]"
	claudeCheckpointIDHeader   = "X-Session-Checkpoint-ID"
	claudeRunOutputLimitMarker = "\n[session-platform: invocation output truncated at 16 MiB]\n"
	claudeOutputLimitMarker    = "\n[session-platform: session output limit reached; further prompts are disabled]\n"
	claudeTurnMarkerLead       = "\x1esession-platform "
	claudeTurnSentinel         = '\x1e'

	claudeProcessGroupDrainTimeout      = 5 * time.Second
	defaultClaudeRunTimeout             = 30 * time.Minute
	defaultClaudeCheckpointDrainTimeout = 5 * time.Minute
)

var (
	claudeManagedTools = []string{"Read", "Write", "Edit", "Glob", "Grep", "Bash"}
	claudeModelPattern = regexp.MustCompile(`^(~[A-Za-z0-9][A-Za-z0-9._:/-]{0,126}|[A-Za-z0-9][A-Za-z0-9._:/-]{0,127})$`)
)

var (
	errClaudeAwaitingRestore = errors.New("claude workload is awaiting restore")
	errClaudeAdmissionClosed = errors.New("claude workload is checkpointing; writes are closed")
	errClaudeQueueFull       = errors.New("claude prompt queue is full")
	errClaudeOutputFull      = errors.New("claude session output limit is reached")
	errClaudeAlreadyReady    = errors.New("claude workload is already restored")
	errClaudeStopping        = errors.New("claude workload is stopping")
	errClaudeCheckpointBusy  = errors.New("claude checkpoint archive is still streaming")
	errClaudeCheckpointID    = errors.New("no matching claude checkpoint")
)

// commandRunner is the process-execution seam for the Claude CLI.
type commandRunner interface {
	Run(ctx context.Context, argv []string, opts runnerOptions) error
}

type runnerOptions struct {
	Dir    string
	Env    []string
	Stdout io.Writer
	Stderr io.Writer
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, argv []string, opts runnerOptions) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return errors.New("runner needs an executable")
	}
	if err := prepareClaudeProcessIsolation(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	// A tool may start a background process, so each one-shot invocation gets
	// its own process group and the whole group is killed, not just the CLI.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = claudeProcessGroupDrainTimeout
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	err := cmd.Run()
	if cmd.Process != nil {
		if cleanupErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cleanupErr != nil &&
			!errors.Is(cleanupErr, syscall.ESRCH) {
			err = errors.Join(err, fmt.Errorf("clean claude process group: %w", cleanupErr))
		}
	}
	if cleanupErr := killAndReapClaudeDescendants(); cleanupErr != nil {
		err = errors.Join(err, cleanupErr)
	}
	return err
}

type claudeConfig struct {
	StateDir        string
	Model           string
	Binary          string
	RestoreMode     bool
	Runner          commandRunner
	Logger          *slog.Logger
	Redact          []string
	RunTimeout      time.Duration
	RunOutputLimit  int
	ScrollbackLimit int
	// Tools is written into the session HOME on boot.
	Tools toolSurface
	CLI   agentCLI
	// AuthSlot, when set, is the pool slot whose login this session's CLI
	// uses (docs/prd/agent-auth-pool.md, AC-N4).
	AuthSlot *authSlot
	// Login, when set, signs a claude-code subscription session in on its
	// first prompt (AC-E7). It needs AuthSlot.
	Login claudeLogin
}

// toolSurface is what the platform lets a session's agent reach beyond its own
// filesystem tools. Exactly one form is sanctioned per workload type (AC-E6 for
// claude-code, AC-F6's 2026-09-03 decision for approval-gated).
type toolSurface struct {
	// Plugins are the managed marketplace plugins to enable, as
	// "<name>@dlddu-plugins" (AC-E6; the session's own selection, AC-L1).
	Plugins []string
	// SessionMCP, when set, is the URL of this session's MCP server.
	SessionMCP string
	// SharedDir, when set, is the shared volume this session's MCP writes large
	// approved responses into (AC-F5). It belongs to the tool surface because
	// the agent's file tools are scoped to its own state tree by default, so
	// without it the path the MCP hands back is one the agent may not open.
	SharedDir string
}

// sessionMCPEndpoint is the URL a client has to POST to, which is not the
// address the control plane injects.
//
// Getting this wrong is silent rather than loud: the pinned CLI answers a 404
// at the MCP endpoint by reporting a server with no tools, not an address it
// could not reach, so the session runs on and the model never reaches the gate.
func (t toolSurface) sessionMCPEndpoint() string {
	if t.SessionMCP == "" {
		return ""
	}
	return t.SessionMCP + sessionMCPPath
}

type claudeRuntimeState struct {
	Version int  `json:"version"`
	HasRun  bool `json:"hasRun"`
}

type promptJob struct {
	prompt string
	// seq is the job's place in arrival order. AC-E7's sign-in reads only
	// writes that arrived after its URL was shown as codes.
	seq uint64
}

// claudeWorkload owns one session's one-shot Claude invocations. A single
// worker drains the bounded in-memory queue, so two processes never overlap.
type claudeWorkload struct {
	stateDir string
	workDir  string
	homeDir  string
	model    string
	binary   string
	runner   commandRunner
	logger   *slog.Logger
	redact   []string
	tools    toolSurface
	cli      agentCLI
	out      scrollback
	authSlot *authSlot
	login    claudeLogin

	// approvals is what AC-F3's idle exception is decided from.
	approvals approvalWaits

	mu                      sync.Mutex
	cond                    *sync.Cond
	queue                   *list.List
	queuedBytes             int
	ready                   bool
	accepting               bool
	checkpointing           bool
	checkpointReady         bool
	checkpointID            string
	lastAbortedCheckpointID string
	restoring               bool
	// refreshing holds the worker between turns while AC-J7's plugin refresh
	// rewrites the session's skills.
	refreshing bool
	// turnRefreshed is set once the running turn asked for its one refresh
	// (AC-J7); turnRefreshNotice is the transcript line that refresh left,
	// written after the turn's end marker rather than inside its output.
	turnRefreshed     bool
	turnRefreshNotice string
	// pendingLogin is AC-E7's sign-in between its two writes.
	pendingLogin *pendingClaudeLogin
	// droppedLogin remembers a sign-in that ended before its code arrived,
	// so a code sent for it is still never recorded or run.
	droppedLogin *droppedClaudeLogin
	// enqueued counts accepted writes; it stamps promptJob.seq.
	enqueued        uint64
	active          bool
	hasRun          bool
	turns           int
	stopping        bool
	runTimeout      time.Duration
	runOutputLimit  int
	scrollbackLimit int

	now        func() time.Time
	ctx        context.Context
	cancel     context.CancelFunc
	workerDone chan struct{}
}

func newClaudeWorkload(cfg claudeConfig) (*claudeWorkload, error) {
	stateDir := strings.TrimSpace(cfg.StateDir)
	if stateDir == "" {
		stateDir = defaultClaudeStateDir
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve claude state dir: %w", err)
	}
	stateDir = filepath.Clean(abs)
	if stateDir == string(filepath.Separator) {
		return nil, errors.New("claude state dir must not be the filesystem root")
	}
	model := cfg.Model
	if model == "" {
		model = platformDefaultModel
	}
	if model != strings.TrimSpace(model) || !claudeModelPattern.MatchString(model) {
		return nil, errors.New("claude model must match the platform model identifier pattern")
	}
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = defaultClaudeBinary
	}
	runner := cfg.Runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runTimeout := cfg.RunTimeout
	if runTimeout <= 0 {
		runTimeout = defaultClaudeRunTimeout
	}
	runOutputLimit := cfg.RunOutputLimit
	if runOutputLimit <= 0 {
		runOutputLimit = maxClaudeRunOutputBytes
	}
	scrollbackLimit := cfg.ScrollbackLimit
	if scrollbackLimit <= 0 {
		scrollbackLimit = int(maxClaudeScrollbackBytes)
	}
	if scrollbackLimit < len(claudeOutputLimitMarker) {
		return nil, errors.New("claude scrollback limit cannot hold its terminal marker")
	}
	if int64(scrollbackLimit) > maxClaudeScrollbackBytes {
		return nil, errors.New("claude scrollback limit exceeds archive maximum")
	}
	cli := cfg.CLI
	if cli == nil {
		cli = claudeCLI{}
	}
	if cfg.Login != nil && cfg.AuthSlot == nil {
		return nil, errors.New("a claude subscription sign-in needs an auth slot")
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &claudeWorkload{
		stateDir:        stateDir,
		workDir:         filepath.Join(stateDir, "workspace"),
		homeDir:         filepath.Join(stateDir, "home"),
		model:           model,
		binary:          binary,
		runner:          runner,
		logger:          logger,
		redact:          normaliseRedactionLiterals(cfg.Redact),
		tools:           cfg.Tools,
		cli:             cli,
		authSlot:        cfg.AuthSlot,
		login:           cfg.Login,
		runTimeout:      runTimeout,
		runOutputLimit:  runOutputLimit,
		scrollbackLimit: scrollbackLimit,
		queue:           list.New(),
		now:             time.Now,
		ctx:             ctx,
		cancel:          cancel,
		workerDone:      make(chan struct{}),
	}
	c.cond = sync.NewCond(&c.mu)

	if !cfg.RestoreMode {
		if err := c.ensureStateDirs(); err != nil {
			cancel()
			return nil, err
		}
		hasRun, err := loadClaudeRuntimeState(c.stateDir)
		if err != nil {
			cancel()
			return nil, err
		}
		c.hasRun = hasRun
		c.ready = true
		c.accepting = true
	}
	go c.runWorker()
	return c, nil
}

func (c *claudeWorkload) ensureStateDirs() error {
	for _, dir := range []string{c.stateDir, c.workDir, c.homeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create claude state directory %s: %w", dir, err)
		}
	}
	return c.cli.ensureHome(c.homeDir, c.tools)
}

func (c *claudeWorkload) Close() {
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		<-c.workerDone
		return
	}
	c.stopping = true
	c.accepting = false
	pending := c.pendingLogin
	c.pendingLogin = nil
	if pending != nil && pending.timer != nil {
		pending.timer.Stop()
	}
	c.cancel()
	c.cond.Broadcast()
	c.mu.Unlock()
	if pending != nil {
		pending.proc.kill()
	}
	<-c.workerDone
}

func (c *claudeWorkload) enqueue(prompt string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.stopping:
		return errClaudeStopping
	case !c.ready:
		return errClaudeAwaitingRestore
	case !c.accepting:
		return errClaudeAdmissionClosed
	case c.out.claudeFullAt(c.scrollbackLimit, claudeOutputLimitMarker):
		return errClaudeOutputFull
	case c.queue.Len() >= maxClaudeQueuedPrompts || c.queuedBytes+len(prompt) > maxClaudeQueuedBytes:
		return errClaudeQueueFull
	}
	c.enqueued++
	c.queue.PushBack(promptJob{prompt: prompt, seq: c.enqueued})
	c.queuedBytes += len(prompt)
	c.cond.Signal()
	return nil
}

// appendPlatformNotice writes one platform-generated in-band marker into the
// same append-only record the agent's own output goes to (AC-F3's approval
// markers today), through the same bounded append as agent output (AC-E3).
//
// It deliberately does not consume the per-invocation output quota: that quota
// is defined over assistant text and diagnostic stderr, and a platform notice
// is neither. It also does not pass through the invocation's redaction sink,
// because a notice is assembled here from a tool name and an external
// identifier and never carries provider or gateway material (AC-F6).
func (c *claudeWorkload) appendPlatformNotice(text string) {
	if c.out.appendClaudeBoundedAt(withoutTurnSentinel([]byte(text)), c.scrollbackLimit, claudeOutputLimitMarker) {
		c.logger.Warn("claude session output limit reached; rejecting new prompts")
	}
}

// Marker grammar and quota exemption: docs/prd/claude-code-workload.md AC-E3.
func (c *claudeWorkload) beginTurn(seq int, prompt string) {
	body := strings.TrimRight(strings.ToValidUTF8(prompt, "\uFFFD"), "\r\n") + "\n"
	redactor := newStreamingRedactor(c.redact)
	redacted := append(redactor.Push(withoutTurnSentinel([]byte(body))), redactor.Finish()...)
	block := claudeTurnMarker("input", seq, "at="+c.timestamp()) + string(redacted) + claudeTurnMarker("output", seq)
	c.appendTurnBlock(block)
}

func (c *claudeWorkload) endTurn(seq int, status string) {
	c.appendTurnBlock(claudeTurnMarker("end", seq, "status="+status, "at="+c.timestamp()))
}

func (c *claudeWorkload) appendTurnBlock(block string) {
	if !c.out.endsWithNewline() {
		block = "\n" + block
	}
	if c.out.appendClaudeBoundedAt([]byte(block), c.scrollbackLimit, claudeOutputLimitMarker) {
		c.logger.Warn("claude session output limit reached; rejecting new prompts")
	}
}

func (c *claudeWorkload) timestamp() string {
	return c.now().UTC().Format(time.RFC3339)
}

func claudeTurnMarker(kind string, seq int, fields ...string) string {
	return claudeTurnMarkerLead + kind + " seq=" + strconv.Itoa(seq) + strings.Join(append([]string{""}, fields...), " ") + "\n"
}

func withoutTurnSentinel(p []byte) []byte {
	if bytes.IndexByte(p, claudeTurnSentinel) < 0 {
		return p
	}
	return bytes.ReplaceAll(p, []byte{claudeTurnSentinel}, nil)
}

func claudeTurnStatus(err error, timedOut bool) string {
	switch {
	case err == nil || errors.Is(err, exec.ErrWaitDelay):
		return "ok"
	case timedOut:
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

func (c *claudeWorkload) runWorker() {
	defer close(c.workerDone)
	for {
		c.mu.Lock()
		for !c.stopping && (!c.ready || c.refreshing || c.queue.Len() == 0) {
			c.cond.Wait()
		}
		if c.stopping {
			c.mu.Unlock()
			return
		}
		elem := c.queue.Front()
		job := elem.Value.(promptJob)
		c.queue.Remove(elem)
		c.queuedBytes -= len(job.prompt)
		resume := c.hasRun
		c.active = true
		c.turnRefreshed = false
		c.turnRefreshNotice = ""
		c.turns++
		seq := c.turns
		input := c.classifyInputLocked(job)
		c.mu.Unlock()

		prompt, ok := c.openTurn(seq, job.prompt, input)
		if !ok {
			c.endTurn(seq, "error")
			c.mu.Lock()
			c.active = false
			c.cond.Broadcast()
			c.mu.Unlock()
			continue
		}
		prompt, noticed := c.withTurnNotice(prompt, resume)
		output := newClaudeOutputSink(
			&c.out,
			c.redact,
			c.runOutputLimit,
			c.scrollbackLimit,
			func() { c.logger.Warn("claude session output limit reached; rejecting new prompts") },
		)
		stdoutUTF8 := newUTF8NormalizingWriter(output)
		stdout := c.cli.newProjector(stdoutUTF8)
		stderr := newUTF8NormalizingWriter(output)
		runCtx, cancelRun := context.WithTimeout(c.ctx, c.runTimeout)
		stopSlotWatch := c.watchAuthSlot()
		err := c.runner.Run(runCtx, c.argv(prompt, resume), runnerOptions{
			Dir:    c.workDir,
			Env:    environmentWithHome(c.homeDir),
			Stdout: stdout,
			Stderr: stderr,
		})
		timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
		cancelRun()
		stopSlotWatch()
		c.pushAuthSlot()
		// Flush projector/newline, per-fd UTF-8 tails, redaction tail and the
		// invocation-limit tail before active becomes false. A checkpoint waiting
		// on c.active therefore archives every byte produced by the drained run.
		err = errors.Join(err, stdout.Close())
		err = errors.Join(err, stdoutUTF8.Close())
		err = errors.Join(err, stderr.Close())
		err = errors.Join(err, output.Close())
		c.endTurn(seq, claudeTurnStatus(err, timedOut))
		c.mu.Lock()
		refreshNotice := c.turnRefreshNotice
		c.turnRefreshNotice = ""
		c.mu.Unlock()
		if refreshNotice != "" {
			c.appendPlatformNotice(refreshNotice)
		}

		// Resume state follows AC-E4. Persist before reporting idle, so a
		// checkpoint cannot archive a stale flag.
		succeeded := err == nil || errors.Is(err, exec.ErrWaitDelay)
		nextHasRun := resume || succeeded
		persistErr := persistClaudeRuntimeState(c.stateDir, nextHasRun)
		// A turn that refreshed its own skills was told what changed in the
		// tool result, so its success settles the note as a told turn does.
		if succeeded && (noticed || refreshNotice != "") {
			c.clearTurnNotice()
		}
		c.mu.Lock()
		c.hasRun = nextHasRun
		c.active = false
		c.cond.Broadcast()
		c.mu.Unlock()

		if err != nil && !errors.Is(err, context.Canceled) {
			c.logger.Warn("claude invocation failed", "err", c.redactString(err.Error()))
		}
		if persistErr != nil {
			c.logger.Error("persist claude runtime state", "err", c.redactString(persistErr.Error()))
		}
	}
}

// openTurn writes the turn's input record and settles what the turn runs. A
// turn normally runs its own prompt. With a sign-in pending (AC-E7) the write
// is the sign-in code instead: the record shows a placeholder, and the turn
// runs the prompt that was held for the sign-in. ok is false when the turn
// has nothing to run.
func (c *claudeWorkload) openTurn(seq int, prompt string, input turnInput) (string, bool) {
	switch {
	case input.code != nil:
		c.beginTurn(seq, claudeLoginCodeInput)
		if !c.completeLogin(input.code, prompt) {
			return "", false
		}
		return input.code.prompt, true
	case input.staleCode:
		c.beginTurn(seq, claudeLoginCodeInput)
		c.appendPlatformNotice("[session-platform: the Claude sign-in this message was for had already ended, so it was taken as its code and not used. Send your prompt again to start a new sign-in.]\n")
		return "", false
	case input.beforeSignIn:
		c.beginTurn(seq, prompt)
		c.appendPlatformNotice("[session-platform: this prompt was sent before the Claude sign-in started, so it did not run. Send it again after signing in.]\n")
		return "", false
	}
	c.beginTurn(seq, prompt)
	c.pullAuthSlot()
	if c.login != nil && !c.subscriptionGate(prompt) {
		return "", false
	}
	return prompt, true
}

// pullAuthSlot gives the CLI the slot's login before it runs (AC-N4). A
// failed copy is logged rather than failing the turn: the CLI then reports
// for itself that it is not signed in.
func (c *claudeWorkload) pullAuthSlot() {
	if c.authSlot == nil {
		return
	}
	if err := c.authSlot.pull(c.homeDir); err != nil {
		c.logger.Error("copy the auth slot login into HOME", "err", c.redactString(err.Error()))
	}
}

func (c *claudeWorkload) pushAuthSlot() {
	if c.authSlot == nil {
		return
	}
	if err := c.authSlot.push(c.homeDir); err != nil {
		c.logger.Error("copy the login back to the auth slot", "err", c.redactString(err.Error()))
	}
}

func (c *claudeWorkload) watchAuthSlot() func() {
	if c.authSlot == nil {
		return func() {}
	}
	return c.authSlot.watch(c.homeDir)
}

// archiveExclusions is what the session archive leaves out of the state tree.
func (c *claudeWorkload) archiveExclusions() []string {
	if c.authSlot == nil {
		return nil
	}
	return []string{c.authSlot.archiveExclusion()}
}

// withTurnNotice prepends AC-J7's skill-change note to a resumed turn. A first
// turn starts a conversation that reads the skills as they are now, so it is
// not told; either way a turn that succeeds settles the note. noticed reports
// whether there was one to settle.
func (c *claudeWorkload) withTurnNotice(prompt string, resume bool) (string, bool) {
	refresher, ok := c.cli.(pluginRefresher)
	if !ok {
		return prompt, false
	}
	notice, err := refresher.turnNotice(c.homeDir)
	if err != nil {
		c.logger.Warn("read the pending plugin skill note", "err", c.redactString(err.Error()))
		return prompt, false
	}
	if notice == "" {
		return prompt, false
	}
	if resume {
		prompt = notice + "\n\n" + prompt
	}
	return prompt, true
}

func (c *claudeWorkload) clearTurnNotice() {
	if refresher, ok := c.cli.(pluginRefresher); ok {
		if err := refresher.clearTurnNotice(c.homeDir); err != nil {
			c.logger.Warn("clear the plugin skill note", "err", c.redactString(err.Error()))
		}
	}
}

// refreshPlugins is AC-J7: fetch the platform plugins again without leaving
// the conversation. It runs only between turns — never under a running one,
// never with one queued, never during a checkpoint — and holds the worker
// while it rewrites the skills, so a prompt that arrives meanwhile waits.
func (c *claudeWorkload) refreshPlugins() (pluginRefresh, error) {
	refresher, ok := c.cli.(pluginRefresher)
	if !ok {
		return pluginRefresh{}, errPluginRefreshUnsupported
	}
	c.mu.Lock()
	switch {
	case c.stopping:
		c.mu.Unlock()
		return pluginRefresh{}, errClaudeStopping
	case !c.ready:
		c.mu.Unlock()
		return pluginRefresh{}, errClaudeAwaitingRestore
	case !c.accepting || c.checkpointing:
		c.mu.Unlock()
		return pluginRefresh{}, errClaudeAdmissionClosed
	case c.refreshing:
		c.mu.Unlock()
		return pluginRefresh{}, errPluginRefreshRunning
	case c.active || c.queue.Len() != 0:
		c.mu.Unlock()
		return pluginRefresh{}, errPluginRefreshBusy
	}
	c.refreshing = true
	c.mu.Unlock()

	result, err := refresher.refreshPlugins(c.homeDir)

	c.mu.Lock()
	c.refreshing = false
	c.cond.Broadcast()
	c.mu.Unlock()
	if err != nil {
		c.logger.Warn("plugin refresh failed", "err", c.redactString(err.Error()))
		return pluginRefresh{}, err
	}
	c.appendPlatformNotice(pluginRefreshNotice(result))
	return result, nil
}

// refreshPluginsInTurn is AC-J7: the running turn's own `refresh_skills` call.
// The turn is the caller, so it is the one thing that may be running; it is
// refused while a freeze waits on the turn, under another refresh, and after
// the turn's first attempt, so a model cannot loop the marketplace clone.
func (c *claudeWorkload) refreshPluginsInTurn() (pluginRefresh, error) {
	refresher, ok := c.cli.(pluginRefresher)
	if !ok {
		return pluginRefresh{}, errPluginRefreshUnsupported
	}
	c.mu.Lock()
	switch {
	case c.stopping:
		c.mu.Unlock()
		return pluginRefresh{}, errClaudeStopping
	case !c.ready:
		c.mu.Unlock()
		return pluginRefresh{}, errClaudeAwaitingRestore
	case !c.active:
		c.mu.Unlock()
		return pluginRefresh{}, errPluginRefreshNoTurn
	case c.checkpointing:
		c.mu.Unlock()
		return pluginRefresh{}, errClaudeAdmissionClosed
	case c.refreshing:
		c.mu.Unlock()
		return pluginRefresh{}, errPluginRefreshRunning
	case c.turnRefreshed:
		c.mu.Unlock()
		return pluginRefresh{}, errPluginRefreshTurnLimit
	}
	c.refreshing = true
	c.turnRefreshed = true
	seq := c.turns
	c.mu.Unlock()

	result, err := refresher.refreshPlugins(c.homeDir)

	c.mu.Lock()
	c.refreshing = false
	// A turn that ended while its refresh ran (a timeout) is no longer there to
	// carry the line, and never saw the result, so the note stays pending.
	sameTurn := c.active && c.turns == seq
	if err == nil && sameTurn {
		c.turnRefreshNotice = pluginRefreshNotice(result)
	}
	c.cond.Broadcast()
	c.mu.Unlock()
	if err != nil {
		c.logger.Warn("in-turn plugin refresh failed", "err", c.redactString(err.Error()))
		return pluginRefresh{}, err
	}
	if !sameTurn {
		c.appendPlatformNotice(pluginRefreshNotice(result))
	}
	return result, nil
}

// pluginRefreshStatus maps a refresh error to the agent route's status.
func pluginRefreshStatus(err error) int {
	switch {
	case errors.Is(err, errPluginRefreshUnsupported):
		return http.StatusNotFound
	case errors.Is(err, errClaudeAwaitingRestore), errors.Is(err, errClaudeStopping):
		return http.StatusServiceUnavailable
	case errors.Is(err, errClaudeAdmissionClosed), errors.Is(err, errPluginRefreshBusy),
		errors.Is(err, errPluginRefreshRunning), errors.Is(err, errPluginRefreshNoTurn):
		return http.StatusConflict
	case errors.Is(err, errPluginRefreshTurnLimit):
		return http.StatusTooManyRequests
	case errors.Is(err, errPluginRefreshFailed):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// fromLoopback reports whether a request came from inside the pod. The control
// plane and the kubelet reach the agent on the pod IP; only a process sharing
// the pod's network namespace — the skills MCP codex spawns — reaches loopback.
func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *claudeWorkload) argv(prompt string, resume bool) []string {
	return c.cli.argv(c.binary, c.model, prompt, resume)
}

// claudeArgv builds one Claude Code invocation. effort is AC-E8's session
// effort, empty for the CLI default.
func claudeArgv(binary, model, effort, prompt string, resume bool) []string {
	argv := []string{binary}
	if resume {
		argv = append(argv, "--continue")
	}
	if model != "" && model != platformDefaultModel {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
	}
	// The option delimiter prevents a prompt beginning with "--" from changing
	// any platform-controlled flag.
	return append(argv,
		"--permission-mode", claudePermissionMode,
		"-p", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--", prompt,
	)
}

// beginCheckpoint atomically closes admission and drains all accepted work.
func (c *claudeWorkload) beginCheckpoint(ctx context.Context, requestedID string) ([]byte, string, error) {
	checkpointID, err := claudeCheckpointID(requestedID)
	if err != nil {
		return nil, "", err
	}
	c.mu.Lock()
	stopWake := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer stopWake()
	switch {
	case c.stopping:
		c.mu.Unlock()
		return nil, "", errClaudeStopping
	case !c.ready:
		c.mu.Unlock()
		return nil, "", errClaudeAwaitingRestore
	case !c.accepting || c.checkpointing:
		c.mu.Unlock()
		return nil, "", errClaudeAdmissionClosed
	}
	c.accepting = false
	c.checkpointing = true
	c.checkpointReady = false
	c.checkpointID = checkpointID
	for !c.stopping && ctx.Err() == nil && (c.active || c.refreshing || c.queue.Len() != 0) {
		c.cond.Wait()
	}
	if c.stopping {
		c.checkpointing = false
		c.checkpointReady = false
		c.checkpointID = ""
		c.mu.Unlock()
		return nil, "", errClaudeStopping
	}
	if err := ctx.Err(); err != nil {
		c.reopenCheckpointLocked()
		c.mu.Unlock()
		return nil, "", err
	}
	hasRun := c.hasRun
	pending := c.pendingLogin
	c.mu.Unlock()
	// A sign-in cannot wait across an archive: its process belongs to this pod.
	c.dropPendingLogin(pending, "[session-platform: the session was archived during the Claude sign-in; the held prompt was dropped. Send it again to start over.]\n")
	if err := persistClaudeRuntimeState(c.stateDir, hasRun); err != nil {
		c.abortCheckpoint()
		return nil, "", err
	}
	if err := c.cli.validateState(c.stateDir); err != nil {
		c.abortCheckpoint()
		return nil, "", fmt.Errorf("validate live claude state: %w", err)
	}
	return c.out.snapshot(), checkpointID, nil
}

func claudeCheckpointID(requested string) (string, error) {
	if requested == "" {
		return newClaudeCheckpointID()
	}
	if len(requested) != 32 {
		return "", errClaudeCheckpointID
	}
	decoded, err := hex.DecodeString(requested)
	if err != nil || hex.EncodeToString(decoded) != requested {
		return "", errClaudeCheckpointID
	}
	return requested, nil
}

func newClaudeCheckpointID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate claude checkpoint id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (c *claudeWorkload) abortCheckpoint() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checkpointing {
		return
	}
	c.lastAbortedCheckpointID = c.checkpointID
	c.reopenCheckpointLocked()
}

func (c *claudeWorkload) reopenCheckpointLocked() {
	c.checkpointing = false
	c.checkpointReady = false
	c.checkpointID = ""
	if c.ready && !c.stopping {
		c.accepting = true
		c.cond.Broadcast()
	}
}

// completeCheckpointStream leaves admission closed until the control plane
// either durably stores the archive and stops the pod, or explicitly aborts.
func (c *claudeWorkload) completeCheckpointStream() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checkpointing {
		c.checkpointReady = true
		c.accepting = false
	}
}

// abortCompletedCheckpoint is the second phase used when durable storage or
// pod reclamation fails after the response body was consumed. It is idempotent.
func (c *claudeWorkload) abortCompletedCheckpoint(checkpointID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.stopping:
		return errClaudeStopping
	case !c.ready:
		return errClaudeAwaitingRestore
	case checkpointID == "":
		return errClaudeCheckpointID
	case !c.checkpointing:
		// The durable prepare record is written before the request. If the
		// control plane exits before this agent sees it, abort is already done.
		c.lastAbortedCheckpointID = checkpointID
		return nil
	case checkpointID != c.checkpointID:
		return errClaudeCheckpointID
	case !c.checkpointReady:
		return errClaudeCheckpointBusy
	default:
		c.lastAbortedCheckpointID = checkpointID
		c.reopenCheckpointLocked()
	}
	return nil
}

func (c *claudeWorkload) restore(r io.Reader) error {
	c.mu.Lock()
	switch {
	case c.stopping:
		c.mu.Unlock()
		return errClaudeStopping
	case c.ready, c.restoring:
		c.mu.Unlock()
		return errClaudeAlreadyReady
	}
	c.restoring = true
	c.mu.Unlock()

	scrollbackBytes, err := restoreStateArchive(r, c.stateDir, c.tools.SharedDir, c.cli.validateState)
	if err != nil {
		c.finishFailedRestore()
		return err
	}
	if err := c.ensureStateDirs(); err != nil {
		c.finishFailedRestore()
		return err
	}
	hasRun, err := loadRequiredClaudeRuntimeState(c.stateDir)
	if err != nil {
		c.finishFailedRestore()
		return err
	}

	c.out.restore(scrollbackBytes)
	c.mu.Lock()
	c.turns = bytes.Count(scrollbackBytes, []byte(claudeTurnMarkerLead+"input "))
	c.hasRun = hasRun
	c.ready = true
	c.accepting = true
	c.restoring = false
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}

func (c *claudeWorkload) finishFailedRestore() {
	c.mu.Lock()
	c.restoring = false
	c.mu.Unlock()
}

func (c *claudeWorkload) isReady() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && !c.stopping
}

func (c *claudeWorkload) redactBytes(p []byte) []byte {
	out := append([]byte(nil), p...)
	for _, literal := range c.redact {
		out = bytes.ReplaceAll(out, []byte(literal), []byte(redactedLiteral))
	}
	return out
}

func (c *claudeWorkload) redactString(s string) string {
	return string(c.redactBytes([]byte(s)))
}

func normaliseRedactionLiterals(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, literal := range in {
		if literal == "" {
			continue
		}
		if _, ok := seen[literal]; ok {
			continue
		}
		seen[literal] = struct{}{}
		out = append(out, literal)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func credentialLiteralsFromEnv() []string {
	return normaliseRedactionLiterals([]string{
		os.Getenv("ANTHROPIC_AUTH_TOKEN"),
		os.Getenv("ANTHROPIC_API_KEY"),
		os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"),
		os.Getenv("K3S_MCP_TOKEN"),
	})
}

func environmentWithHome(home string) []string {
	current := os.Environ()
	out := make([]string, 0, len(current)+1)
	for _, item := range current {
		if strings.HasPrefix(item, "HOME=") {
			continue
		}
		out = append(out, item)
	}
	return append(out, "HOME="+home)
}

func persistClaudeRuntimeState(stateDir string, hasRun bool) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create claude state dir: %w", err)
	}
	f, err := os.CreateTemp(stateDir, ".runtime-state-*")
	if err != nil {
		return fmt.Errorf("create claude runtime state: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := json.NewEncoder(f).Encode(claudeRuntimeState{Version: 1, HasRun: hasRun}); err != nil {
		f.Close()
		return fmt.Errorf("encode claude runtime state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync claude runtime state: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(stateDir, claudeRuntimeStateFile)); err != nil {
		return fmt.Errorf("install claude runtime state: %w", err)
	}
	return nil
}

func loadClaudeRuntimeState(stateDir string) (bool, error) {
	f, err := os.Open(filepath.Join(stateDir, claudeRuntimeStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open claude runtime state: %w", err)
	}
	return decodeClaudeRuntimeState(f)
}

func loadRequiredClaudeRuntimeState(stateDir string) (bool, error) {
	f, err := os.Open(filepath.Join(stateDir, claudeRuntimeStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, errors.New("claude archive is missing runtime state")
	}
	if err != nil {
		return false, fmt.Errorf("open claude runtime state: %w", err)
	}
	return decodeClaudeRuntimeState(f)
}

func decodeClaudeRuntimeState(f *os.File) (bool, error) {
	defer f.Close()
	var state struct {
		Version int   `json:"version"`
		HasRun  *bool `json:"hasRun"`
	}
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&state); err != nil {
		return false, fmt.Errorf("decode claude runtime state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return false, errors.New("claude runtime state has trailing JSON")
		}
		return false, fmt.Errorf("decode claude runtime state trailer: %w", err)
	}
	if state.Version != 1 {
		return false, fmt.Errorf("unsupported claude runtime state version %d", state.Version)
	}
	if state.HasRun == nil {
		return false, errors.New("claude runtime state is missing hasRun")
	}
	return *state.HasRun, nil
}

type claudeManagedSettings struct {
	Permissions struct {
		Allow []string `json:"allow"`
		// AdditionalDirectories widens the agent's file tools past its own state
		// tree. The platform sets exactly one entry, the shared volume, and only
		// where there is one (AC-F5).
		AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	} `json:"permissions"`
	EnabledPlugins map[string]bool `json:"enabledPlugins"`
	// MCPServers is no longer written (claude_mcp_registration.go). The field
	// stays so archives taken while it was still decode under
	// DisallowUnknownFields; ensureClaudeManagedSettings then drops it.
	MCPServers map[string]claudeMCPServer `json:"mcpServers,omitempty"`
}

// claudeMCPServer is one registered MCP server. The session MCP is reached over
// the pod network, so it is an http server rather than a spawned process — the
// agent never gets a command it could point somewhere else.
type claudeMCPServer struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	// Timeout is the CLI's per-server tool-call limit in milliseconds. The
	// pinned CLI (2.1.220) carries an undocumented 60 s request default for
	// http servers, and on the cluster (2026-10-24) a gated call was cancelled
	// at exactly 60 s while a human was still deciding (sessionMCPToolTimeout).
	Timeout int64 `json:"timeout,omitempty"`
}

// managedSettingsFor renders the settings a workload type must run with.
func managedSettingsFor(tools toolSurface) claudeManagedSettings {
	settings := claudeManagedSettings{}
	settings.Permissions.Allow = append([]string(nil), claudeManagedTools...)
	settings.EnabledPlugins = map[string]bool{}
	for _, plugin := range tools.Plugins {
		settings.EnabledPlugins[plugin] = true
	}
	if tools.SessionMCP != "" {
		settings.Permissions.Allow = append(settings.Permissions.Allow, sessionMCPPermission)
	}
	if tools.SharedDir != "" {
		settings.Permissions.AdditionalDirectories = []string{tools.SharedDir}
	}
	return settings
}

// ensureClaudeManagedSettings installs this pod's tool surface into the session
// HOME, or normalises the one an archive brought back. Normalising rather than
// trusting the archive matters for approval-gated: the session MCP lives in a
// helper pod whose address is new on every restore (AC-F4), so a restored HOME
// always names the *previous* round's MCP.
//
// Both halves of the surface are written here (claude_mcp_registration.go):
// writing one without the other yields a session whose tools are silently
// absent.
func ensureClaudeManagedSettings(homeDir string, tools toolSurface) error {
	if err := ensureClaudeSettingsFile(homeDir, tools); err != nil {
		return err
	}
	return ensureClaudeMCPRegistration(homeDir, tools)
}

func ensureClaudeSettingsFile(homeDir string, tools toolSurface) error {
	settingsDir := filepath.Join(homeDir, claudeSettingsDir)
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		return fmt.Errorf("create claude settings directory: %w", err)
	}
	settingsPath := filepath.Join(settingsDir, claudeSettingsFile)
	want := managedSettingsFor(tools)
	if _, err := os.Lstat(settingsPath); err == nil {
		settings, err := loadClaudeManagedSettings(homeDir)
		if err != nil {
			return err
		}
		if managedSettingsMatch(settings, want) {
			return nil
		}
		// Keep whatever permissions the archive carried — they are the session's
		// — and re-assert only the platform-managed parts. Snapshots produced
		// before the managed plugin (or before this type existed) land here too.
		settings.EnabledPlugins = want.EnabledPlugins
		settings.MCPServers = want.MCPServers
		settings.Permissions.AdditionalDirectories = want.Permissions.AdditionalDirectories
		for _, allow := range want.Permissions.Allow {
			if !containsString(settings.Permissions.Allow, allow) {
				settings.Permissions.Allow = append(settings.Permissions.Allow, allow)
			}
		}
		return storeClaudeManagedSettings(settingsDir, settingsPath, settings)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return storeClaudeManagedSettings(settingsDir, settingsPath, want)
}

// managedSettingsMatch reports whether the platform-managed parts of settings
// already say what want says.
func managedSettingsMatch(settings, want claudeManagedSettings) bool {
	if len(settings.EnabledPlugins) != len(want.EnabledPlugins) {
		return false
	}
	for name, enabled := range want.EnabledPlugins {
		if settings.EnabledPlugins[name] != enabled {
			return false
		}
	}
	if len(settings.MCPServers) != len(want.MCPServers) {
		return false
	}
	for name, server := range want.MCPServers {
		if settings.MCPServers[name] != server {
			return false
		}
	}
	for _, allow := range want.Permissions.Allow {
		if !containsString(settings.Permissions.Allow, allow) {
			return false
		}
	}
	// Equality, not containment: this list is wholly the platform's, and an
	// entry the archive carried names a directory of a *previous* round that
	// this pod may not even have (AC-F5).
	if len(settings.Permissions.AdditionalDirectories) != len(want.Permissions.AdditionalDirectories) {
		return false
	}
	for i, dir := range want.Permissions.AdditionalDirectories {
		if settings.Permissions.AdditionalDirectories[i] != dir {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func storeClaudeManagedSettings(settingsDir, settingsPath string, settings claudeManagedSettings) error {
	f, err := os.CreateTemp(settingsDir, ".settings-*")
	if err != nil {
		return fmt.Errorf("create claude managed settings: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := json.NewEncoder(f).Encode(settings); err != nil {
		f.Close()
		return fmt.Errorf("encode claude managed settings: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync claude managed settings: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, settingsPath); err != nil {
		return fmt.Errorf("install claude managed settings: %w", err)
	}
	return nil
}

// validateClaudeManagedSettings insists the session HOME declares exactly one of
// the two sanctioned tool surfaces — marketplace plugins (AC-E6, AC-L1) or a session
// MCP (AC-F6). Archive validation runs it before the workload type of the pod
// doing the restore is relevant, and the pod normalises to its own surface right
// afterwards (ensureClaudeManagedSettings).
func validateClaudeManagedSettings(homeDir string) error {
	settings, err := loadClaudeManagedSettings(homeDir)
	if err != nil {
		return err
	}
	registered, err := loadClaudeMCPRegistration(homeDir)
	if err != nil {
		return err
	}
	return validateClaudeToolSurface(settings, registered)
}

// validateClaudeToolSurface is the exactly-one rule over the two halves.
func validateClaudeToolSurface(settings claudeManagedSettings, registered map[string]claudeMCPServer) error {
	// Which plugins is the pod's to say (ensureClaudeManagedSettings); here any
	// non-empty set of enabled marketplace plugins is the plugin surface.
	plugin := len(settings.EnabledPlugins) != 0
	for name, enabled := range settings.EnabledPlugins {
		if !enabled || !managedClaudePlugin(name) {
			plugin = false
		}
	}
	// An archive taken while the registration was written into settings.json
	// still names the surface there, so either location counts as registered —
	// ensureClaudeMCPRegistration moves it on the boot right after this.
	mcp := registersSessionMCP(registered) || registersSessionMCP(settings.MCPServers)
	permitted := containsString(settings.Permissions.Allow, sessionMCPPermission)
	switch {
	case mcp && !permitted:
		return errors.New("claude managed settings registers the session MCP without permitting it")
	case permitted && !mcp:
		return errors.New("claude managed settings permits the session MCP without registering it")
	case plugin == mcp:
		return fmt.Errorf(
			"claude managed settings must declare exactly one platform tool surface: %s plugins or the %s server",
			claudePluginMarketplace, sessionMCPServerName)
	case plugin && (len(settings.MCPServers) != 0 || len(registered) != 0):
		return errors.New("claude managed settings must not mix the managed plugin with an MCP server")
	case mcp && len(settings.EnabledPlugins) != 0:
		return errors.New("claude managed settings must not mix a session MCP with an enabled plugin")
	}
	return nil
}

// claudePluginNamePattern mirrors the control plane's session plugin names
// (session.NormalizePlugins) so a selection it accepted is one this pod can
// install and an archive can carry.
var claudePluginNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// managedClaudePlugin reports whether name is a plugin the platform installs:
// one from its own marketplace. Which ones a given session runs is decided by
// the pod (claudePluginsFromEnv), never by an archive.
func managedClaudePlugin(name string) bool {
	plugin, ok := strings.CutSuffix(name, "@"+claudePluginMarketplace)
	return ok && claudePluginNamePattern.MatchString(plugin)
}

// claudePluginsFromEnv resolves the plugins a claude-code pod enables: the
// session's own selection when the control plane passed one (AC-L1),
// otherwise the platform plugin alone (AC-E6). entrypoint.sh installs the same
// list before the agent starts, so the two must agree on the default.
func claudePluginsFromEnv() ([]string, error) {
	raw, set := os.LookupEnv(claudePluginsEnv)
	if !set {
		return []string{claudeSessionPlatformPlugin}, nil
	}
	names := strings.Split(raw, ",")
	plugins := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !claudePluginNamePattern.MatchString(name) || seen[name] {
			return nil, fmt.Errorf("%s must be a comma-separated list of distinct plugin names, got %q", claudePluginsEnv, raw)
		}
		seen[name] = true
		plugins = append(plugins, name+"@"+claudePluginMarketplace)
	}
	return plugins, nil
}

// registersSessionMCP reports whether servers carry the platform's own entry.
// Anything else a session registered is left alone: AC-F2's egress list means
// the agent cannot reach it anyway, and failing the restore over it would cost
// the session its state.
func registersSessionMCP(servers map[string]claudeMCPServer) bool {
	server, ok := servers[sessionMCPServerName]
	return ok && server.Type == "http"
}

// loadClaudeManagedSettings reads and shape-checks the platform-owned settings
// file. The tool surface it belongs to is decided by validateClaudeToolSurface.
func loadClaudeManagedSettings(homeDir string) (claudeManagedSettings, error) {
	var settings claudeManagedSettings
	settingsDir := filepath.Join(homeDir, claudeSettingsDir)
	dirInfo, err := os.Lstat(settingsDir)
	if errors.Is(err, os.ErrNotExist) {
		return settings, errors.New("claude archive is missing managed settings directory")
	}
	if err != nil {
		return settings, err
	}
	if !dirInfo.IsDir() {
		return settings, errors.New("claude managed settings path is not a directory")
	}
	settingsPath := filepath.Join(settingsDir, claudeSettingsFile)
	info, err := os.Lstat(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return settings, errors.New("claude archive is missing managed settings")
	}
	if err != nil {
		return settings, err
	}
	if !info.Mode().IsRegular() {
		return settings, errors.New("claude managed settings is not a regular file")
	}
	f, err := os.Open(settingsPath)
	if err != nil {
		return settings, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, fmt.Errorf("decode claude managed settings: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return settings, errors.New("claude managed settings has trailing JSON")
		}
		return settings, fmt.Errorf("decode claude managed settings trailer: %w", err)
	}
	allowed := make(map[string]struct{}, len(settings.Permissions.Allow))
	for _, tool := range settings.Permissions.Allow {
		if _, duplicate := allowed[tool]; duplicate {
			return settings, fmt.Errorf("claude managed settings repeats %s permission", tool)
		}
		allowed[tool] = struct{}{}
	}
	for _, tool := range claudeManagedTools {
		if _, ok := allowed[tool]; !ok {
			return settings, fmt.Errorf("claude managed settings is missing %s permission", tool)
		}
	}
	permitted := len(claudeManagedTools)
	if _, ok := allowed[sessionMCPPermission]; ok {
		// The one sanctioned addition to AC-E2's list, and only for the type
		// whose tool surface *is* that MCP server (AC-F6).
		permitted++
	}
	if len(allowed) != permitted {
		return settings, errors.New("claude managed settings contains non-platform permissions")
	}
	// An archive carrying none of the managed parts — or the shape of a type
	// this pod is not — is acceptable: the caller rewrites them right after.
	// Something the platform never installs is not.
	for name := range settings.EnabledPlugins {
		if !managedClaudePlugin(name) {
			return settings, fmt.Errorf("claude managed settings enables unmanaged plugin %s", name)
		}
	}
	for name := range settings.MCPServers {
		if name != sessionMCPServerName {
			return settings, fmt.Errorf("claude managed settings registers unmanaged MCP server %s", name)
		}
	}
	return settings, nil
}

func claudeRoutes(logger *slog.Logger, c *claudeWorkload) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "awaiting restore"
		if c.isReady() {
			status = "ok"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status, "workload": c.cli.workloadName()})
	})

	// AC-F3's idle exception, read side. It stays answerable while the workload
	// is awaiting restore — an unready agent is simply not waiting on anyone.
	mux.HandleFunc("GET "+approvalWaitPath, func(w http.ResponseWriter, _ *http.Request) {
		serveApprovalWait(w, &c.approvals)
	})

	// AC-N4: whether this session's auth slot holds a login, asked by the
	// control plane before it returns the slot to the pool.
	mux.HandleFunc("GET "+authSlotPath, func(w http.ResponseWriter, _ *http.Request) {
		serveAuthSlot(w, c.authSlot)
	})

	mux.HandleFunc("GET /attach", func(w http.ResponseWriter, r *http.Request) {
		if !c.isReady() {
			http.Error(w, "claude workload is awaiting restore", http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		logger.Info("claude attach stream opened", "remote", r.RemoteAddr)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				logger.Info("claude attach stream closed", "remote", r.RemoteAddr, "reason", c.redactString(err.Error()))
				return
			}
		}
	})

	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxClaudePromptBytes)
		prompt, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "prompt exceeds 1 MiB", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read prompt: "+c.redactString(err.Error()), http.StatusBadRequest)
			return
		}
		if err := c.enqueue(string(prompt)); err != nil {
			switch {
			case errors.Is(err, errClaudeQueueFull):
				http.Error(w, err.Error(), http.StatusTooManyRequests)
			case errors.Is(err, errClaudeOutputFull):
				http.Error(w, err.Error(), http.StatusInsufficientStorage)
			case errors.Is(err, errClaudeAwaitingRestore), errors.Is(err, errClaudeStopping):
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			default:
				http.Error(w, err.Error(), http.StatusConflict)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	})

	mux.HandleFunc("GET /read", func(w http.ResponseWriter, r *http.Request) {
		if !c.isReady() {
			http.Error(w, "claude workload is awaiting restore", http.StatusServiceUnavailable)
			return
		}
		offset := 0
		if value := r.URL.Query().Get("offset"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
				return
			}
			offset = n
		}
		payload, next := c.out.Since(offset)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"payload":    string(payload),
			"nextOffset": next,
		})
	})

	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		if !c.isReady() {
			http.Error(w, "claude workload is awaiting restore", http.StatusServiceUnavailable)
			return
		}
		serveOutputStream(w, r, &c.out)
	})

	mux.HandleFunc("POST "+pluginRefreshPath, func(w http.ResponseWriter, _ *http.Request) {
		result, err := c.refreshPlugins()
		if err != nil {
			http.Error(w, c.redactString(err.Error()), pluginRefreshStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})

	// AC-J7: the skills MCP's refresh_skills, from inside the running turn.
	mux.HandleFunc("POST "+pluginRefreshTurnPath, func(w http.ResponseWriter, r *http.Request) {
		if !fromLoopback(r) {
			http.Error(w, "the in-turn refresh is only for the session's own skills MCP", http.StatusForbidden)
			return
		}
		result, err := c.refreshPluginsInTurn()
		if err != nil {
			http.Error(w, c.redactString(err.Error()), pluginRefreshStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})

	mux.HandleFunc("POST /checkpoint", func(w http.ResponseWriter, r *http.Request) {
		archive, err := os.CreateTemp("", "claude-checkpoint-*.tar")
		if err != nil {
			http.Error(w, "create checkpoint archive: "+c.redactString(err.Error()), http.StatusInternalServerError)
			return
		}
		name := archive.Name()
		defer os.Remove(name)
		defer archive.Close()

		drainCtx, cancelDrain := context.WithTimeout(r.Context(), defaultClaudeCheckpointDrainTimeout)
		defer cancelDrain()
		scrollbackBytes, checkpointID, err := c.beginCheckpoint(drainCtx, r.Header.Get(claudeCheckpointIDHeader))
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, errClaudeCheckpointID):
				status = http.StatusBadRequest
			case errors.Is(err, errClaudeAdmissionClosed):
				status = http.StatusConflict
			case errors.Is(err, errClaudeAwaitingRestore), errors.Is(err, errClaudeStopping):
				status = http.StatusServiceUnavailable
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				status = http.StatusRequestTimeout
			}
			http.Error(w, c.redactString(err.Error()), status)
			return
		}
		committed := false
		defer func() {
			if !committed {
				c.abortCheckpoint()
			}
		}()
		if err := writeStateArchiveExcluding(archive, c.stateDir, c.tools.SharedDir, scrollbackBytes, c.archiveExclusions()); err != nil {
			http.Error(w, "build checkpoint archive: "+c.redactString(err.Error()), http.StatusInternalServerError)
			return
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "rewind checkpoint archive: "+c.redactString(err.Error()), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set(claudeCheckpointIDHeader, checkpointID)
		if _, err := io.Copy(w, archive); err != nil {
			logger.Error("stream claude checkpoint archive", "err", c.redactString(err.Error()))
			return
		}
		c.completeCheckpointStream()
		committed = true
	})

	mux.HandleFunc("POST /checkpoint/abort", func(w http.ResponseWriter, r *http.Request) {
		if err := c.abortCompletedCheckpoint(r.Header.Get(claudeCheckpointIDHeader)); err != nil {
			status := http.StatusConflict
			if errors.Is(err, errClaudeStopping) || errors.Is(err, errClaudeAwaitingRestore) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, c.redactString(err.Error()), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"aborted"}`))
	})

	mux.HandleFunc("POST /restore", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxClaudeArchiveBytes)
		if err := c.restore(r.Body); err != nil {
			switch {
			case errors.Is(err, errClaudeAlreadyReady):
				http.Error(w, c.redactString(err.Error()), http.StatusConflict)
			case errors.Is(err, errClaudeStopping):
				http.Error(w, c.redactString(err.Error()), http.StatusServiceUnavailable)
			default:
				http.Error(w, "restore claude archive: "+c.redactString(err.Error()), http.StatusBadRequest)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restored"}`))
	})

	return mux
}
