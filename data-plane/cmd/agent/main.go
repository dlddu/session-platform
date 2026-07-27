// Command agent is the data plane session agent — the entrypoint of every
// session pod. On start it launches exactly ONE interactive shell (default
// /bin/bash, overridable via DATA_PLANE_SHELL) attached to a PTY: that shell
// process and its children are the entire session workload (AC-D1). The
// control plane never runs the shell itself; it only orchestrates this pod and
// reaches the agent over the network:
//
//	GET  /healthz    -> 200 while the shell process is alive (or, in restore
//	                    mode, while the agent is up awaiting a checkpoint); the
//	                    pod's readiness probe targets this.
//	GET  /attach     -> WebSocket upgrade held open until the peer closes it.
//	                    Reachability verification only (AC-D1).
//	POST /write       -> injects the request body into the shell's stdin (AC-D2).
//	GET  /read        -> ?offset=N returns {"payload","nextOffset"} (AC-D3).
//	POST /checkpoint  -> CRIU-dumps the shell tree and streams back a tar of the
//	                    criu images + scrollback (AC-B1/AC-D4). See checkpoint.go.
//	POST /restore     -> (restore mode) receives that tar and CRIU-restores the
//	                    shell tree, resuming the frozen session (AC-B2/AC-D4).
//
// Two startup modes:
//   - normal: launch a fresh shell immediately.
//   - restore (DATA_PLANE_RESTORE_MODE=1): start WITHOUT a shell and wait for
//     POST /restore to bring back the checkpointed one. This lets the pod become
//     Ready (healthz 200) before the control plane pushes the checkpoint, which
//     is how in-pod CRIU restore avoids a readiness deadlock.
//
// The agent's lifetime is tied to the (adopted) shell's: when it exits the agent
// exits, the container restarts (RestartPolicy Always), and a fresh agent starts
// a fresh shell — so "exactly one PTY-attached shell" (AC-D1) holds across
// restarts. Shell-state *continuity* across a snapshot is the CRIU work here.
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const (
	// defaultShell is the interactive shell launched when DATA_PLANE_SHELL is
	// unset (AC-D1).
	defaultShell = "/bin/bash"
	// defaultAddr is where the agent serves its endpoints. Keep the port in sync
	// with the control plane orchestrator's agentPort
	// (control-plane/internal/adapter/k8s/client_orchestrator.go).
	defaultAddr = ":8090"
	// restoreModeEnv, when "1", starts the agent without a shell: it waits for a
	// checkpoint on POST /restore instead (in-pod CRIU restore). The control
	// plane sets it on restore-target pods (AnnotationRestoreCheckpoint path).
	restoreModeEnv = "DATA_PLANE_RESTORE_MODE"

	// nsLastPIDPath is the per-pid-namespace knob for the last allocated pid: the
	// next fork gets the value written here + 1. Writable only in a privileged
	// pod (which is what the CRIU gate provisions).
	nsLastPIDPath = "/proc/sys/kernel/ns_last_pid"
	// defaultShellPIDFloor is written to nsLastPIDPath before the session shell
	// forks, so the shell lands well above the pids an agent process occupies.
	// CRIU restores a task under its ORIGINAL pid, and a shell started right
	// after the agent would otherwise land around pid ~10 — exactly where the
	// restore pod's own agent has its Go runtime threads (tids ≤ ~15), making the
	// restore collide (observed on-cluster 2026-07-23; earlier successes were
	// luck). Overridable via CRIU_PID_FLOOR while tuning against a runtime.
	defaultShellPIDFloor = 300
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := env("DATA_PLANE_AGENT_ADDR", defaultAddr)
	a := &agent{
		shellPath: env("DATA_PLANE_SHELL", defaultShell),
		engine:    newExecCriuEngine(),
		exited:    make(chan struct{}),
	}

	if env(restoreModeEnv, "") == "1" {
		// Restore mode: no shell yet — POST /restore brings back the checkpointed
		// one. healthz reports 200 (agent up, awaiting restore) so the pod becomes
		// Ready before the control plane pushes the checkpoint. The restored
		// shell's reachability is then proven by the control plane's Reach.
		logger.Info("agent started in restore mode; awaiting checkpoint", "addr", addr)
	} else {
		// Push the next pid up before forking the shell, so a later CRIU restore
		// (which reinstates the shell under its original pid) does not land on a
		// pid the restore pod's agent already occupies. Best-effort: the knob is
		// only writable in a privileged pod, and without CRIU a low shell pid is
		// harmless.
		floor := shellPIDFloor()
		if err := reserveShellPID(floor); err != nil {
			logger.Info("could not raise ns_last_pid; the shell will take a low pid"+
				" (harmless without CRIU, but a restore may hit a pid collision)",
				"path", nsLastPIDPath, "floor", floor, "err", err)
		}
		sh, err := startShell(a.shellPath)
		if err != nil {
			logger.Error("failed to start session shell", "shell", a.shellPath, "err", err)
			os.Exit(1)
		}
		a.adopt(sh)
		logger.Info("session shell started", "shell", a.shellPath, "pid", sh.pid, "addr", addr)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(logger, a),
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
		// Pod shutdown: hang up the shell's terminal (if any) and exit cleanly.
		logger.Info("signal received; terminating", "signal", s.String())
		if sh := a.current(); sh != nil {
			sh.hangup(logger)
		}
		os.Exit(0)
	case <-a.exited:
		// The adopted shell exited on its own. Exit so the kubelet restarts the
		// container with a fresh agent+shell, keeping AC-D1 true.
		logger.Error("session shell exited; restarting container")
		os.Exit(1)
	}
}

