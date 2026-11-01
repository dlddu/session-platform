package main

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testCheckpointID = "0123456789abcdef0123456789abcdef"

// newShellAgentServer starts a real shell in a fresh state root and serves it.
func newShellAgentServer(t *testing.T) (*agent, *httptest.Server) {
	t.Helper()
	a := &agent{shellPath: defaultShell, stateDir: filepath.Join(t.TempDir(), "state")}
	if err := os.MkdirAll(a.homeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	sh, err := startShell(a.shellPath, a.homeDir(), nil)
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	a.sh = sh
	t.Cleanup(func() {
		a.resumePaused()
		_ = sh.kill()
		<-sh.done
	})
	srv := httptest.NewServer(routes(testLogger(), a))
	t.Cleanup(srv.Close)
	return a, srv
}

// newRestoreAgentServer serves a restore-mode shell agent with no shell yet.
func newRestoreAgentServer(t *testing.T) (*agent, *httptest.Server) {
	t.Helper()
	a := &agent{shellPath: defaultShell, stateDir: filepath.Join(t.TempDir(), "state"), exited: make(chan struct{})}
	t.Cleanup(func() {
		if sh := a.current(); sh != nil {
			_ = sh.kill()
			<-sh.done
		}
	})
	srv := httptest.NewServer(routes(testLogger(), a))
	t.Cleanup(srv.Close)
	return a, srv
}

func postCheckpoint(t *testing.T, srv *httptest.Server, path, id string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		req.Header.Set(claudeCheckpointIDHeader, id)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func postRestore(t *testing.T, srv *httptest.Server, archive []byte) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/restore", "application/x-tar", bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("POST /restore: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// stateOf is the one-letter state in /proc/<pid>/stat ("T" = stopped).
func stateOf(t *testing.T, pid int) string {
	t.Helper()
	state, ok := processState(pid)
	if !ok {
		t.Fatalf("read state of %d", pid)
	}
	return state
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// AC-D4: home-directory files and the scrollback survive; the restored shell is
// fresh, starts in its home, and carries none of the frozen shell's variables.
func TestShellArchiveRoundTrip(t *testing.T) {
	_, src := newShellAgentServer(t)
	writeViaHTTP(t, src, "echo kept-$((1+1)) > note.txt; export MARKER=42; echo frozen-$((2+2))\n", http.StatusOK)
	_, cursor := eventuallyRead(t, src, 0, func(p string) bool { return strings.Contains(p, "frozen-4") })

	resp, archive := postCheckpoint(t, src, "/checkpoint", testCheckpointID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status=%d body=%q", resp.StatusCode, archive)
	}
	if got := resp.Header.Get(claudeCheckpointIDHeader); got != testCheckpointID {
		t.Fatalf("checkpoint echoed generation %q, want %q", got, testCheckpointID)
	}

	dst, srv := newRestoreAgentServer(t)
	if code, body := postRestore(t, srv, archive); code != http.StatusOK {
		t.Fatalf("restore status=%d body=%q", code, body)
	}
	note, err := os.ReadFile(filepath.Join(dst.homeDir(), "note.txt"))
	if err != nil || string(note) != "kept-2\n" {
		t.Fatalf("restored note = %q, %v; want kept-2", note, err)
	}
	if full, _ := readViaHTTP(t, srv, 0); !strings.Contains(full, "frozen-4") {
		t.Fatalf("offset=0 after restore lost pre-snapshot history: %q", full)
	}
	if delta, _ := readViaHTTP(t, srv, cursor); strings.Contains(delta, "frozen-4") {
		t.Fatalf("pre-snapshot cursor replayed old output: %q", delta)
	}

	writeViaHTTP(t, srv, "echo \"var=[$MARKER]\"; pwd\n", http.StatusOK)
	out, _ := eventuallyRead(t, srv, cursor, func(p string) bool {
		return strings.Contains(p, "var=[]") && strings.Contains(p, dst.homeDir())
	})
	if strings.Contains(out, "var=[42]") {
		t.Fatalf("restored shell kept the frozen shell's variable: %q", out)
	}
}

// While an archive is in flight or awaiting the control plane's decision, the
// shell's tree is paused and writes are refused; abort resumes both.
func TestCheckpointPausesTreeUntilAbort(t *testing.T) {
	a, srv := newShellAgentServer(t)
	writeViaHTTP(t, srv, "sleep 300 &\necho bg-started\n", http.StatusOK)
	eventuallyRead(t, srv, 0, func(p string) bool { return strings.Contains(p, "bg-started") })
	var child int
	eventually(t, "background child", func() bool {
		tree := processTree(a.sh.pid)
		if len(tree) > 1 {
			child = tree[len(tree)-1]
			return true
		}
		return false
	})

	if resp, body := postCheckpoint(t, srv, "/checkpoint", testCheckpointID); resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status=%d body=%q", resp.StatusCode, body)
	}
	if got := stateOf(t, child); got != "T" {
		t.Fatalf("background child state = %q during checkpoint, want stopped", got)
	}
	if got := stateOf(t, a.sh.pid); got != "T" {
		t.Fatalf("shell state = %q during checkpoint, want stopped", got)
	}
	writeViaHTTP(t, srv, "echo refused\n", http.StatusConflict)
	if code := healthzCode(t, srv); code != http.StatusOK {
		t.Fatalf("healthz during checkpoint = %d, want 200 (the shell is paused, not dead)", code)
	}

	if resp, body := postCheckpoint(t, srv, "/checkpoint/abort", testCheckpointID); resp.StatusCode != http.StatusOK {
		t.Fatalf("abort status=%d body=%q", resp.StatusCode, body)
	}
	eventually(t, "child resumed", func() bool { return stateOf(t, child) != "T" })
	writeViaHTTP(t, srv, "echo reopened-$((3+3))\n", http.StatusOK)
	eventuallyRead(t, srv, 0, func(p string) bool { return strings.Contains(p, "reopened-6") })
}

func TestCheckpointAbortIsFenced(t *testing.T) {
	_, srv := newShellAgentServer(t)
	// Nothing in flight: the control plane's recovery abort is a no-op success.
	if resp, body := postCheckpoint(t, srv, "/checkpoint/abort", testCheckpointID); resp.StatusCode != http.StatusOK {
		t.Fatalf("idle abort status=%d body=%q", resp.StatusCode, body)
	}
	if resp, body := postCheckpoint(t, srv, "/checkpoint/abort", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("abort without generation status=%d body=%q, want 409", resp.StatusCode, body)
	}
	if resp, body := postCheckpoint(t, srv, "/checkpoint", testCheckpointID); resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status=%d body=%q", resp.StatusCode, body)
	}
	other := strings.Repeat("f", 32)
	if resp, body := postCheckpoint(t, srv, "/checkpoint/abort", other); resp.StatusCode != http.StatusConflict {
		t.Fatalf("abort of another generation status=%d body=%q, want 409", resp.StatusCode, body)
	}
	if resp, body := postCheckpoint(t, srv, "/checkpoint", testCheckpointID); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second checkpoint status=%d body=%q, want 409", resp.StatusCode, body)
	}
	writeViaHTTP(t, srv, "echo still-closed\n", http.StatusConflict)
}

