// Command agent is the data plane entrypoint for every workload mode, selected
// by DATA_PLANE_WORKLOAD (default "shell"). The switch in main lists them, and
// each mode's contract lives with its own code: shell here (AC-D1~D3) and its
// archive in checkpoint.go (AC-D4),
// claude-code in claude.go (AC-E2~E5) with its tool surface in
// agentToolSurface below, minimax-code in minimax_code.go on the same runner,
// mcp in session_mcp.go, credential-proxy in credential_proxy.go.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const (
	workloadEnv             = "DATA_PLANE_WORKLOAD"
	workloadShell           = "shell"
	workloadClaudeCode      = "claude-code"
	workloadSessionMCP      = "mcp"
	workloadCredentialProxy = "credential-proxy"

	claudeProxyBaseURL          = "http://127.0.0.1:8091"
	claudeProxyPlaceholderToken = "session-platform-proxy"
	// helperProxyPort and sessionMCPPort are the ports of this session's helper
	// pod (AC-F2). Keep them in sync with the control plane orchestrator
	// (credentialProxyPort, SessionMCPPort).
	helperProxyPort = 8091
	sessionMCPPort  = 8092
	// proxyPlacementEnv is read by credentialProxyPlacementFromEnv.
	proxyPlacementEnv = "DATA_PLANE_PROXY_PLACEMENT"
	// sessionMCPURLEnv carries the address of this session's MCP (AC-F6).
	sessionMCPURLEnv = "SESSION_MCP_URL"
	// The MCP container's own environment (AC-F6). Keep in sync with
	// control-plane/internal/adapter/k8s (ApprovalGateway*EnvVar, SessionIDEnvVar).
	approvalGatewayURLEnv       = "APPROVAL_GATEWAY_URL"
	approvalGatewayTokenFileEnv = "APPROVAL_GATEWAY_TOKEN_FILE"
	sessionIDEnv                = "SESSION_ID"
	// sharedVolumeDirEnv is the session's shared volume (AC-F5), read by both
	// halves of the pair: the MCP container spills large approved responses
	// there and the workload container's agent reads them back. Keep in sync
	// with control-plane/internal/adapter/k8s (SharedVolumeDirEnvVar).
	sharedVolumeDirEnv = "SESSION_SHARED_DIR"

	defaultShell = "/bin/bash"
	// defaultAddr: keep the port in sync with the control plane orchestrator's
	// agentPort (control-plane/internal/adapter/k8s/client_orchestrator.go).
	defaultAddr = ":8090"
	// restoreModeEnv is set by the control plane on restore-target pods
	// (AnnotationRestoreArchive path).
	restoreModeEnv = "DATA_PLANE_RESTORE_MODE"
	// shellStateDirEnv names the shell's archived state root. The shell's home
	// directory is its "home" child; keep the default in sync with the control
	// plane orchestrator, which mounts the volume above it.
	shellStateDirEnv     = "SHELL_STATE_DIR"
	defaultShellStateDir = "/session/state"
)

