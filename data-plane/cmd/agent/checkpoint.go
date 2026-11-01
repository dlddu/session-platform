package main

// Shell snapshot and restore through the filesystem archive every workload
// type shares (state_archive.go). What survives is the shell's home directory
// and its scrollback (AC-D3/D4); the process tree does not. A restore starts a
// fresh shell in the restored home with the scrollback preloaded, so cursors
// issued before the snapshot stay valid.
//
// The protocol is the Claude archive's, generation-fenced by the control plane:
// /checkpoint closes write admission and pauses the shell's process tree so the
// home directory holds still while it is walked, then streams the archive. The
// tree stays paused and writes stay closed until the control plane either
// reclaims the pod or calls /checkpoint/abort, which resumes both.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	errShellAdmissionClosed = errors.New("shell is checkpointing; writes are closed")
	errShellNoLiveShell     = errors.New("no live shell")
	errShellCheckpointBusy  = errors.New("shell checkpoint archive is still streaming")
)

// shellCheckpoint is one in-flight snapshot attempt, guarded by agent.mu.
type shellCheckpoint struct {
	active bool   // admission closed, tree paused
	ready  bool   // archive fully streamed; only abort or reclamation follows
	id     string // the control plane's generation
	paused []int  // pids stopped for the walk, resumed on abort
}

// maxPauseRounds bounds the stop-then-rescan loop in pauseTree: a process that
// forks between the scan and its own SIGSTOP is caught on the next round.
const maxPauseRounds = 8

// pauseSettleTimeout bounds how long beginCheckpoint waits for SIGSTOP to land.
const pauseSettleTimeout = 2 * time.Second

func (a *agent) checkpointInFlight() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ckpt.active
}

// beginCheckpoint closes admission and pauses the live shell's tree.
func (a *agent) beginCheckpoint(id string) (*shellProc, error) {
	a.admission.Lock()
	defer a.admission.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.sh == nil || !a.sh.alive.Load():
		return nil, errShellNoLiveShell
	case a.ckpt.active:
		return nil, errShellAdmissionClosed
	}
	a.ckpt = shellCheckpoint{active: true, id: id, paused: pauseTree(a.sh.pid)}
	return a.sh, nil
}

// completeCheckpoint marks the archive as fully streamed.
func (a *agent) completeCheckpoint() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ckpt.active {
		a.ckpt.ready = true
	}
}

// abortCheckpoint resumes the tree and reopens writes after a local failure.
func (a *agent) abortCheckpoint() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reopenLocked()
}

// abortCompletedCheckpoint is the control plane's rollback once it has read
// the whole archive but could not commit the snapshot. It is idempotent: an
// attempt that already rolled back locally reports success.
func (a *agent) abortCompletedCheckpoint(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.sh == nil:
		return errShellNoLiveShell
	case id == "":
		return errClaudeCheckpointID
	case !a.ckpt.active:
		return nil
	case id != a.ckpt.id:
		return errClaudeCheckpointID
	case !a.ckpt.ready:
		return errShellCheckpointBusy
	}
	a.reopenLocked()
	return nil
}

func (a *agent) reopenLocked() {
	resumePids(a.ckpt.paused)
	a.ckpt = shellCheckpoint{}
}

// resumePaused resumes a paused tree without reopening writes, for shutdown.
func (a *agent) resumePaused() {
	a.mu.Lock()
	defer a.mu.Unlock()
	resumePids(a.ckpt.paused)
}

