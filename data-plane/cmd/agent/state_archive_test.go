package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// AC-F5: the shared volume rides the archive and is expanded into the next
// round's (empty, separately mounted) volume.
func TestStateArchiveCarriesSharedVolume(t *testing.T) {
	src := t.TempDir()
	state := filepath.Join(src, "state")
	shared := filepath.Join(src, "shared")
	writeTestTree(t, state, map[string]string{"home/notes.txt": "state-file"})
	writeTestTree(t, shared, map[string]string{"downloads/report.json": "{\"ok\":true}"})
	if err := os.Symlink("downloads/report.json", filepath.Join(shared, "latest")); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := writeStateArchive(&archive, state, shared, []byte("history\n")); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	dst := t.TempDir()
	restoredState := filepath.Join(dst, "state")
	restoredShared := filepath.Join(dst, "shared")
	if err := os.Mkdir(restoredShared, 0o777); err != nil {
		t.Fatal(err)
	}
	sb, err := restoreStateArchive(bytes.NewReader(archive.Bytes()), restoredState, restoredShared, validateRestoredShellState)
	if err != nil {
		t.Fatalf("restore archive: %v", err)
	}
	if string(sb) != "history\n" {
		t.Fatalf("scrollback = %q", sb)
	}
	if got := readTestFile(t, filepath.Join(restoredState, "home", "notes.txt")); got != "state-file" {
		t.Fatalf("state file = %q", got)
	}
	if got := readTestFile(t, filepath.Join(restoredShared, "downloads", "report.json")); got != "{\"ok\":true}" {
		t.Fatalf("shared file = %q", got)
	}
	if got := readTestFile(t, filepath.Join(restoredShared, "latest")); got != "{\"ok\":true}" {
		t.Fatalf("shared symlink resolves to %q", got)
	}
	info, err := os.Stat(filepath.Join(restoredShared, "downloads", "report.json"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("shared file mode = %v, %v; want 0640", info.Mode().Perm(), err)
	}
}

// Dropping archived shared files on a pod without the volume would lose them
// silently, so the restore fails before touching the state target.
func TestStateArchiveRefusesSharedFilesWithoutVolume(t *testing.T) {
	src := t.TempDir()
	state := filepath.Join(src, "state")
	shared := filepath.Join(src, "shared")
	writeTestTree(t, state, map[string]string{"home/notes.txt": "x"})
	writeTestTree(t, shared, map[string]string{"file": "y"})
	var archive bytes.Buffer
	if err := writeStateArchive(&archive, state, shared, nil); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "state")
	_, err := restoreStateArchive(bytes.NewReader(archive.Bytes()), target, "", validateRestoredShellState)
	if err == nil || !strings.Contains(err.Error(), "shared volume") {
		t.Fatalf("restore err = %v, want a shared volume error", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("state target mutated: %v", err)
	}
}

// An archive taken without a shared volume restores into a pod that has one,
// leaving the volume as it was.
func TestStateArchiveWithoutSharedLeavesVolumeAlone(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	writeTestTree(t, state, map[string]string{"home/notes.txt": "x"})
	var archive bytes.Buffer
	if err := writeStateArchive(&archive, state, "", nil); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	shared := filepath.Join(dst, "shared")
	writeTestTree(t, shared, map[string]string{"preexisting": "keep"})
	if _, err := restoreStateArchive(bytes.NewReader(archive.Bytes()), filepath.Join(dst, "state"), shared, validateRestoredShellState); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readTestFile(t, filepath.Join(shared, "preexisting")); got != "keep" {
		t.Fatalf("shared volume changed: %q", got)
	}
}

// newSharedClaudeServer serves a Claude-family workload that mounts a shared
// volume, as gated (minimax-code) pods do.
func newSharedClaudeServer(t *testing.T, stateDir, sharedDir string, restoreMode bool) *httptest.Server {
	t.Helper()
	c, err := newClaudeWorkload(claudeConfig{
		StateDir:    stateDir,
		Binary:      defaultClaudeBinary,
		RestoreMode: restoreMode,
		Runner:      newFakeClaudeRunner(),
		Logger:      testLogger(),
		Tools:       toolSurface{Plugin: true, SharedDir: sharedDir},
	})
	if err != nil {
		t.Fatalf("new claude workload: %v", err)
	}
	srv := httptest.NewServer(routes(testLogger(), &agent{claude: c}))
	t.Cleanup(func() {
		srv.Close()
		c.Close()
	})
	return srv
}

// AC-F5 end to end through the agent: files on the shared volume survive a
// checkpoint and are present on the next round's volume after restore.
func TestClaudeFamilyArchiveRoundTripsSharedVolume(t *testing.T) {
	shared1 := t.TempDir()
	writeTestTree(t, shared1, map[string]string{"spill/response.bin": "approved-payload"})
	srv1 := newSharedClaudeServer(t, filepath.Join(t.TempDir(), "state"), shared1, false)
	resp, err := http.Post(srv1.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status=%d err=%v body=%q", resp.StatusCode, err, archive)
	}

	shared2 := t.TempDir()
	srv2 := newSharedClaudeServer(t, filepath.Join(t.TempDir(), "state"), shared2, true)
	restoreResp, err := http.Post(srv2.URL+"/restore", "application/x-tar", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(restoreResp.Body)
	restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		t.Fatalf("restore status=%d body=%q", restoreResp.StatusCode, body)
	}
	if got := readTestFile(t, filepath.Join(shared2, "spill", "response.bin")); got != "approved-payload" {
		t.Fatalf("restored shared file = %q", got)
	}
}