func main() {
	if code, handled := runAgentSubcommand(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := env("DATA_PLANE_AGENT_ADDR", defaultAddr)
	workload := env(workloadEnv, workloadShell)
	restoreMode := env(restoreModeEnv, "") == "1"
	a := &agent{exited: make(chan struct{})}
	var handler http.Handler

	switch workload {
	case workloadShell:
		a.shellPath = env("DATA_PLANE_SHELL", defaultShell)
		stateDir, err := shellStateDir(env(shellStateDirEnv, defaultShellStateDir))
		if err != nil {
			logger.Error("invalid shell state dir", "err", err)
			os.Exit(1)
		}
		a.stateDir = stateDir
		if restoreMode {
			logger.Info("shell agent started in restore mode; awaiting archive", "addr", addr, "state_dir", stateDir)
		} else {
			if err := os.MkdirAll(a.homeDir(), 0o700); err != nil {
				logger.Error("failed to create shell home", "home", a.homeDir(), "err", err)
				os.Exit(1)
			}
			sh, err := startShell(a.shellPath, a.homeDir(), nil)
			if err != nil {
				logger.Error("failed to start session shell", "shell", a.shellPath, "err", err)
				os.Exit(1)
			}
			a.adopt(sh)
			logger.Info("session shell started", "shell", a.shellPath, "pid", sh.pid, "addr", addr, "home", a.homeDir())
		}
	case workloadClaudeCode:
		auth, err := claudeAuthFromEnv(workload)

		if err != nil {
			logger.Error("unsafe agent credential configuration", "workload", workload, "err", err)
			os.Exit(1)
		}
		platform, err := claudePlatformFromEnv(logger)
		if err != nil {
			logger.Error("unsafe agent credential configuration", "workload", workload, "err", err)
			os.Exit(1)
		}
		runTimeout, err := durationEnv("CLAUDE_CODE_RUN_TIMEOUT", defaultClaudeRunTimeout)
		if err != nil {
			logger.Error("invalid claude-code run timeout", "err", err)
			os.Exit(1)
		}
		claude, err := newClaudeWorkload(claudeConfig{
			StateDir:    env("CLAUDE_CODE_STATE_DIR", defaultClaudeStateDir),
			Model:       env("CLAUDE_CODE_MODEL", platformDefaultModel),
			Binary:      env(claudeRunnerBinaryEnv, defaultClaudeRunnerBinary),
			RestoreMode: restoreMode,
			Runner:      execCommandRunner{},
			Logger:      logger,
			Redact:      credentialLiteralsFromEnv(),
			RunTimeout:  runTimeout,
			Tools:       auth.tools,
			CLI:         claudeCLI{effort: auth.effort, platform: platform, agentURL: loopbackAgentURL(addr)},
		})
		if err != nil {
			logger.Error("failed to initialise agent workload", "workload", workload, "err", err)
			os.Exit(1)
		}
		a.claude = claude
		logger.Info("agent workload started", "workload", workload, "addr", addr,
			"model", claude.model, "state_dir", claude.stateDir, "restore_mode", restoreMode,
			"auth_mode", auth.mode, "effort", auth.effort)
	case workloadCodexChat, workloadCodexTask:
		claude, err := newCodexWorkloadFromEnv(workload, logger, restoreMode)
		if err != nil {
			logger.Error("failed to initialise agent workload", "workload", workload, "err", err)
			os.Exit(1)
		}
		a.claude = claude
		logger.Info("agent workload started", "workload", workload, "addr", addr,
			"model", claude.model, "state_dir", claude.stateDir, "restore_mode", restoreMode)
	case workloadMiniMaxCode:
		claude, err := newMiniMaxWorkloadFromEnv(logger, restoreMode)
		if err != nil {
			logger.Error("failed to initialise agent workload", "workload", workload, "err", err)
			os.Exit(1)
		}
		a.claude = claude
		logger.Info("agent workload started", "workload", workload, "addr", addr,
			"model", claude.model, "state_dir", claude.stateDir, "restore_mode", restoreMode)
	case workloadSessionMCP:
		// AC-F4's helper pod container; it serves its own routes rather than the
		// workload agent's (session_mcp.go).
		gateway, err := newApprovalGateway(
			os.Getenv(approvalGatewayURLEnv),
			os.Getenv(approvalGatewayTokenFileEnv),
			os.Getenv(sessionIDEnv),
		)
		if err != nil {
			logger.Warn("session MCP has no approval gate; it will offer no tools", "err", err)
			gateway = nil
		}
		shared := env(sharedVolumeDirEnv, "")
		repoAccess := newRepoAccessClient(os.Getenv(repoAccessURLEnv), os.Getenv(repoAccessTokenFileEnv))
		var k3sProxyMCP string
		if proxyBase := os.Getenv(k3sMCPProxyURLEnv); proxyBase != "" {
			mcpURL, err := k3sProxyMCPURL(proxyBase)
			if err != nil {
				logger.Warn("session MCP ignores an invalid K3s MCP proxy URL; private clones cannot be issued a token", "err", err)
			} else {
				k3sProxyMCP = mcpURL
			}
		}
		drop := newDropClient(os.Getenv(dropURLEnv), os.Getenv(dropTokenFileEnv))
		handler = sessionMCPRoutes(logger, newSessionMCPConfig(gateway, shared, repoAccess, k3sProxyMCP, drop))
		logger.Info("session MCP started", "addr", addr, "gated", gateway != nil, "shared_dir", shared,
			"repo_access", repoAccess != nil, "k3s_mcp_proxy", k3sProxyMCP != "", "drop", drop != nil)
	case workloadCredentialProxy:
		placement, err := credentialProxyPlacementFromEnv()
		if err != nil {
			logger.Error("invalid credential-proxy placement", "err", err)
			os.Exit(1)
		}
		if err := validateCredentialProxyBindAddr(addr, placement); err != nil {
			logger.Error("invalid credential-proxy bind address", "placement", string(placement), "err", err)
			os.Exit(1)
		}
		proxy, err := newCredentialProxy(
			os.Getenv("ANTHROPIC_BASE_URL"),
			os.Getenv("ANTHROPIC_AUTH_TOKEN"),
			os.Getenv(providerCACertEnv),
			logger,
		)
		if err != nil {
			logger.Error("failed to initialise credential proxy", "err", err)
			os.Exit(1)
		}
		handler = proxy
		logger.Info("credential proxy started", "addr", addr, "placement", string(placement))
	case workloadK3SMCPProxy:
		placement, err := credentialProxyPlacementFromEnv()
		if err != nil {
			logger.Error("invalid k3s-mcp-proxy placement", "err", err)
			os.Exit(1)
		}
		if err := validateCredentialProxyBindAddr(addr, placement); err != nil {
			logger.Error("invalid k3s-mcp-proxy bind address", "placement", string(placement), "err", err)
			os.Exit(1)
		}
		token := os.Getenv(k3sMCPTokenEnv)
		proxy, err := newK3SMCPProxy(os.Getenv(k3sMCPURLEnv), token, os.Getenv(sessionIDEnv), logger)
		if err != nil {
			logger.Error("failed to initialise k3s mcp proxy", "err", err)
			os.Exit(1)
		}
		handler = proxy
		if proxy.seeder != nil {
			go proxy.seeder.run(context.Background())
		}
		// A codex-chat sidecar also serves the session DB from its slot (AC-N6).
		if dir := os.Getenv(sessionDBDirEnv); dir != "" {
			db, err := newSessionDB(dir, logger)
			if err != nil {
				logger.Error("invalid session database directory", "err", err)
				os.Exit(1)
			}
			defer db.Close()
			handler = withSessionDBMCP(proxy, db)
			logger.Info("session db mcp enabled", "path", sessionDBMCPPath)
		}
		// A codex sidecar also keeps the session's schedules (AC-P1~P3).
		if raw := os.Getenv(schedulerAgentURLEnv); raw != "" {
			agent, err := newHTTPSchedulerAgent(raw)
			if err != nil {
				logger.Error("invalid scheduler agent URL", "err", err)
				os.Exit(1)
			}
			sched := newScheduler(agent, logger)
			go sched.run(context.Background())
			handler = withScheduler(handler, sched)
			logger.Info("scheduler mcp enabled", "path", schedulerMCPPath)
		}
		logger.Info("k3s mcp proxy started", "addr", addr, "configured", token != "",
			"session_id_header", k3sMCPFixedHeaders(os.Getenv(sessionIDEnv)) != nil)
	default:
		logger.Error("unknown data plane workload", "workload", workload)
		os.Exit(1)
	}
	if handler == nil {
		handler = routes(logger, a)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("agent server error", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case s := <-sig:
		logger.Info("signal received; terminating", "signal", s.String())
		if a.claude != nil {
			a.claude.Close()
		}
		if sh := a.current(); sh != nil {
			// A paused tree would hold SIGHUP pending until the kill timeout.
			a.resumePaused()
			sh.hangup(logger)
		}
		os.Exit(0)
	case <-a.exited:
		// Exit so the kubelet restarts the container with a fresh shell (AC-D1).
		logger.Error("session shell exited; restarting container")
		os.Exit(1)
	}
}

// agent holds the current session shell, which is swappable: a normal pod adopts
// one at startup, a restore-mode pod adopts a fresh one on /restore.
type agent struct {
	shellPath string
	stateDir  string
	claude    *claudeWorkload

	mu     sync.Mutex
	sh     *shellProc
	exited chan struct{} // closed once an adopted shell exits (triggers restart)
	once   sync.Once
	// ckpt is the snapshot admission state checkpoint.go owns, guarded by mu.
	ckpt shellCheckpoint
	// admission orders writes against a starting checkpoint: a write holds it
	// shared across its check and its PTY write, so no accepted input can land
	// after the archive's scrollback was taken.
	admission sync.RWMutex
}

// homeDir is the shell's home and starting directory, the part of the shell
// that survives a snapshot (AC-D4).
func (a *agent) homeDir() string { return filepath.Join(a.stateDir, "home") }

// current returns the adopted shell, or nil in restore mode before /restore.
func (a *agent) current() *shellProc {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sh
}

// adopt installs sh as the current shell and arms the exit watch: when this
// shell exits, a.exited fires so main restarts the container. A pod adopts at
// most one shell in its lifetime (fresh start OR one restore), so once suffices.
func (a *agent) adopt(sh *shellProc) {
	a.mu.Lock()
	a.sh = sh
	a.mu.Unlock()
	go func() {
		<-sh.done
		a.once.Do(func() { close(a.exited) })
	}()
}

// scrollback is the append-only record of everything the shell has written to
// its PTY since it started (stdout and stderr merged by the PTY itself, order
// preserved). There is no size cap — the buffer bound is a deliberately open
// design decision (see docs/prd/shell-workload.md). It lives in the agent's
// memory rather than on disk, which is why checkpoint.go archives it next to
// the home directory (AC-D3/D4).
type scrollback struct {
	mu      sync.Mutex
	buf     []byte
	changed chan struct{}
}

func (b *scrollback) Append(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) == 0 {
		return
	}
	b.buf = append(b.buf, p...)
	b.notifyLocked()
}

// appendClaudeBoundedAt appends to the Claude scrollback while reserving room
// for an explicit terminal marker (AC-E3). It reports only the transition to
// full.
func (b *scrollback) appendClaudeBoundedAt(p []byte, limit int, markerText string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	marker := []byte(markerText)
	if b.claudeFullAtLocked(limit, len(marker)) || len(p) == 0 {
		return false
	}
	dataLimit := limit - len(marker)
	if dataLimit < 0 {
		dataLimit = 0
	}
	if len(b.buf)+len(p) <= dataLimit {
		b.buf = append(b.buf, p...)
		b.notifyLocked()
		return false
	}
	if remaining := dataLimit - len(b.buf); remaining > 0 {
		prefix := min(remaining, len(p))
		prefix = validUTF8PrefixAtMost(p, prefix)
		b.buf = append(b.buf, p[:prefix]...)
	}
	b.buf = append(b.buf, marker...)
	b.notifyLocked()
	return true
}

// claudeFullAt is the admission guard after the terminal marker has been
// appended. It also fails closed for a legacy restored archive that already
// consumed the reserved marker space before this bound was introduced.
func (b *scrollback) claudeFullAt(limit int, markerText string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.claudeFullAtLocked(limit, len(markerText))
}

func (b *scrollback) claudeFullAtLocked(limit, markerBytes int) bool {
	dataLimit := limit - markerBytes
	if dataLimit < 0 {
		dataLimit = 0
	}
	return len(b.buf) > dataLimit
}

func (b *scrollback) endsWithNewline() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf) == 0 || b.buf[len(b.buf)-1] == '\n'
}

