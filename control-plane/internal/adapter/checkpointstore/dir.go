package checkpointstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Dir stores checkpoint archives in a local directory instead of S3. It exists
// because the archive only has to outlive the *session* pod (which is reclaimed
// at snapshot), not the control plane — so a directory the control plane can
// read and write is a complete store.
//
// This is what makes CRIU exercisable without any cloud: the kind e2e SUT mounts
// a volume and points CHECKPOINT_DIR at it. In a multi-replica deployment the
// directory must be shared by every replica (a restore may be served by a
// different replica than the snapshot), which is why the e2e overlay backs it
// with a shared volume. S3 (store.go) remains the durable option for production.
type Dir struct {
	root string
}

// NewDir returns a store rooted at path, creating it if needed.
func NewDir(path string) (*Dir, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("checkpoint dir store needs a path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve checkpoint dir %q: %w", path, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint dir %s: %w", abs, err)
	}
	return &Dir{root: abs}, nil
}

// Root reports the directory archives are written to (for logging).
func (d *Dir) Root() string { return d.root }

// Put writes the archive under key and returns a file:// ref.
func (d *Dir) Put(_ context.Context, key string, r io.Reader) (string, error) {
	dst, err := d.resolve(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("create checkpoint dir for %s: %w", key, err)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create checkpoint file %s: %w", dst, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", fmt.Errorf("write checkpoint file %s: %w", dst, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync checkpoint file %s: %w", dst, err)
	}
	return "file://" + dst, nil
}

// Get opens the archive at ref (a file:// URI produced by Put).
func (d *Dir) Get(_ context.Context, ref string) (io.ReadCloser, error) {
	const scheme = "file://"
	if !strings.HasPrefix(ref, scheme) {
		return nil, fmt.Errorf("not a file ref: %q", ref)
	}
	path := filepath.Clean(ref[len(scheme):])
	// Only serve what lives under this store's root, so a ref from elsewhere
	// can never read arbitrary files.
	if path != d.root && !strings.HasPrefix(path, d.root+string(os.PathSeparator)) {
		return nil, fmt.Errorf("checkpoint ref %q is outside the store root %s", ref, d.root)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", ref, err)
	}
	return f, nil
}

// resolve maps a key to a path inside the root. A traversing key is rejected
// rather than clamped into the root: silently rewriting it would let two
// different keys collide on one file.
func (d *Dir) resolve(key string) (string, error) {
	clean := filepath.Clean(strings.TrimPrefix(key, "/"))
	unsafe := clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(os.PathSeparator))
	if unsafe {
		return "", fmt.Errorf("unsafe checkpoint key %q", key)
	}
	dst := filepath.Join(d.root, clean)
	if !strings.HasPrefix(dst, d.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe checkpoint key %q", key)
	}
	return dst, nil
}
