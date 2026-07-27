package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// fakeCriuEngine stands in for the real criu binary so the checkpoint/restore
// handlers and archive plumbing are tested without a CRIU runtime. Dump writes a
// placeholder image file; Restore starts a real fresh shell (preloaded with the
// checkpointed scrollback) to stand in for the restored one.
type fakeCriuEngine struct {
	dumpErr    error
	restoreErr error
	onDump     func() // mimics criu freezing/killing the shell tree during dump

	gotDumpPid     int
	gotRestoreInit []byte
}

func (f *fakeCriuEngine) Dump(_ context.Context, pid int, imagesDir string) error {
	f.gotDumpPid = pid
	if f.onDump != nil {
		f.onDump()
	}
	if f.dumpErr != nil {
		return f.dumpErr
	}
	return os.WriteFile(filepath.Join(imagesDir, "core-1.img"), []byte("fake-criu-image"), 0o600)
}

func (f *fakeCriuEngine) Restore(_ context.Context, _ string, initial []byte) (*shellProc, error) {
	f.gotRestoreInit = append([]byte(nil), initial...)
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	cmd := exec.Command(defaultShell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, err
	}
	return newShellProc(ptmx, cmd.Process.Pid, initial, cmd.Wait, cmd.Process.Signal, cmd.Process.Kill), nil
}

var _ criuEngine = (*fakeCriuEngine)(nil)

// The archive round-trips: scrollback bytes and every image file come back
// byte-for-byte after writeArchive -> readArchive.
func TestArchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "core-1.img"), []byte("image-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pages-1.img"), []byte("image-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	scrollback := []byte("frozen output\nwith two lines\n")

	var buf bytes.Buffer
	if err := writeArchive(&buf, src, scrollback); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}

	dst := t.TempDir()
	got, err := readArchive(&buf, dst)
	if err != nil {
		t.Fatalf("readArchive: %v", err)
	}
	if !bytes.Equal(got, scrollback) {
		t.Errorf("scrollback = %q, want %q", got, scrollback)
	}
	for name, want := range map[string]string{"core-1.img": "image-one", "pages-1.img": "image-two"} {
		b, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("restored image %s: %v", name, err)
		}
		if string(b) != want {
			t.Errorf("image %s = %q, want %q", name, b, want)
		}
	}
}

// readArchive rejects a path-traversal entry rather than writing outside the
// images dir.
func TestReadArchiveRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: archiveScrollbackName, Size: 0, Mode: 0o600})
	_ = tw.WriteHeader(&tar.Header{Name: archiveImagesPrefix + "../escape", Size: 3, Mode: 0o600})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()

	if _, err := readArchive(&buf, t.TempDir()); err == nil {
		t.Fatal("readArchive accepted a traversal entry; want error")
	}
}

// /checkpoint dumps the live shell (passing its pid to the engine) and streams a
// tar carrying the scrollback plus the criu images.
func TestCheckpointHandlerStreamsArchive(t *testing.T) {
	sh := startTestShell(t)
	fake := &fakeCriuEngine{}
	a := &agent{sh: sh, engine: fake}
	srv := httptest.NewServer(routes(testLogger(), a))
	defer srv.Close()

	// Produce some shell output so the scrollback is non-trivial.
	writeViaHTTP(t, srv, "echo checkpoint-marker-$((1+1))\n", http.StatusOK)
	eventuallyRead(t, srv, 0, func(p string) bool { return strings.Contains(p, "checkpoint-marker-2") })

	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatalf("POST /checkpoint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status = %d, want 200", resp.StatusCode)
	}

	dst := t.TempDir()
	sb, err := readArchive(resp.Body, dst)
	if err != nil {
		t.Fatalf("readArchive from response: %v", err)
	}
	if !strings.Contains(string(sb), "checkpoint-marker-2") {
		t.Errorf("archived scrollback %q missing the shell output", sb)
	}
	if _, err := os.Stat(filepath.Join(dst, "core-1.img")); err != nil {
		t.Errorf("archived criu image missing: %v", err)
	}
	if fake.gotDumpPid != sh.pid {
		t.Errorf("engine dumped pid %d, want the shell pid %d", fake.gotDumpPid, sh.pid)
	}
}

