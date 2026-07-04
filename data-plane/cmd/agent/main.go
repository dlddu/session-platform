// Command agent is the data plane session agent — the entrypoint of every
// session pod. On start it launches exactly ONE interactive shell (default
// /bin/bash, overridable via DATA_PLANE_SHELL) attached to a PTY: that shell
// process and its children are the entire session workload (AC-D1). The
// control plane never runs the shell itself; it only orchestrates this pod and
// reaches the agent over the network:
//
//	GET  /healthz -> 200 while the shell process is alive; the pod's readiness
//	                 probe targets this, so pod Ready implies a live shell.
//	GET  /attach  -> WebSocket upgrade held open until the peer closes it.
//	                 Reachability verification only (AC-D1): the control plane
//	                 opens and closes it to prove the shell is reachable. The
//	                 shell I/O itself moves over plain HTTP below.
//	POST /write   -> injects the raw request body into the shell's stdin (PTY
//	                 master) and returns immediately, never waiting for the
//	                 command to run (AC-D2).
//	GET  /read    -> ?offset=N returns {"payload","nextOffset"}: the shell
//	                 output accumulated after offset (0 = everything since
//	                 session start) plus the cursor for the next delta read.
//	                 Non-consuming — nothing is ever discarded (AC-D3).
//
// The agent's lifetime is tied to the shell's: when the shell exits the agent
// exits, the container restarts (RestartPolicy Always), and a fresh agent
// starts a fresh shell — so "exactly one PTY-attached shell" (AC-D1) holds
// across restarts. Shell-state continuity across restarts is out of scope here
// (that is the CRIU snapshot/restore work, AC-B*/AC-D4).
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
	// defaultAddr is where the agent serves /attach and /healthz. Keep the port
	// in sync with the control plane orchestrator's agentPort
	// (control-plane/internal/adapter/k8s/client_orchestrator.go).
	defaultAddr = ":8090"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	shellPath := env("DATA_PLANE_SHELL", defaultShell)
	addr := env("DATA_PLANE_AGENT_ADDR", defaultAddr)

	sh, err := startShell(shellPath)
	if err != nil {
		logger.Error("failed to start session shell", "shell", shellPath, "err", err)
		os.Exit(1)
	}
	logger.Info("session shell started", "shell", shellPath, "pid", sh.cmd.Process.Pid, "addr", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(logger, sh),
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
		// Pod shutdown: hang up the shell's terminal and exit cleanly.
		logger.Info("signal received; terminating shell", "signal", s.String())
		sh.hangup(logger)
		os.Exit(0)
	case <-sh.done:
		// The shell exited on its own. Exit so the kubelet restarts the
		// container with a fresh agent+shell, keeping AC-D1 true.
		logger.Error("session shell exited; restarting container", "err", sh.waitErr)
		os.Exit(1)
	}
}

// scrollback is the append-only record of everything the shell has written to
// its PTY since it started (stdout and stderr merged by the PTY itself, order
// preserved). Read serves deltas from it by offset (AC-D3); nothing is ever
// discarded, so offset 0 always replays the full session history. There is no
// size cap — the buffer bound is a deliberately open design decision (see
// docs/prd/shell-workload.md).
//
// The buffer lives in the agent process's own memory, so a CRIU checkpoint of
// the session pod captures it along with the shell's process tree (AC-D4). A
// restore therefore brings back the same accumulated bytes at the same length,
// which is what keeps a read cursor (nextOffset) issued before the snapshot
// valid afterwards: a client resumes with only the delta, and offset 0 still
// replays the full pre- and post-snapshot history. This is distinct from a
// container *restart* (RestartPolicy Always), which starts a fresh agent with
// an empty buffer — restore resumes, restart does not.
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

// shellProc is the one PTY-attached session shell (AC-D1) and its lifecycle.
type shellProc struct {
	cmd     *exec.Cmd
	ptmx    *os.File    // PTY master; the shell owns the slave as its ctty
	out     *scrollback // everything the shell has emitted (AC-D3)
	alive   atomic.Bool
	done    chan struct{} // closed once the shell has exited
	waitErr error         // cmd.Wait result, valid after done is closed
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
	s := &shellProc{cmd: cmd, ptmx: ptmx, out: &scrollback{}, done: make(chan struct{})}
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
		s.waitErr = cmd.Wait()
		s.alive.Store(false)
		close(s.done)
	}()
	return s, nil
}

// hangup terminates the shell the way a closing terminal does: SIGHUP first
// (interactive shells ignore SIGTERM), SIGKILL if it lingers.
func (s *shellProc) hangup(logger *slog.Logger) {
	_ = s.cmd.Process.Signal(syscall.SIGHUP)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		logger.Warn("shell ignored SIGHUP; killing")
		_ = s.cmd.Process.Kill()
		<-s.done
	}
	_ = s.ptmx.Close()
}

// upgrader accepts the control plane's attach dial. The peer is the control
// plane inside the cluster (not a browser), so no origin gate applies.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func routes(logger *slog.Logger, sh *shellProc) http.Handler {
	mux := http.NewServeMux()

	// The readiness probe: 200 only while the shell process is alive, so pod
	// Ready reflects shell liveness (AC-D1).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !sh.alive.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"shell exited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// The attach stream — reachability verification only (AC-D1): the agent
	// holds the stream open, discarding any frames, until the peer closes.
	// Opening and closing it is how the control plane proves the shell is
	// reachable; the shell I/O itself moves over /write and /read below.
	mux.HandleFunc("GET /attach", func(w http.ResponseWriter, r *http.Request) {
		if !sh.alive.Load() {
			http.Error(w, "shell exited", http.StatusServiceUnavailable)
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

	// write = shell stdin (AC-D2): the raw request body goes into the PTY
	// master verbatim and the handler returns immediately — it never waits for
	// the shell to run the command (output is recovered via /read).
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		if !sh.alive.Load() {
			http.Error(w, "shell exited", http.StatusServiceUnavailable)
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

	return mux
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
