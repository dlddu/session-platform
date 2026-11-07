package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestAuthSlot(t *testing.T, file, homeRel string) *authSlot {
	t.Helper()
	return &authSlot{dir: t.TempDir(), file: file, homeRel: homeRel, poll: 10 * time.Millisecond, logger: testLogger()}
}

func readOptional(t *testing.T, path string) (string, bool) {
	t.Helper()
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(content), true
}

// AC-N4 1·3: the slot is the authority before a turn, HOME after it, and a
// logout in HOME empties the slot.
func TestAuthSlotPullPushAndLogout(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	home := t.TempDir()
	homeFile := slot.homePath(home)

	if err := os.MkdirAll(filepath.Dir(homeFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeFile, []byte(`{"t":"archived-old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(home); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOptional(t, homeFile); ok {
		t.Fatal("an empty slot must take a stale HOME login away")
	}
	if slot.signedIn() {
		t.Fatal("an empty slot is not signed in")
	}

	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"token-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(home); err != nil {
		t.Fatal(err)
	}
	if got, _ := readOptional(t, homeFile); got != `{"t":"token-1"}` {
		t.Fatalf("HOME login = %q, want the slot's", got)
	}
	if !slot.signedIn() {
		t.Fatal("a slot with a login is signed in")
	}

	if err := os.WriteFile(homeFile, []byte(`{"t":"token-2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := slot.push(home); err != nil {
		t.Fatal(err)
	}
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"token-2"}` {
		t.Fatalf("slot login = %q, want the refreshed one", got)
	}
	info, err := os.Stat(slot.slotPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("slot file mode = %v, %v; want 0600", info, err)
	}

	if err := os.Remove(homeFile); err != nil {
		t.Fatal(err)
	}
	if err := slot.pushIfPresent(home); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOptional(t, slot.slotPath()); !ok {
		t.Fatal("the in-turn poll must not treat a missing file as a logout")
	}
	if err := slot.push(home); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOptional(t, slot.slotPath()); ok {
		t.Fatal("a logout at the end of a turn empties the slot")
	}
}

// AC-N4 2: a login the CLI refreshes mid-turn reaches the slot before the
// turn ends.
func TestAuthSlotWatchCopiesRefreshMidTurn(t *testing.T) {
	slot := newTestAuthSlot(t, claudeCredentialsFile, claudeCredentialsHomeRel)
	home := t.TempDir()
	stop := slot.watch(home)
	defer stop()
	if err := writeLoginFile(slot.homePath(home), []byte(`{"t":"refreshed"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := readOptional(t, slot.slotPath()); got == `{"t":"refreshed"}` {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the refreshed login did not reach the slot during the turn")
}

func TestAuthSlotRefusesNonRegularLogin(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	if err := os.Mkdir(slot.slotPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(t.TempDir()); err == nil {
		t.Fatal("a directory where the login file belongs must be refused")
	}
}

func TestNewAuthSlotFromEnv(t *testing.T) {
	t.Setenv(authSlotDirEnv, "")
	if _, err := newAuthSlotFromEnv(codexAuthFile, codexAuthHomeRel, testLogger()); err == nil {
		t.Fatal("a missing slot must be refused")
	}
	t.Setenv(authSlotDirEnv, "relative")
	if _, err := newAuthSlotFromEnv(codexAuthFile, codexAuthHomeRel, testLogger()); err == nil {
		t.Fatal("a relative slot path must be refused")
	}
	t.Setenv(authSlotDirEnv, filepath.Join(t.TempDir(), "absent"))
	if _, err := newAuthSlotFromEnv(codexAuthFile, codexAuthHomeRel, testLogger()); err == nil {
		t.Fatal("a slot that is not mounted must be refused")
	}
	dir := t.TempDir()
	t.Setenv(authSlotDirEnv, dir)
	slot, err := newAuthSlotFromEnv(codexAuthFile, codexAuthHomeRel, testLogger())
	if err != nil || slot.slotPath() != filepath.Join(dir, codexAuthFile) {
		t.Fatalf("slot = %v, %v", slot, err)
	}
}

func newSlotClaudeTestServer(t *testing.T, runner commandRunner, slot *authSlot, login claudeLogin) (*claudeWorkload, *httptest.Server) {
	t.Helper()
	c, err := newClaudeWorkload(claudeConfig{
		StateDir: filepath.Join(t.TempDir(), "state"),
		Model:    platformDefaultModel,
		Binary:   defaultClaudeBinary,
		Runner:   runner,
		Logger:   testLogger(),
		Tools:    toolSurface{Plugins: []string{claudeSessionPlatformPlugin}},
		AuthSlot: slot,
		Login:    login,
	})
	if err != nil {
		t.Fatalf("new claude workload: %v", err)
	}
	c.now = func() time.Time { return testTurnTime }
	srv := httptest.NewServer(routes(testLogger(), &agent{claude: c}))
	t.Cleanup(func() {
		srv.Close()
		c.Close()
	})
	return c, srv
}

func archiveNames(t *testing.T, archive []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}

// AC-N4 end to end on the worker: the CLI sees the slot's login, its
// refresh goes back to the slot, the archive leaves the login out, and
// GET /auth-slot reports the slot.
func TestClaudeWorkloadSyncsAuthSlotAroundTurns(t *testing.T) {
	slot := newTestAuthSlot(t, claudeCredentialsFile, claudeCredentialsHomeRel)
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"slot-token"}`)); err != nil {
		t.Fatal(err)
	}
	runner := newFakeClaudeRunner()
	seen := make(chan string, 1)
	runner.output = func(_ int, opts runnerOptions) {
		home := envValue(opts.Env, "HOME")
		content, _ := os.ReadFile(filepath.Join(home, claudeCredentialsHomeRel))
		seen <- string(content)
		_ = os.WriteFile(filepath.Join(home, claudeCredentialsHomeRel), []byte(`{"t":"refreshed-token"}`), 0o600)
	}
	c, srv := newSlotClaudeTestServer(t, runner, slot, &fakeClaudeLogin{loggedIn: true})

	writeViaHTTP(t, srv, "hello", http.StatusOK)
	receiveClaudeRun(t, runner.started)
	if got := <-seen; got != `{"t":"slot-token"}` {
		t.Fatalf("the CLI saw %q, want the slot's login", got)
	}
	waitClaudeIdle(t, c)
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"refreshed-token"}` {
		t.Fatalf("slot = %q, want the refreshed login", got)
	}

	resp, err := http.Get(srv.URL + authSlotPath)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !body["signedIn"] {
		t.Fatalf("GET /auth-slot = %d %v", resp.StatusCode, body)
	}

	checkpoint, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(checkpoint.Body)
	checkpoint.Body.Close()
	if checkpoint.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint = %d: %s", checkpoint.StatusCode, archive)
	}
	names := archiveNames(t, archive)
	for _, name := range names {
		if strings.HasSuffix(name, claudeCredentialsHomeRel) {
			t.Fatalf("the archive carries the slot login: %q", names)
		}
	}
	if !slicesContain(names, claudeArchiveStateRoot+"/home/"+claudeSettingsDir) {
		t.Fatalf("the rest of HOME must still be archived: %q", names)
	}
}

func TestAuthSlotRouteWithoutSlotIs404(t *testing.T) {
	_, srv := newClaudeTestServer(t, newFakeClaudeRunner(), platformDefaultModel, false, nil)
	resp, err := http.Get(srv.URL + authSlotPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /auth-slot without a slot = %d, want 404", resp.StatusCode)
	}
}

// A pull that fails leaves HOME without the login; the turn end must not read
// that absence as a logout and wipe the slot (AC-N4).
func TestAuthSlotFailedPullIsNotALogout(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	home := t.TempDir()
	// An unreadable slot entry: pull refuses it, so HOME never gets a login.
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), slot.slotPath()); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(home); err == nil {
		t.Fatal("pull of a non-regular slot entry must fail")
	}
	if err := slot.push(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(slot.slotPath()); err != nil {
		t.Fatalf("turn end after a failed pull removed the slot's entry: %v", err)
	}
}

// A login the turn end could not save stays in HOME: the next pull saves it
// first instead of overwriting it with the slot's older copy.
func TestAuthSlotUnsavedLoginSurvivesNextPull(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	home := t.TempDir()
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(home); err != nil {
		t.Fatal(err)
	}
	if err := writeLoginFile(slot.homePath(home), []byte(`{"t":"rotated"}`)); err != nil {
		t.Fatal(err)
	}
	// The slot entry turns into something the copy cannot replace.
	if err := os.Remove(slot.slotPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(slot.slotPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := slot.push(home); err == nil {
		t.Fatal("push onto a directory must fail")
	}
	if err := slot.pull(home); err == nil {
		t.Fatal("pull must report the login it still could not save")
	}
	if got, _ := readOptional(t, slot.homePath(home)); got != `{"t":"rotated"}` {
		t.Fatalf("HOME = %q, want the unsaved rotated login kept", got)
	}
	if err := os.Remove(slot.slotPath()); err != nil {
		t.Fatal(err)
	}
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(home); err != nil {
		t.Fatal(err)
	}
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"rotated"}` {
		t.Fatalf("slot = %q, want the rotated login saved before the pull", got)
	}
	if got, _ := readOptional(t, slot.homePath(home)); got != `{"t":"rotated"}` {
		t.Fatalf("HOME = %q, want the rotated login", got)
	}
}

// A HOME file that is not JSON — caught mid-rewrite — never replaces the
// slot's login, in the poll or at the turn end.
func TestAuthSlotCopiesOnlyJSON(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	home := t.TempDir()
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"good"}`)); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(home); err != nil {
		t.Fatal(err)
	}
	if err := writeLoginFile(slot.homePath(home), []byte(`{"t":"go`)); err != nil {
		t.Fatal(err)
	}
	if err := slot.pushIfPresent(home); !errors.Is(err, errLoginNotJSON) {
		t.Fatalf("poll copy = %v, want errLoginNotJSON", err)
	}
	if err := slot.push(home); !errors.Is(err, errLoginNotJSON) {
		t.Fatalf("turn-end copy = %v, want errLoginNotJSON", err)
	}
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"good"}` {
		t.Fatalf("slot = %q, want the good login kept", got)
	}
	if slot.unpushed {
		t.Fatal("a non-JSON file is not a login to retry")
	}
}