// agent holds the current session shell, which is swappable: a normal pod adopts
// one at startup, a restore-mode pod adopts the CRIU-restored one on POST
// /restore. It also carries the CRIU engine the checkpoint/restore handlers use.
type agent struct {
	shellPath string
	engine    criuEngine

	mu     sync.Mutex
	sh     *shellProc
	exited chan struct{} // closed once an adopted shell exits (triggers restart)
	once   sync.Once
	// checkpointing suppresses the shell-exit→container-restart path while a
	// /checkpoint is in flight. criu dump freezes and kills the shell tree, which
	// would otherwise trip the exit watch and os.Exit(1) mid-request — truncating
	// the archive still streaming in the response body. Set before the dump; the
	// control plane reclaims the pod once it has the archive, so no self-restart
	// is needed on the checkpoint path.
	checkpointing atomic.Bool
}

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
		if a.checkpointing.Load() {
			// Expected exit: criu dump froze/killed the shell for a checkpoint.
			// Don't restart — the archive is still streaming and the control
			// plane reclaims this pod once it has it.
			return
		}
		a.once.Do(func() { close(a.exited) })
	}()
}

// scrollback is the append-only record of everything the shell has written to
// its PTY since it started (stdout and stderr merged by the PTY itself, order
// preserved). Read serves deltas from it by offset (AC-D3); nothing is ever
// discarded, so offset 0 always replays the full session history. There is no
// size cap — the buffer bound is a deliberately open design decision (see
// docs/prd/shell-workload.md).
//
// The buffer lives in the agent process's memory. In-pod CRIU checkpoints the
// shell *process tree* — not the agent — so the scrollback does not ride the
// criu images automatically; instead /checkpoint serializes it alongside them
// and /restore preloads it into the restored shell's buffer (AC-D4). Either way
// a read cursor (nextOffset) issued before the snapshot stays valid: the same
// bytes come back at the same length, so a client resumes with only the delta
// and offset 0 still replays the full pre- and post-snapshot history.
type scrollback struct {
	mu  sync.Mutex
	buf []byte
}

func (b *scrollback) Append(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
}

// Since returns a copy of the output accumulated after offset, plus the cursor
// for the next delta read (the current accumulated length). An offset at or
// past the current length yields an empty payload with that cursor.
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

// shellProc is the one PTY-attached session shell (AC-D1) and its lifecycle.
// It is lifecycle-generic rather than tied to *exec.Cmd so both a freshly
// started shell (startShell) and a CRIU-restored one (criuEngine.Restore) share
// the same drain/reap/hangup machinery.
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

// newShellProc wires a started-or-restored shell: it preloads the scrollback
// with initial (nil for a fresh shell, the checkpointed history for a restored
// one), starts draining the PTY master into it (AC-D3), and reaps the process in
// the background, flipping alive/done when wait returns. Constructing the buffer
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

// shellPIDFloor is the pid floor to claim before forking the shell, from
// CRIU_PID_FLOOR or defaultShellPIDFloor. A non-positive or unparseable value
// falls back to the default rather than disabling the guard silently.
func shellPIDFloor() int {
	if v := os.Getenv("CRIU_PID_FLOOR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultShellPIDFloor
}

// reserveShellPID writes floor to ns_last_pid so the next fork in this pid
// namespace gets floor+1. The agent's own threads may consume a few pids after
// this, which only pushes the shell higher — the guarantee needed is "well above
// the agent's tids", not an exact number. Fails on a non-privileged pod, where
// the file is read-only; callers treat that as best-effort.
func reserveShellPID(floor int) error {
	return os.WriteFile(nsLastPIDPath, []byte(strconv.Itoa(floor)), 0o644)
}

// startShell launches exactly one interactive shell attached to a fresh PTY.
func startShell(path string) (*shellProc, error) {
	cmd := exec.Command(path)
	// The PTY slave becomes the shell's stdin/stdout/stderr and controlling
	// terminal, which is what makes the shell interactive. TERM is set for the
	// shell's line editing; the size is a sane default until a client-driven
	// resize exists (J5-S2+).
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, err
	}
	return newShellProc(ptmx, cmd.Process.Pid, nil, cmd.Wait, cmd.Process.Signal, cmd.Process.Kill), nil
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
	mux := http.NewServeMux()

	// The readiness probe. In restore mode before the checkpoint arrives the
	// agent is up and ready to receive it, so it reports 200 (the restored
	// shell's reachability is proven separately by the control plane's Reach).
	// Once a shell is adopted, 200 only while it is alive, so pod Ready reflects
	// shell liveness (AC-D1).
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

	// The attach stream — reachability verification only (AC-D1): the agent
	// holds the stream open, discarding any frames, until the peer closes.
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

	// write = shell stdin (AC-D2): the raw request body goes into the PTY master
	// verbatim and the handler returns immediately — it never waits for the
	// shell to run the command (output is recovered via /read).
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
		if _, err := sh.ptmx.Write(payload); err != nil {
			http.Error(w, "write to shell stdin: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// read = shell output since a cursor (AC-D3): non-consuming, so it serves
	// even after the shell has exited — the scrollback is history, not a pipe.
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

	// CRIU checkpoint/restore (in-pod). See checkpoint.go.
	mux.HandleFunc("POST /checkpoint", a.handleCheckpoint(logger))
	mux.HandleFunc("POST /restore", a.handleRestore(logger))

	return mux
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
