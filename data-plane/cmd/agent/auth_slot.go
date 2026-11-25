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

// The auth slot (docs/prd/agent-auth-pool.md, AC-N4): a pool PVC the control
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
	// interleave their renames. It also guards the per-home state below.
	mu sync.Mutex
	// homes is the turn state of each HOME this slot serves, keyed by it.
	//
	// A manager that runs two lanes (AC-N11) has two HOMEs on one login: the
	// lanes take turns at the same time, and each has its own copy of the file.
	// One `placed` flag would mean one lane's pull decided whether the other
	// lane's missing file counted as a logout, so the state is per HOME. A
	// workload with a single HOME has exactly one entry and behaves as it did.
	homes map[string]*authSlotHome
}

// authSlotHome is what the slot knows about one HOME it keeps equal.
type authSlotHome struct {
	// placed is true when this turn's pull succeeded and left the slot's file
	// in HOME. Only then does a file missing at turn end mean the CLI logged
	// out; a failed pull also leaves HOME without the file, and reading that
	// as a logout would wipe the slot's valid login.
	placed bool
	// unpushed is true when the last turn-end copy to the slot failed: HOME
	// holds a login newer than the slot's, so the next pull must not
	// overwrite it with the older copy.
	unpushed bool
	// baseline is the slot content this HOME started from — what its last
	// successful pull read, or what its last successful push left. A copy back
	// is only taken when the slot still holds it, so one lane never writes over
	// a login the other lane refreshed while it was running. The lane that is
	// behind re-adopts the newer file on its next pull instead.
	baseline []byte
	// hasBaseline distinguishes "started from an empty slot" from "has not
	// looked yet": the first may write, the second must read first.
	hasBaseline bool
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
// archive must not outlive the slot it came from. The one exception is a
// login the last turn could not save: it is saved first, and if that still
// fails HOME keeps it rather than taking the slot's older copy.
func (s *authSlot) pull(home string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.homeLocked(home)
	state.placed = false
	target := s.homePath(home)
	if state.unpushed {
		if err := s.copyBackLocked(home, false); err != nil {
			_, have, _ := readLoginFile(target)
			state.placed = have
			return fmt.Errorf("save the unsaved login before the turn: %w", err)
		}
		state.unpushed = false
	}
	content, ok, err := readLoginFile(s.slotPath())
	if err != nil {
		return err
	}
	// Whatever the slot holds now is this turn's starting point, including its
	// absence: a copy back is measured against it.
	state.baseline, state.hasBaseline = content, true
	if !ok {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove stale login: %w", err)
		}
		return nil
	}
	current, have, err := readLoginFile(target)
	if !(err == nil && have && bytes.Equal(current, content)) {
		if err := writeLoginFile(target, content); err != nil {
			return err
		}
	}
	state.placed = true
	return nil
}

// homeLocked is this HOME's turn state, created on first use.
func (s *authSlot) homeLocked(home string) *authSlotHome {
	if s.homes == nil {
		s.homes = map[string]*authSlotHome{}
	}
	state, ok := s.homes[home]
	if !ok {
		state = &authSlotHome{}
		s.homes[home] = state
	}
	return state
}

// push runs after a turn: whatever the CLI left in HOME is the login now. A
// file missing after this turn's pull put it there means the CLI logged out,
// and the slot follows. A copy that fails is retried by the next pull.
func (s *authSlot) push(home string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.homeLocked(home)
	err := s.copyBackLocked(home, state.placed)
	if err != nil && !errors.Is(err, errLoginNotJSON) && !errors.Is(err, errLoginOverwritten) {
		state.unpushed = true
	}
	return err
}

// pushIfPresent is the in-turn poll's copy. A file that is momentarily gone
// mid-rewrite is not a logout, so it never removes the slot's file.
func (s *authSlot) pushIfPresent(home string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyBackLocked(home, false)
}

// errLoginNotJSON is a HOME file caught mid-rewrite, or not a login at all:
// both slot files are JSON, and copying a truncated one would leave the slot
// with no usable login if the pod died before the next copy.
var errLoginNotJSON = errors.New("the login file in HOME is not valid JSON; the slot keeps its copy")

// errLoginOverwritten is the slot having moved on since this HOME pulled it:
// another lane of the same manager refreshed the login while this one was
// running (AC-N11). It is not a failure to retry — the slot holds the newer
// file, and this HOME adopts it on its next pull — so it never sets unpushed,
// which would push the older copy back over it.
var errLoginOverwritten = errors.New("another lane refreshed this login while the turn ran; the slot keeps the newer copy")

func (s *authSlot) copyBackLocked(home string, removeWhenAbsent bool) error {
	content, ok, err := readLoginFile(s.homePath(home))
	if err != nil {
		return err
	}
	slot := s.slotPath()
	current, have, readErr := readLoginFile(slot)
	// The compare-and-swap: this HOME may only replace the login it started
	// from. With a single HOME the baseline is always what the slot holds, so
	// this is transparent; with two, it is what stops the slower lane from
	// undoing the faster lane's refresh.
	if state := s.homeLocked(home); state.hasBaseline && readErr == nil {
		started := state.baseline
		if (have && !bytes.Equal(current, started)) || (!have && len(started) > 0) {
			return errLoginOverwritten
		}
	}
	if !ok {
		if !removeWhenAbsent {
			return nil
		}
		if err := os.Remove(slot); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove slot login: %w", err)
		}
		s.rebaseLocked(home, nil)
		return syncDir(s.dir)
	}
	if !json.Valid(content) {
		return errLoginNotJSON
	}
	if readErr == nil && have && bytes.Equal(current, content) {
		s.rebaseLocked(home, content)
		return nil
	}
	if err := writeLoginFile(slot, content); err != nil {
		return err
	}
	s.rebaseLocked(home, content)
	return nil
}

// rebaseLocked records what this HOME left in the slot, so its next copy back
// compares against what it actually wrote rather than what it first read.
func (s *authSlot) rebaseLocked(home string, content []byte) {
	state := s.homeLocked(home)
	state.baseline, state.hasBaseline = content, true
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
				// A half-written file is the CLI mid-rewrite; the next tick or
				// the turn-end copy picks up the finished one.
				if err := s.pushIfPresent(home); errors.Is(err, errLoginOverwritten) {
					// The other lane of this manager refreshed first. Saying so
					// once per poll would be a log line a second; the pull that
					// adopts the newer file is the event worth having.
					s.log().Debug("the auth slot moved on while this turn ran", "home", home)
				} else if err != nil && !errors.Is(err, errLoginNotJSON) {
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
// lease, so the pool can hand out signed-in slots first (AC-N2, AC-N4).
func serveAuthSlot(w http.ResponseWriter, slot *authSlot) {
	if slot == nil {
		http.Error(w, "this workload has no auth slot", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"signedIn": slot.signedIn()})
}