// The dump freezes and kills the shell, but that must NOT trip the container
// restart path until the archive has streamed — otherwise the agent os.Exit(1)s
// mid-response and truncates the checkpoint. The exit watch is suppressed while
// a checkpoint is in flight.
func TestCheckpointDefersRestartWhileStreaming(t *testing.T) {
	sh := startTestShell(t)
	// The fake dump mimics criu: it freezes/kills the shell tree, closing sh.done
	// exactly as a real dump would.
	fake := &fakeCriuEngine{onDump: func() { _ = sh.kill(); <-sh.done }}
	a := &agent{engine: fake, exited: make(chan struct{})}
	a.adopt(sh) // arms the real exit watch (the restart trigger)
	srv := httptest.NewServer(routes(testLogger(), a))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatalf("POST /checkpoint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status = %d, want 200", resp.StatusCode)
	}
	// The archive streamed fully despite the shell dying mid-request.
	if _, err := readArchive(resp.Body, t.TempDir()); err != nil {
		t.Fatalf("archive truncated (early restart raced the stream?): %v", err)
	}
	// The restart path stayed suppressed: a.exited must still be open.
	select {
	case <-a.exited:
		t.Fatal("shell-exit→restart fired during checkpoint; the archive would truncate")
	default:
	}
}

// A failed dump leaves the shell running, so the restart path is re-armed: a
// subsequent real shell exit still triggers a restart (a.exited closes).
func TestCheckpointDumpFailureReArmsRestart(t *testing.T) {
	sh := startTestShell(t)
	fake := &fakeCriuEngine{dumpErr: errors.New("dump boom")} // does not kill the shell
	a := &agent{engine: fake, exited: make(chan struct{})}
	a.adopt(sh)
	srv := httptest.NewServer(routes(testLogger(), a))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatalf("POST /checkpoint: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed-dump checkpoint = %d, want 500", resp.StatusCode)
	}
	// The shell was never frozen; killing it now must still trigger a restart,
	// proving checkpointing was re-armed (not left suppressed).
	_ = sh.kill()
	select {
	case <-a.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("shell exit after a failed dump did not trigger restart; checkpointing stuck on")
	}
}

// /checkpoint with no live shell is a 503, not a dump of nothing.
func TestCheckpointHandlerRequiresLiveShell(t *testing.T) {
	a := &agent{engine: &fakeCriuEngine{}} // restore mode, no shell adopted yet
	srv := httptest.NewServer(routes(testLogger(), a))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatalf("POST /checkpoint: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("checkpoint without a shell = %d, want 503", resp.StatusCode)
	}
}

// A restore-mode agent: healthz is 200 "awaiting restore" and read is 503 until
// /restore adopts the checkpointed shell; then the preloaded scrollback is
// served and healthz reflects the live restored shell.
func TestRestoreHandlerAdoptsCheckpointedShell(t *testing.T) {
	fake := &fakeCriuEngine{}
	a := &agent{engine: fake, exited: make(chan struct{})}
	t.Cleanup(func() {
		if sh := a.current(); sh != nil {
			_ = sh.kill()
			<-sh.done
		}
	})
	srv := httptest.NewServer(routes(testLogger(), a))
	defer srv.Close()

	// Before restore: agent up (Ready) but no shell to read from.
	if code := healthzCode(t, srv); code != http.StatusOK {
		t.Fatalf("restore-mode healthz = %d, want 200 (awaiting restore)", code)
	}
	if resp, _ := http.Get(srv.URL + "/read?offset=0"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("read before restore = %d, want 503", resp.StatusCode)
	}

	// Push a checkpoint archive carrying pre-freeze scrollback.
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "core-1.img"), []byte("img"), 0o600)
	var archive bytes.Buffer
	if err := writeArchive(&archive, src, []byte("PRE-FREEZE-HISTORY\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/restore", "application/x-tar", &archive)
	if err != nil {
		t.Fatalf("POST /restore: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore status = %d, want 200", resp.StatusCode)
	}
	if string(fake.gotRestoreInit) != "PRE-FREEZE-HISTORY\n" {
		t.Errorf("engine restore initial = %q, want the archived scrollback", fake.gotRestoreInit)
	}

	// After restore: the restored shell is live and its buffer replays the
	// pre-freeze history at offset 0 (AC-D4 continuity — buffer preloaded).
	if code := healthzCode(t, srv); code != http.StatusOK {
		t.Fatalf("post-restore healthz = %d, want 200", code)
	}
	payload, _ := readViaHTTP(t, srv, 0)
	if !strings.HasPrefix(payload, "PRE-FREEZE-HISTORY\n") {
		t.Errorf("restored read at offset 0 = %q, want the pre-freeze history first", payload)
	}
}

// /restore is refused once a shell already exists (a restore pod restores once).
func TestRestoreHandlerRefusesWhenShellExists(t *testing.T) {
	sh := startTestShell(t)
	a := &agent{sh: sh, engine: &fakeCriuEngine{}}
	srv := httptest.NewServer(routes(testLogger(), a))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/restore", "application/x-tar", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST /restore: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("restore with an existing shell = %d, want 409", resp.StatusCode)
	}
}

func healthzCode(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}
