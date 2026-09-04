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
	// sessionMCPPermission is how the managed permission allowlist names every
	// tool of the session MCP server (AC-F6 adds exactly this to AC-E2's list).
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

// commandRunner is the process-execution seam for the Claude CLI. Production
// uses execCommandRunner; tests inject a deterministic fake.
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
	// A tool may start a background process. Give each one-shot invocation its
	// own process group, kill the whole group on cancellation and again after
	// the direct CLI exits, and bound inherited stdout/stderr pipe draining.
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
	// Tools is the platform-managed tool surface written into the session
	// HOME's settings.json. It is the one place the two agent workload types
	// diverge (AC-E6 vs AC-F6).
	Tools toolSurface
}

// toolSurface is what the platform lets a session's agent reach beyond its own
// filesystem tools. Exactly one form is sanctioned per workload type:
// claude-code enables the marketplace plugin, approval-gated registers its
// session MCP instead (AC-F6's 2026-09-03 decision — no plugin bootstrap in
// that type, because AC-F2's egress allowlist has neither the K3s MCP endpoint
// nor github.com in it).
type toolSurface struct {
	// Plugin enables the managed session-platform marketplace plugin.
	Plugin bool
	// SessionMCP, when set, is the URL of this session's MCP server.
	SessionMCP string
}

type claudeRuntimeState struct {
	Version int  `json:"version"`
	HasRun  bool `json:"hasRun"`
}

type promptJob struct {
	prompt string
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
	out      scrollback

	// approvals is what AC-F3's idle exception is decided from. It stays empty
	// for claude-code, which has no gate to wait on.
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
	active                  bool
	hasRun                  bool
	stopping                bool
	runTimeout              time.Duration
	runOutputLimit          int
	scrollbackLimit         int

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
		runTimeout:      runTimeout,
		runOutputLimit:  runOutputLimit,
		scrollbackLimit: scrollbackLimit,
		queue:           list.New(),
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
	return ensureClaudeManagedSettings(c.homeDir, c.tools)
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
	c.cancel()
	c.cond.Broadcast()
	c.mu.Unlock()
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
	c.queue.PushBack(promptJob{prompt: prompt})
	c.queuedBytes += len(prompt)
	c.cond.Signal()
	return nil
}

// appendPlatformNotice writes one platform-generated in-band marker into the
// same append-only record the agent's own output goes to (AC-F3's approval
// markers today). It goes through the bounded append, so the cumulative
// scrollback limit and its terminal marker hold exactly as they do for agent
// output — a marker cannot push the buffer past the bound or land after the
// stream has been closed by it (AC-E3).
//
// It deliberately does not consume the per-invocation output quota: that quota
// is defined over assistant text and diagnostic stderr, and a platform notice
// is neither. It also does not pass through the invocation's redaction sink,
// because a notice is assembled here from a tool name and an external
// identifier and never carries provider or gateway material (AC-F6).
func (c *claudeWorkload) appendPlatformNotice(text string) {
	if c.out.appendClaudeBoundedAt([]byte(text), c.scrollbackLimit, claudeOutputLimitMarker) {
		c.logger.Warn("claude session output limit reached; rejecting new prompts")
	}
}

