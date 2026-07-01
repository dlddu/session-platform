package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startTestShell starts the real default shell and guarantees it is reaped.
func startTestShell(t *testing.T) *shellProc {
	t.Helper()
	sh, err := startShell(defaultShell)
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(func() {
		_ = sh.cmd.Process.Kill()
		<-sh.done
	})
	return sh
}

// AC-D1: the started shell is attached to a PTY — its stdin is a PTY slave —
// and exactly one shell exists (one startShell call spawns one process).
func TestStartShellAttachesPTY(t *testing.T) {
	sh := startTestShell(t)
	if !sh.alive.Load() {
		t.Fatal("shell not alive after start")
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", sh.cmd.Process.Pid))
	if err != nil {
		t.Fatalf("read shell stdin link: %v", err)
	}
	if !strings.HasPrefix(link, "/dev/pts/") {
		t.Fatalf("shell stdin = %q, want a PTY slave (/dev/pts/*) (AC-D1)", link)
	}
}

// /healthz mirrors shell liveness: 200 while the shell runs, 503 once it has
// exited — which is what makes the pod readiness probe mean "shell alive".
func TestHealthzReflectsShellLiveness(t *testing.T) {
	sh := startTestShell(t)
	h := routes(testLogger(), sh)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz with live shell = %d, want 200", rec.Code)
	}

	_ = sh.cmd.Process.Kill()
	<-sh.done

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with dead shell = %d, want 503", rec.Code)
	}
}

// /attach upgrades to a WebSocket and survives an immediate open/close — the
// exact reachability handshake the control plane performs (J5-S1).
func TestAttachUpgradesAndCloses(t *testing.T) {
	sh := startTestShell(t)
	srv := httptest.NewServer(routes(testLogger(), sh))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/attach"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %d, want 101", resp.StatusCode)
	}
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	if err := conn.Close(); err != nil {
		t.Fatalf("close attach: %v", err)
	}
}
