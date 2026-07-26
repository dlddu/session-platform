package checkpointstore_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/adapter/checkpointstore"
)

// Put writes the archive under the key and Get reads back the same bytes — the
// round trip the agent-driven checkpointer performs between snapshot and restore.
func TestDir_PutGetRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkpoints")
	store, err := checkpointstore.NewDir(root)
	if err != nil {
		t.Fatalf("new dir store: %v", err)
	}
	ctx := context.Background()

	ref, err := store.Put(ctx, "sess-abcd-r1/checkpoint.tar", strings.NewReader("ARCHIVE-BYTES"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !strings.HasPrefix(ref, "file://") {
		t.Errorf("ref = %q, want a file:// URI", ref)
	}

	rc, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "ARCHIVE-BYTES" {
		t.Errorf("archive = %q, want the bytes Put stored", got)
	}
	// The nested key created its directory under the root.
	if _, err := os.Stat(filepath.Join(root, "sess-abcd-r1", "checkpoint.tar")); err != nil {
		t.Errorf("archive not at the expected path: %v", err)
	}
}

// NewDir creates the root if it does not exist yet (the e2e volume is mounted
// empty) and rejects an empty path.
func TestDir_CreatesRootAndValidates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "checkpoints")
	if _, err := checkpointstore.NewDir(root); err != nil {
		t.Fatalf("new dir store on a missing path: %v", err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("root not created: %v", err)
	}
	if _, err := checkpointstore.NewDir("  "); err == nil {
		t.Error("empty path accepted; want error")
	}
}

// A traversing key must not escape the store root.
func TestDir_PutRejectsTraversal(t *testing.T) {
	store, err := checkpointstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"../escape.tar", "a/../../escape.tar"} {
		if _, err := store.Put(context.Background(), key, strings.NewReader("x")); err == nil {
			t.Errorf("Put(%q) succeeded; want a traversal rejection", key)
		}
	}
}

// Get refuses refs that are not file:// or that point outside the root, so a
// stored ref can never be turned into an arbitrary file read.
func TestDir_GetRejectsForeignRefs(t *testing.T) {
	root := t.TempDir()
	store, err := checkpointstore.NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		"s3://bucket/key",
		"/etc/passwd",
		"file://" + outside,
		"file://" + root + "/../escape",
	} {
		if _, err := store.Get(context.Background(), ref); err == nil {
			t.Errorf("Get(%q) succeeded; want rejection", ref)
		}
	}
}