// Since returns a copy of the output accumulated after offset, plus the cursor
// for the next delta read (the current accumulated length).
func (b *scrollback) Since(offset int) ([]byte, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.buf)
	if offset < 0 || offset >= n {
		return nil, n
	}
	out := make([]byte, n-offset)
	copy(out, b.buf[offset:])
	return out, n
}

// snapshot returns a copy of the full accumulated buffer, for serialization into
// a checkpoint archive (/checkpoint).
func (b *scrollback) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.buf))
	copy(out, b.buf)
	return out
}

// restore replaces the complete buffer before a restored Claude workload starts
// accepting writes (AC-E5's cursor continuity).
func (b *scrollback) restore(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf[:0], p...)
	b.notifyLocked()
}

// notifyLocked wakes every output stream waiting for an append. Closing and
// replacing the generation channel avoids lost wakeups without a goroutine per
// scrollback; a stream that starts after an append observes the bytes directly.
func (b *scrollback) notifyLocked() {
	if b.changed == nil {
		return
	}
	close(b.changed)
	b.changed = make(chan struct{})
}

// shellProc is the one PTY-attached session shell (AC-D1) and its lifecycle.
// It is lifecycle-generic rather than tied to *exec.Cmd so tests can drive the
// drain/reap/hangup machinery with any process.
type shellProc struct {
	ptmx    *os.File    // PTY master; the shell owns the slave as its ctty
	out     *scrollback // everything the shell has emitted (AC-D3)
	pid     int         // shell pid, for logging/diagnostics
	alive   atomic.Bool
	done    chan struct{} // closed once the shell has exited
	waitErr error         // wait result, valid after done is closed

	signal func(os.Signal) error // deliver a signal to the shell (hangup)
	kill   func() error          // force-kill the shell
}