// handleCheckpoint streams the shell's archive (AC-B1/AC-D4).
func (a *agent) handleCheckpoint(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := claudeCheckpointID(r.Header.Get(claudeCheckpointIDHeader))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		archive, err := os.CreateTemp("", "shell-checkpoint-*.tar")
		if err != nil {
			http.Error(w, "create checkpoint archive: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer os.Remove(archive.Name())
		defer archive.Close()

		sh, err := a.beginCheckpoint(id)
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, errShellNoLiveShell) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		committed := false
		defer func() {
			if !committed {
				a.abortCheckpoint()
			}
		}()
		if err := writeStateArchive(archive, a.stateDir, "", sh.out.snapshot()); err != nil {
			http.Error(w, "build checkpoint archive: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "rewind checkpoint archive: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set(claudeCheckpointIDHeader, id)
		if _, err := io.Copy(w, archive); err != nil {
			logger.Error("stream shell checkpoint archive", "err", err)
			return
		}
		a.completeCheckpoint()
		committed = true
	}
}

// handleCheckpointAbort is the control plane's rollback of a streamed archive.
func (a *agent) handleCheckpointAbort() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.abortCompletedCheckpoint(r.Header.Get(claudeCheckpointIDHeader)); err != nil {
			status := http.StatusConflict
			if errors.Is(err, errShellNoLiveShell) {
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"aborted"}`))
	}
}

// handleRestore expands an archive into the state root and starts a fresh
// shell in the restored home. Only valid in restore mode before a shell exists.
func (a *agent) handleRestore(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		if a.sh != nil || a.ckpt.active {
			a.mu.Unlock()
			http.Error(w, "agent already has a shell", http.StatusConflict)
			return
		}
		// Holding the slot as active keeps a concurrent restore out until this
		// one has adopted its shell or failed.
		a.ckpt.active = true
		a.mu.Unlock()
		defer func() {
			a.mu.Lock()
			a.ckpt = shellCheckpoint{}
			a.mu.Unlock()
		}()

		r.Body = http.MaxBytesReader(w, r.Body, maxClaudeArchiveBytes)
		sb, err := restoreStateArchive(r.Body, a.stateDir, "", validateRestoredShellState)
		if err != nil {
			http.Error(w, "restore shell archive: "+err.Error(), http.StatusBadRequest)
			return
		}
		sh, err := startShell(a.shellPath, a.homeDir(), sb)
		if err != nil {
			http.Error(w, "start restored shell: "+err.Error(), http.StatusInternalServerError)
			return
		}
		a.adopt(sh)
		logger.Info("shell archive restored", "pid", sh.pid, "scrollback_bytes", len(sb))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"restored"}`))
	}
}

// validateRestoredShellState refuses an archive without a home directory, the
// one thing a shell snapshot always carries.
func validateRestoredShellState(root string) error {
	info, err := os.Lstat(filepath.Join(root, "home"))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("shell archive is missing home directory")
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("shell archive home is not a directory")
	}
	return nil
}

// pauseTree SIGSTOPs root and every descendant, rescanning until a round finds
// nothing new. A process that has already left the tree (daemonized onto the
// pid namespace's init) is out of reach and keeps running.
func pauseTree(root int) []int {
	stopped := map[int]bool{}
	var order []int
	for round := 0; round < maxPauseRounds; round++ {
		fresh := false
		for _, pid := range processTree(root) {
			if stopped[pid] {
				continue
			}
			stopped[pid] = true
			fresh = true
			if err := syscall.Kill(pid, syscall.SIGSTOP); err == nil {
				order = append(order, pid)
			}
		}
		if !fresh {
			break
		}
	}
	waitStopped(order)
	return order
}

// waitStopped gives SIGSTOP time to land: a process running on another CPU
// only stops when it next enters the kernel. A process that never reports
// stopped within the bound is left to the archive walk's own change checks.
func waitStopped(pids []int) {
	deadline := time.Now().Add(pauseSettleTimeout)
	for _, pid := range pids {
		for {
			state, ok := processState(pid)
			if !ok || state == "T" || state == "t" || state == "Z" || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// resumePids SIGCONTs pids in reverse stop order, children before the shell.
func resumePids(pids []int) {
	for i := len(pids) - 1; i >= 0; i-- {
		_ = syscall.Kill(pids[i], syscall.SIGCONT)
	}
}

// processTree returns root followed by its live descendants, read from /proc.
func processTree(root int) []int {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err == nil {
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			ppid, ok := parentPID(pid)
			if !ok {
				continue
			}
			children[ppid] = append(children[ppid], pid)
		}
	}
	tree := []int{root}
	for i := 0; i < len(tree); i++ {
		tree = append(tree, children[tree[i]]...)
	}
	return tree
}

// parentPID reads the ppid field of /proc/<pid>/stat.
func parentPID(pid int) (int, bool) {
	fields, ok := procStatFields(pid)
	if !ok || len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// processState reads the one-letter state field of /proc/<pid>/stat.
func processState(pid int) (string, bool) {
	fields, ok := procStatFields(pid)
	if !ok || len(fields) < 1 {
		return "", false
	}
	return fields[0], true
}

// procStatFields returns the /proc/<pid>/stat fields after the command name,
// which may hold spaces and parentheses and so is skipped up to its last ')'.
func procStatFields(pid int) ([]string, bool) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, false
	}
	end := bytes.LastIndexByte(line, ')')
	if end < 0 {
		return nil, false
	}
	return strings.Fields(string(line[end+1:])), true
}
