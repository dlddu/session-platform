package main

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// This file implements in-pod CRIU checkpoint/restore of the session shell tree
// (AC-B1/B2/B3, AC-D4), done by the agent because it already owns the shell
// process and its PTY master. Why not a container-level kubelet checkpoint:
// docs/criu-verification.md, decision ⑤.
//
// The archive is a tar carrying the criu image directory plus the scrollback
// bytes, so the accumulated shell output (AC-D3/D4) survives the round trip even
// though it lives in the agent's memory, not the dumped tree. The control plane
// streams the archive to/from durable storage (S3); the agent never touches
// object storage or cloud credentials.

// archiveScrollbackName is the tar entry holding the serialized scrollback.
const archiveScrollbackName = "scrollback"
const archiveImagesPrefix = "images/"

// criuEngine performs the runtime-specific half of in-pod checkpoint/restore.
// The default execCriuEngine shells out to criu; tests inject a fake so the
// handlers and archive plumbing run without a CRIU runtime.
type criuEngine interface {
	// Dump CRIU-dumps the process tree rooted at pid into imagesDir. After a
	// successful dump the process is frozen/gone (the pod is about to be
	// reclaimed), so the caller snapshots the scrollback afterwards.
	Dump(ctx context.Context, pid int, imagesDir string) error
	// Restore CRIU-restores the tree from imagesDir, reattaches it to a fresh
	// PTY, and returns a live shellProc whose scrollback is preloaded with
	// initial (the checkpointed history).
	Restore(ctx context.Context, imagesDir string, initial []byte) (*shellProc, error)
}

// handleCheckpoint dumps the live shell tree and streams back a tar of the criu
// images + scrollback. The control plane persists it and reclaims the pod.
func (a *agent) handleCheckpoint(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sh := a.current()
		if sh == nil || !sh.alive.Load() {
			http.Error(w, "no live shell to checkpoint", http.StatusServiceUnavailable)
			return
		}
		imagesDir, err := os.MkdirTemp("", "criu-dump-")
		if err != nil {
			http.Error(w, "make images dir: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(imagesDir)

		// Suppress the shell-exit→restart path BEFORE the dump: criu freezes and
		// kills the shell tree, and without this the exit watch would os.Exit(1)
		// mid-request and truncate the archive below. On dump failure criu leaves
		// the tree running, so re-arm restart; on success we stay suppressed and
		// let the control plane reclaim the pod after the archive lands.
		a.checkpointing.Store(true)
		if err := a.engine.Dump(r.Context(), sh.pid, imagesDir); err != nil {
			a.checkpointing.Store(false)
			http.Error(w, "criu dump: "+err.Error(), http.StatusInternalServerError)
			return
		}
		sb := sh.out.snapshot()

		w.Header().Set("Content-Type", "application/x-tar")
		if err := writeArchive(w, imagesDir, sb); err != nil {
			// Body streaming has begun, so we can only log — the control plane
			// sees a truncated archive and fails the checkpoint.
			logger.Error("stream checkpoint archive", "err", err)
			return
		}
		logger.Info("checkpoint streamed; shell frozen, awaiting pod reclaim by control plane",
			"pid", sh.pid, "scrollback_bytes", len(sb))
	}
}

// handleRestore receives a checkpoint archive, CRIU-restores the shell tree, and
// adopts it. Only valid in restore mode before a shell exists.
func (a *agent) handleRestore(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.current() != nil {
			http.Error(w, "agent already has a shell", http.StatusConflict)
			return
		}
		imagesDir, err := os.MkdirTemp("", "criu-restore-")
		if err != nil {
			http.Error(w, "make images dir: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.RemoveAll(imagesDir)

		sb, err := readArchive(r.Body, imagesDir)
		if err != nil {
			http.Error(w, "read checkpoint archive: "+err.Error(), http.StatusBadRequest)
			return
		}
		sh, err := a.engine.Restore(r.Context(), imagesDir, sb)
		if err != nil {
			http.Error(w, "criu restore: "+err.Error(), http.StatusInternalServerError)
			return
		}
		a.adopt(sh)
		logger.Info("checkpoint restored", "pid", sh.pid, "scrollback_bytes", len(sb))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restored"}`))
	}
}

// writeArchive tars the scrollback and every file in imagesDir into w.
func writeArchive(w io.Writer, imagesDir string, scrollback []byte) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{
		Name: archiveScrollbackName,
		Mode: 0o600,
		Size: int64(len(scrollback)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(scrollback); err != nil {
		return err
	}

	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue // criu writes a flat image directory
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: archiveImagesPrefix + e.Name(),
			Mode: 0o600,
			Size: info.Size(),
		}); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(imagesDir, e.Name()))
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return tw.Close()
}