// newShellProc wires a started-or-restored shell. Constructing the buffer
// before the drain goroutine starts keeps the preload race-free.
func newShellProc(ptmx *os.File, pid int, initial []byte, wait func() error, signal func(os.Signal) error, kill func() error) *shellProc {
	s := &shellProc{
		ptmx:   ptmx,
		out:    &scrollback{buf: initial},
		pid:    pid,
		done:   make(chan struct{}),
		signal: signal,
		kill:   kill,
	}
	s.alive.Store(true)

	// Drain the PTY master so the shell never blocks on a full output buffer,
	// accumulating everything into the scrollback that read serves from (AC-D3).
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				s.out.Append(buf[:n])
			}
			if err != nil {
				return // PTY closed: the shell exited or the agent is shutting down
			}
		}
	}()

	go func() {
		s.waitErr = wait()
		s.alive.Store(false)
		close(s.done)
	}()
	return s
}

// startShell launches exactly one interactive shell attached to a fresh PTY,
// started in home with HOME pointing at it. initial preloads the scrollback a
// restored shell continues from (AC-D3/D4); a fresh shell passes nil.
func startShell(path, home string, initial []byte) (*shellProc, error) {
	cmd := exec.Command(path)
	cmd.Dir = home
	// The PTY slave becomes the shell's stdin/stdout/stderr and controlling
	// terminal, which is what makes the shell interactive. TERM is set for the
	// shell's line editing; the size is a sane default until a client-driven
	// resize exists (STP-command-input and later).
	cmd.Env = shellEnvironment(home)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, err
	}
	return newShellProc(ptmx, cmd.Process.Pid, initial, cmd.Wait, cmd.Process.Signal, cmd.Process.Kill), nil
}

