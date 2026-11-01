package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A restored workspace keeps the timestamps it was frozen with — for files and
// directories, in the state tree and in the shared volume. The past times are
// whole seconds because the default tar writer records ModTime at that
// precision; anything finer is out of scope.
func TestStateArchiveRestorePreservesModTimes(t *testing.T) {
	src := t.TempDir()
	state := filepath.Join(src, "state")
	shared := filepath.Join(src, "shared")
	writeTestTree(t, state, map[string]string{"home/project/notes.txt": "state-file"})
	writeTestTree(t, shared, map[string]string{"downloads/report.json": "{}"})

	fileTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	dirTime := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	// Files first, then directories deepest-first: stamping a child after its
	// parent would not move the parent, but creating one would.
	for _, f := range []string{
		filepath.Join(state, "home", "project", "notes.txt"),
		filepath.Join(shared, "downloads", "report.json"),
	} {
		if err := os.Chtimes(f, fileTime, fileTime); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{
		filepath.Join(state, "home", "project"),
		filepath.Join(state, "home"),
		filepath.Join(shared, "downloads"),
	} {
		if err := os.Chtimes(d, dirTime, dirTime); err != nil {
			t.Fatal(err)
		}
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
	if _, err := restoreStateArchive(bytes.NewReader(archive.Bytes()), restoredState, restoredShared, validateRestoredShellState); err != nil {
		t.Fatalf("restore archive: %v", err)
	}

	for path, want := range map[string]time.Time{
		filepath.Join(restoredState, "home", "project", "notes.txt"): fileTime,
		filepath.Join(restoredState, "home", "project"):              dirTime,
		filepath.Join(restoredState, "home"):                         dirTime,
		filepath.Join(restoredShared, "downloads", "report.json"):    fileTime,
		filepath.Join(restoredShared, "downloads"):                   dirTime,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.ModTime().Equal(want) {
			t.Errorf("%s mtime = %s, want %s", path, info.ModTime().UTC(), want)
		}
	}
}

// Symlinks are not stamped: os.Chtimes follows them, so stamping a link would
// rewrite its target's time instead. The target keeps its own archived time.
func TestStateArchiveRestoreDoesNotStampThroughSymlinks(t *testing.T) {
	src := t.TempDir()
	state := filepath.Join(src, "state")
	writeTestTree(t, state, map[string]string{"home/target.txt": "x"})
	target := filepath.Join(state, "home", "target.txt")
	targetTime := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(target, targetTime, targetTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(state, "home", "link")); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := writeStateArchive(&archive, state, "", nil); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "state")
	if _, err := restoreStateArchive(bytes.NewReader(archive.Bytes()), restored, "", validateRestoredShellState); err != nil {
		t.Fatalf("restore archive: %v", err)
	}
	info, err := os.Stat(filepath.Join(restored, "home", "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(targetTime) {
		t.Fatalf("symlink target mtime = %s, want %s", info.ModTime().UTC(), targetTime)
	}
}
