package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	claudeArchiveScrollback = "scrollback"
	claudeArchiveStateRoot  = "state"
)

// writeClaudeArchive captures the complete filesystem-backed Claude state plus
// the already-redacted output buffer. The worker is drained before this runs,
// so the tree and the resume flag cannot change while it is walked.
func writeClaudeArchive(w io.Writer, stateDir string, scrollback []byte) error {
	if int64(len(scrollback)) > maxClaudeScrollbackBytes {
		return fmt.Errorf("claude scrollback exceeds %d bytes", maxClaudeScrollbackBytes)
	}
	w = &claudeArchiveLimitWriter{writer: w, remaining: maxClaudeArchiveBytes}
	rootInfo, err := os.Lstat(stateDir)
	if err != nil {
		return fmt.Errorf("inspect claude state root: %w", err)
	}
	if !rootInfo.IsDir() {
		return errors.New("claude state root is not a directory")
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return fmt.Errorf("open claude state root: %w", err)
	}
	defer root.Close()
	openedRootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(rootInfo, openedRootInfo) {
		return errors.New("claude state root changed while archiving")
	}

	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{
		Name: claudeArchiveScrollback,
		Mode: 0o600,
		Size: int64(len(scrollback)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(scrollback); err != nil {
		return err
	}

	entryCount := 1 // scrollback
	err = filepath.WalkDir(stateDir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(stateDir, name)
		if err != nil {
			return err
		}
		archiveName := claudeArchiveStateRoot
		if rel != "." {
			archiveName += "/" + filepath.ToSlash(rel)
		}
		entryCount++
		if entryCount > maxClaudeArchiveEntries {
			return fmt.Errorf("claude archive exceeds %d entries", maxClaudeArchiveEntries)
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		var link string
		switch {
		case info.Mode().IsRegular(), info.IsDir():
		case info.Mode()&os.ModeSymlink != 0:
			link, err = os.Readlink(name)
			if err != nil {
				return err
			}
			if err := validateClaudeSymlink(filepath.ToSlash(rel), filepath.ToSlash(link)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported claude state entry %s (%s)", name, info.Mode())
		}
		if len(archiveName) > maxClaudeArchivePathBytes || len(link) > maxClaudeArchivePathBytes {
			return errors.New("claude archive entry path is too long")
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = archiveName
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		openedInfo, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return statErr
		}
		if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
			f.Close()
			return fmt.Errorf("claude state entry changed while archiving: %s", name)
		}
		_, copyErr := io.CopyN(tw, f, info.Size())
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		_ = tw.Close()
		return err
	}
	return tw.Close()
}

type restoredDirMode struct {
	name string
	mode fs.FileMode
}

// restoreClaudeArchive validates and extracts into a fresh sibling directory,
// then atomically swaps that directory into place. A malformed archive never
// partially mutates the live target.
func restoreClaudeArchive(r io.Reader, stateDir string) ([]byte, error) {
	stateDir = filepath.Clean(stateDir)
	if !filepath.IsAbs(stateDir) || stateDir == string(filepath.Separator) {
		return nil, fmt.Errorf("unsafe claude state target %q", stateDir)
	}
	parent := filepath.Dir(stateDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create state parent: %w", err)
	}
	tempRoot, err := os.MkdirTemp(parent, "."+filepath.Base(stateDir)+"-restore-")
	if err != nil {
		return nil, fmt.Errorf("create restore staging dir: %w", err)
	}
	defer os.RemoveAll(tempRoot)
	extractedState := filepath.Join(tempRoot, claudeArchiveStateRoot)

	tr := tar.NewReader(r)
	var scrollback []byte
	var sawScrollback, sawState bool
	var dirModes []restoredDirMode
	seenEntries := make(map[string]struct{})
	entryCount := 0
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entryCount++
		if entryCount > maxClaudeArchiveEntries {
			return nil, fmt.Errorf("claude archive exceeds %d entries", maxClaudeArchiveEntries)
		}
		if len(header.Name) > maxClaudeArchivePathBytes || len(header.Linkname) > maxClaudeArchivePathBytes {
			return nil, errors.New("claude archive entry path is too long")
		}
		cleanName, err := cleanClaudeArchiveName(header.Name)
		if err != nil {
			return nil, err
		}
		if _, exists := seenEntries[cleanName]; exists {
			return nil, fmt.Errorf("duplicate claude archive entry %q", cleanName)
		}
		seenEntries[cleanName] = struct{}{}
		if cleanName == claudeArchiveScrollback {
			if sawScrollback {
				return nil, errors.New("duplicate scrollback entry")
			}
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				return nil, errors.New("scrollback entry is not a regular file")
			}
			if header.Size > maxClaudeScrollbackBytes {
				return nil, fmt.Errorf("claude scrollback exceeds %d bytes", maxClaudeScrollbackBytes)
			}
			scrollback, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			sawScrollback = true
			continue
		}
		if cleanName != claudeArchiveStateRoot && !strings.HasPrefix(cleanName, claudeArchiveStateRoot+"/") {
			return nil, fmt.Errorf("unexpected claude archive entry %q", header.Name)
		}
		sawState = true
		rel := strings.TrimPrefix(cleanName, claudeArchiveStateRoot)
		rel = strings.TrimPrefix(rel, "/")
		if err := extractClaudeStateEntry(tr, header, extractedState, rel, &dirModes); err != nil {
			return nil, err
		}
	}
	if !sawScrollback {
		return nil, errors.New("claude archive is missing scrollback")
	}
	if !sawState {
		return nil, errors.New("claude archive is missing state")
	}
	if info, err := os.Stat(extractedState); err != nil || !info.IsDir() {
		return nil, errors.New("claude archive state root is not a directory")
	}
	// Validate every checkpoint invariant while the previous live state is
	// still intact. A checkpoint always contains these entries; silently
	// recreating a missing workspace/home would turn corruption into data loss.
	if err := validateRestoredClaudeState(extractedState); err != nil {
		return nil, err
	}

	// Directory permissions are applied after their children have been created.
	sort.Slice(dirModes, func(i, j int) bool {
		return strings.Count(dirModes[i].name, string(filepath.Separator)) >
			strings.Count(dirModes[j].name, string(filepath.Separator))
	})
	for _, item := range dirModes {
		if err := os.Chmod(item.name, item.mode.Perm()); err != nil {
			return nil, fmt.Errorf("restore directory mode %s: %w", item.name, err)
		}
	}
	if err := installClaudeState(extractedState, stateDir); err != nil {
		return nil, err
	}
	return scrollback, nil
}

type claudeArchiveLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *claudeArchiveLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("claude archive exceeds %d bytes", maxClaudeArchiveBytes)
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func cleanClaudeArchiveName(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return "", fmt.Errorf("unsafe claude archive entry %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe claude archive entry %q", name)
	}
	return clean, nil
}