func (c *claudeWorkload) runWorker() {
	defer close(c.workerDone)
	for {
		c.mu.Lock()
		for !c.stopping && (!c.ready || c.queue.Len() == 0) {
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
		c.mu.Unlock()

		output := newClaudeOutputSink(
			&c.out,
			c.redact,
			c.runOutputLimit,
			c.scrollbackLimit,
			func() { c.logger.Warn("claude session output limit reached; rejecting new prompts") },
		)
		stdoutUTF8 := newUTF8NormalizingWriter(output)
		stdout := newClaudeStreamProjector(stdoutUTF8)
		stderr := newUTF8NormalizingWriter(output)
		runCtx, cancelRun := context.WithTimeout(c.ctx, c.runTimeout)
		err := c.runner.Run(runCtx, c.argv(job.prompt, resume), runnerOptions{
			Dir:    c.workDir,
			Env:    environmentWithHome(c.homeDir),
			Stdout: stdout,
			Stderr: stderr,
		})
		cancelRun()
		// Flush projector/newline, per-fd UTF-8 tails, redaction tail and the
		// invocation-limit tail before active becomes false. A checkpoint waiting
		// on c.active therefore archives every byte produced by the drained run.
		err = errors.Join(err, stdout.Close())
		err = errors.Join(err, stdoutUTF8.Close())
		err = errors.Join(err, stderr.Close())
		err = errors.Join(err, output.Close())

		// A failed first invocation may not have created a Claude conversation.
		// Once a run succeeds, later failures do not erase the established resume
		// state. Persist before reporting idle, so checkpoint cannot archive a
		// stale flag.
		nextHasRun := resume || err == nil || errors.Is(err, exec.ErrWaitDelay)
		persistErr := persistClaudeRuntimeState(c.stateDir, nextHasRun)
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

func (c *claudeWorkload) argv(prompt string, resume bool) []string {
	argv := []string{c.binary}
	if resume {
		argv = append(argv, "--continue")
	}
	if c.model != "" && c.model != platformDefaultModel {
		argv = append(argv, "--model", c.model)
	}
	// Partial stream-json is projected back to user-facing text by runWorker.
	// Permission mode is platform policy for every invocation. The
	// option delimiter still prevents a prompt beginning with "--" from changing
	// any platform-controlled flag.
	return append(argv,
		"--permission-mode", claudePermissionMode,
		"-p", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--", prompt,
	)
}

// beginCheckpoint atomically closes admission and drains all accepted work.
// commitCheckpoint keeps it closed after a successful stream; abortCheckpoint
// reopens it when archive construction or delivery fails.
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
	for !c.stopping && ctx.Err() == nil && (c.active || c.queue.Len() != 0) {
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
	c.mu.Unlock()
	if err := persistClaudeRuntimeState(c.stateDir, hasRun); err != nil {
		c.abortCheckpoint()
		return nil, "", err
	}
	if err := validateRestoredClaudeState(c.stateDir); err != nil {
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

	scrollbackBytes, err := restoreClaudeArchive(r, c.stateDir)
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
	} `json:"permissions"`
	EnabledPlugins map[string]bool            `json:"enabledPlugins"`
	MCPServers     map[string]claudeMCPServer `json:"mcpServers,omitempty"`
}

// claudeMCPServer is one registered MCP server. The session MCP is reached over
// the pod network, so it is an http server rather than a spawned process — the
// agent never gets a command it could point somewhere else.
type claudeMCPServer struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// managedSettingsFor renders the settings a workload type must run with.
func managedSettingsFor(tools toolSurface) claudeManagedSettings {
	settings := claudeManagedSettings{}
	settings.Permissions.Allow = append([]string(nil), claudeManagedTools...)
	settings.EnabledPlugins = map[string]bool{}
	if tools.Plugin {
		settings.EnabledPlugins[claudeSessionPlatformPlugin] = true
	}
	if tools.SessionMCP != "" {
		settings.MCPServers = map[string]claudeMCPServer{
			sessionMCPServerName: {Type: "http", URL: tools.SessionMCP},
		}
		settings.Permissions.Allow = append(settings.Permissions.Allow, sessionMCPPermission)
	}
	return settings
}

// ensureClaudeManagedSettings installs the managed settings, or normalises the
// ones an archive brought back, to this pod's tool surface. Normalising rather
// than trusting the archive matters for approval-gated: the session MCP lives
// in a helper pod whose address is new on every restore (AC-F4), so a restored
// settings file always names the *previous* round's MCP.
func ensureClaudeManagedSettings(homeDir string, tools toolSurface) error {
	settingsDir := filepath.Join(homeDir, claudeSettingsDir)
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		return fmt.Errorf("create claude settings directory: %w", err)
	}
	settingsPath := filepath.Join(settingsDir, claudeSettingsFile)
	want := managedSettingsFor(tools)
	if _, err := os.Lstat(settingsPath); err == nil {
		settings, err := loadClaudeManagedSettings(homeDir, false)
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
// already say what want says, so an unchanged file is left alone.
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

func validateClaudeManagedSettings(homeDir string) error {
	_, err := loadClaudeManagedSettings(homeDir, true)
	return err
}

// loadClaudeManagedSettings reads the managed settings. With requireToolSurface
// it also insists the file declares exactly one of the two sanctioned tool
// surfaces — the marketplace plugin (AC-E6) or a session MCP (AC-F6) — which is
// what archive validation needs: it runs before the workload type of the pod
// doing the restore is relevant, and the pod normalises to its own surface
// right afterwards (ensureClaudeManagedSettings).
func loadClaudeManagedSettings(homeDir string, requireToolSurface bool) (claudeManagedSettings, error) {
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
		if len(settings.MCPServers) == 0 {
			return settings, errors.New("claude managed settings permits the session MCP without registering it")
		}
		permitted++
	}
	if len(allowed) != permitted {
		return settings, errors.New("claude managed settings contains non-platform permissions")
	}
	if !requireToolSurface {
		// Normalisation path: the caller rewrites the managed parts right after,
		// so an archive carrying none of them — or the shape of a type this pod
		// is not — is acceptable. Something the platform never installs is not.
		for name := range settings.EnabledPlugins {
			if name != claudeSessionPlatformPlugin {
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
	plugin := len(settings.EnabledPlugins) == 1 && settings.EnabledPlugins[claudeSessionPlatformPlugin]
	mcp := len(settings.MCPServers) == 1 && settings.MCPServers[sessionMCPServerName].Type == "http"
	if mcp {
		if _, ok := allowed[sessionMCPPermission]; !ok {
			return settings, errors.New("claude managed settings registers the session MCP without permitting it")
		}
	}
	if plugin == mcp {
		return settings, fmt.Errorf(
			"claude managed settings must declare exactly one platform tool surface: %s or the %s server",
			claudeSessionPlatformPlugin, sessionMCPServerName)
	}
	if plugin && len(settings.MCPServers) != 0 {
		return settings, errors.New("claude managed settings must not mix the managed plugin with an MCP server")
	}
	if mcp && len(settings.EnabledPlugins) != 0 {
		return settings, errors.New("claude managed settings must not mix a session MCP with an enabled plugin")
	}
	return settings, nil
}

func claudeRoutes(logger *slog.Logger, c *claudeWorkload) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if c.isReady() {
			_, _ = w.Write([]byte(`{"status":"ok","workload":"claude-code"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"awaiting restore","workload":"claude-code"}`))
	})

	// AC-F3's idle exception, read side. It answers whether a human is being
	// waited on right now, so the control plane can hold the idle count
	// instead of freezing a session that has an approval in flight. It stays
	// answerable while the workload is awaiting restore — an unready agent is
	// simply not waiting on anyone.
	mux.HandleFunc("GET "+approvalWaitPath, func(w http.ResponseWriter, _ *http.Request) {
		serveApprovalWait(w, &c.approvals)
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
		if err := writeClaudeArchive(archive, c.stateDir, scrollbackBytes); err != nil {
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