// readArchive unpacks an archive written by writeArchive.
func readArchive(r io.Reader, imagesDir string) ([]byte, error) {
	tr := tar.NewReader(r)
	var scrollback []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch {
		case hdr.Name == archiveScrollbackName:
			scrollback, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
		case len(hdr.Name) > len(archiveImagesPrefix) && hdr.Name[:len(archiveImagesPrefix)] == archiveImagesPrefix:
			name := hdr.Name[len(archiveImagesPrefix):]
			// Guard against path traversal in a hostile archive.
			if filepath.Base(name) != name {
				return nil, fmt.Errorf("unsafe archive entry %q", hdr.Name)
			}
			dst, err := os.Create(filepath.Join(imagesDir, name))
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(dst, tr); err != nil { //nolint:gosec // size bounded by the trusted control plane
				dst.Close()
				return nil, err
			}
			dst.Close()
		default:
			return nil, fmt.Errorf("unexpected archive entry %q", hdr.Name)
		}
	}
	return scrollback, nil
}

// execCriuEngine shells out to the criu binary.
//
// RUNTIME SEAM — being tuned against a real runtime. This is the one part of the
// in-pod CRIU path that cannot run without a CRIU-capable node, so it is
// exercised only during on-runtime verification, never in CI (tests inject a
// fake engine). The PTY handling on restore — recreating a master and restoring
// the shell job against that tty — remains the least-proven part. See
// docs/criu-verification.md.
type execCriuEngine struct {
	bin string
}

func newExecCriuEngine() *execCriuEngine {
	return &execCriuEngine{bin: env("CRIU_BIN", "criu")}
}

func (e *execCriuEngine) Dump(ctx context.Context, pid int, imagesDir string) error {
	// --shell-job: the shell is a session/PTY job whose sid/tty is inherited
	// from outside the dumped tree (the agent holds the PTY master).
	cmd := exec.CommandContext(ctx, e.bin, "dump",
		"--tree", strconv.Itoa(pid),
		"--images-dir", imagesDir,
		"--shell-job",
		"--file-locks",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func (e *execCriuEngine) Restore(ctx context.Context, imagesDir string, initial []byte) (*shellProc, error) {
	// Recreate a PTY pair; the restored shell's controlling terminal is wired to
	// the slave and the agent keeps the master to drain output, mirroring a fresh
	// start. --shell-job tells criu the tty is inherited.
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("open pty for restore: %w", err)
	}
	fail := func(err error) (*shellProc, error) {
		_ = ptmx.Close()
		_ = tty.Close()
		return nil, err
	}

	// --restore-detached: criu exits as soon as the tree is running, so criu's
	// own lifetime is NOT the shell's. --restore-sibling: the restored root task
	// becomes a child of criu's parent, i.e. of this agent, so the agent can
	// signal and reap it. --pidfile: criu records the restored root task's real
	// pid, which is what we then watch/signal instead of criu's. The incident
	// these three answer is docs/criu-verification.md 5차 (2026-07-23).
	pidfile := filepath.Join(imagesDir, "restored.pid")
	cmd := exec.CommandContext(ctx, e.bin, "restore",
		"--images-dir", imagesDir,
		"--shell-job",
		"--restore-detached",
		"--restore-sibling",
		"--pidfile", pidfile,
	)
	// criu's stdio is the PTY slave: that is the tty the shell job is restored
	// against (and where criu's own diagnostics land if the restore fails).
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if err := cmd.Run(); err != nil {
		return fail(fmt.Errorf("criu restore: %w", err))
	}
	_ = tty.Close() // the restored task holds its own copy now

	b, err := os.ReadFile(pidfile)
	if err != nil {
		return fail(fmt.Errorf("read criu pidfile %s: %w", pidfile, err))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return fail(fmt.Errorf("parse criu pidfile %s (%q): %w", pidfile, b, err))
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fail(fmt.Errorf("find restored task %d: %w", pid, err))
	}
	return newShellProc(ptmx, pid, initial,
		func() error { return waitRestored(proc) }, proc.Signal, proc.Kill), nil
}

// waitRestored blocks until the restored root task exits. --restore-sibling makes
// it a child of this agent, so os.Process.Wait normally reaps it directly. If the
// kernel/criu ever leaves it unreapable here (wait4 ECHILD), fall back to
// liveness polling rather than returning early — an early return would be read
// as "shell exited" and restart the container while the session is alive.
func waitRestored(p *os.Process) error {
	if _, err := p.Wait(); err == nil {
		return nil
	}
	for {
		if err := p.Signal(syscall.Signal(0)); err != nil {
			return nil // process is gone
		}
		time.Sleep(500 * time.Millisecond)
	}
}