func extractClaudeStateEntry(tr *tar.Reader, header *tar.Header, root, rel string, dirModes *[]restoredDirMode) error {
	if rel == "" && header.Typeflag != tar.TypeDir {
		return errors.New("claude archive state root must be a directory")
	}
	dst, err := safeClaudeArchiveDestination(root, rel)
	if err != nil {
		return err
	}
	if rel != "" {
		if err := ensureSafeClaudeParents(root, filepath.Dir(filepath.FromSlash(rel))); err != nil {
			return err
		}
	}

	mode := header.FileInfo().Mode()
	switch header.Typeflag {
	case tar.TypeDir:
		if info, err := os.Lstat(dst); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("archive directory collides with non-directory %s", rel)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		*dirModes = append(*dirModes, restoredDirMode{name: dst, mode: mode})
		return nil
	case tar.TypeReg, tar.TypeRegA:
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create restored file %s: %w", rel, err)
		}
		_, copyErr := io.Copy(f, tr)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := os.Chmod(dst, mode.Perm()); err != nil {
			return err
		}
		return nil
	case tar.TypeSymlink:
		if err := validateClaudeSymlink(rel, header.Linkname); err != nil {
			return err
		}
		if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("duplicate restored entry %s", rel)
			}
			return err
		}
		if err := os.Symlink(filepath.FromSlash(header.Linkname), dst); err != nil {
			return fmt.Errorf("restore symlink %s: %w", rel, err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported claude archive entry %q (type %d)", header.Name, header.Typeflag)
	}
}