func TestCheckpointRejectsMalformedGeneration(t *testing.T) {
	_, srv := newShellAgentServer(t)
	if resp, body := postCheckpoint(t, srv, "/checkpoint", "not-hex"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400", resp.StatusCode, body)
	}
	writeViaHTTP(t, srv, "echo open\n", http.StatusOK)
}

// A local archive failure means no snapshot exists, so the shell must resume
// and accept writes again rather than staying paused.
func TestCheckpointFailureResumesShell(t *testing.T) {
	a, srv := newShellAgentServer(t)
	if err := syscall.Mkfifo(filepath.Join(a.homeDir(), "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, body := postCheckpoint(t, srv, "/checkpoint", testCheckpointID); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("checkpoint status=%d body=%q, want 500", resp.StatusCode, body)
	}
	if got := stateOf(t, a.sh.pid); got == "T" {
		t.Fatal("shell left stopped after a failed checkpoint")
	}
	writeViaHTTP(t, srv, "echo after-failure-$((4+4))\n", http.StatusOK)
	eventuallyRead(t, srv, 0, func(p string) bool { return strings.Contains(p, "after-failure-8") })
}

func TestCheckpointRequiresLiveShell(t *testing.T) {
	_, srv := newRestoreAgentServer(t)
	if resp, body := postCheckpoint(t, srv, "/checkpoint", testCheckpointID); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%q, want 503", resp.StatusCode, body)
	}
}

func TestRestoreRefusesWhenShellExists(t *testing.T) {
	_, srv := newShellAgentServer(t)
	if code, body := postRestore(t, srv, nil); code != http.StatusConflict {
		t.Fatalf("restore over a live shell status=%d body=%q, want 409", code, body)
	}
}

func TestRestoreRequiresHomeDirectory(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: claudeArchiveScrollback, Mode: 0o600, Typeflag: tar.TypeReg},
		{Name: claudeArchiveStateRoot, Mode: 0o700, Typeflag: tar.TypeDir},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	a, srv := newRestoreAgentServer(t)
	code, body := postRestore(t, srv, buf.Bytes())
	if code != http.StatusBadRequest || !strings.Contains(body, "home") {
		t.Fatalf("restore status=%d body=%q, want 400 naming home", code, body)
	}
	if a.current() != nil {
		t.Fatal("a rejected archive still adopted a shell")
	}
	if _, err := os.Lstat(a.stateDir); !os.IsNotExist(err) {
		t.Fatalf("state root mutated by a rejected archive: %v", err)
	}
}

func healthzCode(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