// shellEnvironment is the agent's environment with HOME and the starting
// directory moved to the archived home.
func shellEnvironment(home string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "HOME="), strings.HasPrefix(kv, "PWD="),
			strings.HasPrefix(kv, "OLDPWD="), strings.HasPrefix(kv, "TERM="):
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+home, "PWD="+home, "TERM=xterm-256color")
}

// shellStateDir resolves the configured state root, refusing the filesystem
// root: restore swaps the whole directory into place.
func shellStateDir(dir string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return "", fmt.Errorf("resolve shell state dir: %w", err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return "", errors.New("shell state dir must not be the filesystem root")
	}
	return abs, nil
}

// hangup terminates the shell the way a closing terminal does: SIGHUP first
// (interactive shells ignore SIGTERM), SIGKILL if it lingers.
func (s *shellProc) hangup(logger *slog.Logger) {
	_ = s.signal(syscall.SIGHUP)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		logger.Warn("shell ignored SIGHUP; killing")
		_ = s.kill()
		<-s.done
	}
	_ = s.ptmx.Close()
}

// upgrader accepts the control plane's attach dial. The peer is the control
// plane inside the cluster (not a browser), so no origin gate applies.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func routes(logger *slog.Logger, a *agent) http.Handler {
	if a.claude != nil {
		return claudeRoutes(logger, a.claude)
	}
	mux := http.NewServeMux()

	// The readiness probe. A restore-mode pod reports 200 before its checkpoint
	// arrives, or the control plane could never stream it one; that pod's
	// reachability is proven separately by Reach (AC-D1).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sh := a.current()
		switch {
		case sh == nil:
			_, _ = w.Write([]byte(`{"status":"awaiting restore"}`))
		case !sh.alive.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"shell exited"}`))
		default:
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	})

	// The attach stream — reachability verification only (AC-D1).
	mux.HandleFunc("GET /attach", func(w http.ResponseWriter, r *http.Request) {
		sh := a.current()
		if sh == nil || !sh.alive.Load() {
			http.Error(w, "no live shell", http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade already wrote the error response
		}
		defer conn.Close()
		logger.Info("attach stream opened", "remote", r.RemoteAddr)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				logger.Info("attach stream closed", "remote", r.RemoteAddr, "reason", err.Error())
				return
			}
		}
	})

	// write = shell stdin (AC-D2).
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		sh := a.current()
		if sh == nil || !sh.alive.Load() {
			http.Error(w, "no live shell", http.StatusServiceUnavailable)
			return
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.admission.RLock()
		if a.checkpointInFlight() {
			a.admission.RUnlock()
			http.Error(w, errShellAdmissionClosed.Error(), http.StatusConflict)
			return
		}
		_, err = sh.ptmx.Write(payload)
		a.admission.RUnlock()
		if err != nil {
			http.Error(w, "write to shell stdin: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// read = shell output since a cursor (AC-D3).
	mux.HandleFunc("GET /read", func(w http.ResponseWriter, r *http.Request) {
		sh := a.current()
		if sh == nil {
			http.Error(w, "no shell yet (awaiting restore)", http.StatusServiceUnavailable)
			return
		}
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				http.Error(w, "offset must be a non-negative integer", http.StatusBadRequest)
				return
			}
			offset = n
		}
		payload, next := sh.out.Since(offset)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"payload":    string(payload),
			"nextOffset": next,
		})
	})

	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		sh := a.current()
		if sh == nil {
			http.Error(w, "no shell yet (awaiting restore)", http.StatusServiceUnavailable)
			return
		}
		serveOutputStream(w, r, sh.out)
	})

	// Filesystem archive snapshot/restore (AC-D4). See checkpoint.go.
	mux.HandleFunc("POST /checkpoint", a.handleCheckpoint(logger))
	mux.HandleFunc("POST /checkpoint/abort", a.handleCheckpointAbort())
	mux.HandleFunc("POST /restore", a.handleRestore(logger))

	return mux
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func durationEnv(k string, def time.Duration) (time.Duration, error) {
	value := os.Getenv(k)
	if value == "" {
		return def, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", k, value)
	}
	return duration, nil
}

// claudeAuth is how a Claude Code workload reaches its provider and what it
// may use: the tool surface, AC-E7's auth mode, and AC-E8's effort.
type claudeAuth struct {
	tools  toolSurface
	mode   string
	effort string
}

// claudeAuthFromEnv validates claude-code's credential wiring under AC-E7's
// mode.
func claudeAuthFromEnv(workload string) (claudeAuth, error) {
	mode, err := claudeAuthModeFromEnv()
	if err != nil {
		return claudeAuth{}, err
	}
	effort, err := claudeEffortFromEnv()
	if err != nil {
		return claudeAuth{}, err
	}
	if mode == claudeAuthModeGateway {
		tools, err := agentToolSurface(workload)
		return claudeAuth{tools: tools, mode: mode, effort: effort}, err
	}
	if err := validateClaudeSubscriptionClientEnv(); err != nil {
		return claudeAuth{}, err
	}
	plugins, err := claudePluginsFromEnv()
	if err != nil {
		return claudeAuth{}, err
	}
	return claudeAuth{tools: toolSurface{Plugins: plugins}, mode: mode, effort: effort}, nil
}

// agentToolSurface validates the credential wiring of a gateway-mode Claude CLI
// workload and reports the platform-managed tool surface it must run with.
// minimax-code validates its own (validateMiniMaxClientEnv).
func agentToolSurface(workload string) (toolSurface, error) {
	switch workload {
	case workloadClaudeCode:
		if err := validateClaudeProxyClientEnv(); err != nil {
			return toolSurface{}, err
		}
		plugins, err := claudePluginsFromEnv()
		if err != nil {
			return toolSurface{}, err
		}
		return toolSurface{Plugins: plugins}, nil
	default:
		return toolSurface{}, fmt.Errorf("workload %q has no agent tool surface", workload)
	}
}