func safeClaudeArchiveDestination(root, rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute claude state entry %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("claude state entry escapes root: %q", rel)
	}
	if clean == "." {
		return root, nil
	}
	dst := filepath.Join(root, clean)
	if dst != root && !strings.HasPrefix(dst, root+string(filepath.Separator)) {
		return "", fmt.Errorf("claude state entry escapes root: %q", rel)
	}
	return dst, nil
}

// ensureSafeClaudeParents creates missing parents one component at a time and
// rejects symlink parents, preventing a preceding archive entry from redirecting
// a later file outside the staging directory.
func ensureSafeClaudeParents(root, relParent string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	clean := filepath.Clean(relParent)
	if clean == "." || clean == "" {
		return nil
	}
	current := root
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			return errors.New("archive parent escapes restore root")
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case !info.IsDir():
			return fmt.Errorf("archive parent %s is not a directory", current)
		}
	}
	return nil
}

func validateClaudeSymlink(rel, link string) error {
	if link == "" || strings.Contains(link, "\\") || filepath.IsAbs(filepath.FromSlash(link)) {
		return fmt.Errorf("unsafe symlink %q -> %q", rel, link)
	}
	// Leading .. components are common and safe when the link's own parent is
	// still within state (for example node_modules/.bin/tool -> ../tool/bin).
	// A .. after a normal component is ambiguous because that component may be
	// another symlink: lexical Clean would miss chained-link escapes.
	sawNormal := false
	for _, component := range strings.Split(link, "/") {
		switch component {
		case "", ".":
		case "..":
			if sawNormal {
				return fmt.Errorf("unsafe chained symlink %q -> %q", rel, link)
			}
		default:
			sawNormal = true
		}
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(rel)), filepath.FromSlash(link)))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("symlink escapes claude state: %q -> %q", rel, link)
	}
	return nil
}

func validateRestoredClaudeState(root string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() {
		return errors.New("claude state root is not a directory")
	}
	for _, name := range []string{"workspace", "home"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("claude archive is missing %s directory", name)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("claude archive %s is not a directory", name)
		}
	}
	runtimePath := filepath.Join(root, claudeRuntimeStateFile)
	info, err := os.Lstat(runtimePath)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("claude archive is missing runtime state")
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("claude archive runtime state is not a regular file")
	}
	if _, err = loadRequiredClaudeRuntimeState(root); err != nil {
		return err
	}
	return validateClaudeManagedSettings(filepath.Join(root, "home"))
}

func installClaudeState(staged, target string) error {
	if info, err := os.Stat(staged); err != nil {
		return fmt.Errorf("restored state missing: %w", err)
	} else if !info.IsDir() {
		return errors.New("restored state root is not a directory")
	}
	parent := filepath.Dir(target)
	var backup string
	if _, err := os.Lstat(target); err == nil {
		placeholder, err := os.MkdirTemp(parent, "."+filepath.Base(target)+"-backup-")
		if err != nil {
			return err
		}
		if err := os.Remove(placeholder); err != nil {
			return err
		}
		backup = placeholder
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("move existing claude state aside: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.Rename(staged, target); err != nil {
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("install restored claude state: %w", err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove replaced claude state: %w", err)
		}
	}
	return nil
}
