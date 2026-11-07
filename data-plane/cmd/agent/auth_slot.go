package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The auth slot (docs/prd/agent-auth-pool.md, AC-L4): a pool PVC the control
// plane leases to this session alone and mounts at authSlotDirEnv. It holds
// one login file. The CLI reads and writes that login under HOME, so the agent
// keeps the two places equal by copying — never by a symlink, because a CLI
// that replaces its file with a temporary file and a rename would break the
// link and the refreshed token would land only in HOME.
const (
	// Keep in sync with control-plane/internal/adapter/k8s (the auth slot
	// mount and its env).
	authSlotDirEnv = "AGENT_AUTH_SLOT_DIR"
	authSlotPath   = "/auth-slot"

	defaultAuthSlotPollInterval = time.Second
	// A login file is a few KiB of JSON; anything larger is not one.
	maxAuthSlotFileBytes = 1 << 20
)

// authSlot is one workload's login file and the two places it lives.
type authSlot struct {
	// dir is the mounted slot PVC.
	dir string
	// file is the login file's name inside the slot.
	file string
	// homeRel is where the CLI keeps the same file, relative to HOME.
	homeRel string
	// poll is how often a running turn's refreshed login is copied back.
	poll   time.Duration
	logger *slog.Logger

	// mu serialises copies: the in-turn poll and the end-of-turn copy must not
	// interleave their renames.
	mu sync.Mutex
}

// newAuthSlotFromEnv returns the slot this pod mounts, or an error when the
// workload needs one and the control plane did not provide it.
func newAuthSlotFromEnv(file, homeRel string, logger *slog.Logger) (*authSlot, error) {
	dir := strings.TrimSpace(os.Getenv(authSlotDirEnv))
	if dir == "" {
		return nil, fmt.Errorf("%s must name the auth slot mount", authSlotDirEnv)
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("%s must be an absolute path", authSlotDirEnv)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("auth slot %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("auth slot %s is not a directory", dir)
	}
	return &authSlot{dir: filepath.Clean(dir), file: file, homeRel: homeRel, poll: defaultAuthSlotPollInterval, logger: logger}, nil
}

func (s *authSlot) slotPath() string { return filepath.Join(s.dir, s.file) }

func (s *authSlot) homePath(home string) string {
	return filepath.Join(home, filepath.FromSlash(s.homeRel))
}

// archiveExclusion is the login file's path inside the state tree. The
// archive leaves it out: the login is the slot's, and an archived copy could
// later bring an older refresh token back over the slot's newer one.
func (s *authSlot) archiveExclusion() string {
	return "home/" + s.homeRel
}

func (s *authSlot) log() *slog.Logger {
	if s.logger == nil {
		return slog.Default()
	}
	return s.logger
}

// signedIn reports whether the slot holds a login, for GET /auth-slot.
func (s *authSlot) signedIn() bool {
	content, ok, err := readLoginFile(s.slotPath())
	return err == nil && ok && len(bytes.TrimSpace(content)) > 0
}

// pull runs before a turn: the slot is the authority, so HOME takes its file,
// and takes its absence too — a login another session left behind in an
// archive must not outlive the slot it came from.
func (s *authSlot) pull(home string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok, err := readLoginFile(s.slotPath())
	if err != nil {
		return err
	}
	target := s.homePath(home)
	if !ok {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove stale login: %w", err)
		}
		return nil
	}
	current, have, err := readLoginFile(target)
	if err == nil && have && bytes.Equal(current, content) {
		return nil
	}
	return writeLoginFile(target, content)
}

// push runs after a turn: whatever the CLI left in HOME is the login now. A
// missing file means the CLI logged out, and the slot follows.
func (s *authSlot) push(home string) error {
	return s.copyBack(home, true)
}

// pushIfPresent is the in-turn poll's copy. A file that is momentarily gone
// mid-rewrite is not a logout, so it never removes the slot's file.
func (s *authSlot) pushIfPresent(home string) error {
	return s.copyBack(home, false)
}

func (s *authSlot) copyBack(home string, removeWhenAbsent bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok, err := readLoginFile(s.homePath(home))
	if err != nil {
		return err
	}
	slot := s.slotPath()
	if !ok {
		if !removeWhenAbsent {
			return nil
		}
		if err := os.Remove(slot); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove slot login: %w", err)
		}
		return syncDir(s.dir)
	}
	current, have, err := readLoginFile(slot)
	if err == nil && have && bytes.Equal(current, content) {
		return nil
	}
	return writeLoginFile(slot, content)
}

// watch copies a refreshed login back while a turn runs, so a pod that dies
// mid-turn still leaves the slot with the token the provider now expects.
// The returned stop waits for the poller to finish.
func (s *authSlot) watch(home string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	interval := s.poll
	if interval <= 0 {
		interval = defaultAuthSlotPollInterval
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.pushIfPresent(home); err != nil {
					s.log().Warn("copy the refreshed login to the auth slot", "err", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func readLoginFile(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("login file %s is not a regular file", path)
	}
	if info.Size() > maxAuthSlotFileBytes {
		return nil, false, fmt.Errorf("login file %s exceeds %d bytes", path, maxAuthSlotFileBytes)
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

// writeLoginFile replaces path atomically with a 0600 file and makes the
// rename durable before returning: the slot outlives this pod.
func writeLoginFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// serveAuthSlot answers the control plane's question before it returns the
// lease, so the pool can hand out signed-in slots first (AC-L2, AC-L4).
func serveAuthSlot(w http.ResponseWriter, slot *authSlot) {
	if slot == nil {
		http.Error(w, "this workload has no auth slot", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"signedIn": slot.signedIn()})
}
