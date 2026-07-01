// Command agent is the data plane session agent — the entrypoint of every
// session pod. On start it launches exactly ONE interactive shell (default
// /bin/bash, overridable via DATA_PLANE_SHELL) attached to a PTY: that shell
// process and its children are the entire session workload (AC-D1). The
// control plane never runs the shell itself; it only orchestrates this pod and
// reaches the agent over the network:
//
//	GET /healthz -> 200 while the shell process is alive; the pod's readiness
//	                probe targets this, so pod Ready implies a live shell.
//	GET /attach  -> WebSocket upgrade; the stream is held open until the peer
//	                closes it. J5-S1 proves reachability only (open/close) —
//	                the endpoint is payload-agnostic, and the stdin/stdout
//	                semantics (AC-D2/D3) land on top of it in J5-S2/S3.
//
// The agent's lifetime is tied to the shell's: when the shell exits the agent
// exits, the container restarts (RestartPolicy Always), and a fresh agent
// starts a fresh shell — so "exactly one PTY-attached shell" (AC-D1) holds
// across restarts. Shell-state continuity across restarts is out of scope here
// (that is the CRIU snapshot/restore work, AC-B*/AC-D4).
package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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

// shellProc is the one PTY-attached session shell (AC-D1) and its lifecycle.
type shellProc struct {
	cmd     *exec.Cmd
	ptmx    *os.File // PTY master; the shell owns the slave as its ctty
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
	s := &shellProc{cmd: cmd, ptmx: ptmx, done: make(chan struct{})}
	s.alive.Store(true)

	// Drain the PTY master so the shell never blocks on a full output buffer.
	// The output is discarded for now: accumulating it for read is J5-S3.
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()

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

	// The attach stream. Payload-agnostic in J5-S1: the agent holds the stream
	// open, discarding any frames, until the peer closes — opening and closing
	// it is how the control plane proves the shell is reachable. J5-S2/S3 layer
	// the stdin/stdout semantics on top of this same endpoint.
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

	return mux
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
