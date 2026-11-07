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
	"time"
)

const (
	claudeArchiveScrollback = "scrollback"
	claudeArchiveStateRoot  = "state"
	// stateArchiveSharedRoot carries the session's shared volume (AC-F5) for
	// the workload types that mount one. Archives without it stay valid.
	stateArchiveSharedRoot = "shared"
)

// writeStateArchive captures a workload's filesystem state, plus its shared
// volume when sharedDir is set, and the output buffer. Every workload type
// snapshots through this one format: Claude-family agents drain their worker
// first and the shell agent pauses its process tree first, so the trees cannot
// change while they are walked.
func writeStateArchive(w io.Writer, stateDir, sharedDir string, scrollback []byte) error {
	return writeStateArchiveExcluding(w, stateDir, sharedDir, scrollback, nil)
}

// writeStateArchiveExcluding is writeStateArchive leaving out the named
// regular files of the state tree (slash paths relative to stateDir). The one
// user is the auth slot's login file (AC-N4): it belongs to the slot, not to
// the session, so it never travels in the session's archive.
func writeStateArchiveExcluding(w io.Writer, stateDir, sharedDir string, scrollback []byte, exclude []string) error {
	skip := make(map[string]bool, len(exclude))
	for _, rel := range exclude {
		skip[path.Clean(rel)] = true
	}
	if int64(len(scrollback)) > maxClaudeScrollbackBytes {
		return fmt.Errorf("workload scrollback exceeds %d bytes", maxClaudeScrollbackBytes)
	}
	w = &claudeArchiveLimitWriter{writer: w, remaining: maxClaudeArchiveBytes}

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
	if err := archiveTree(tw, stateDir, claudeArchiveStateRoot, &entryCount, skip); err != nil {
		_ = tw.Close()
		return err
	}
	if sharedDir != "" {
		if err := archiveTree(tw, sharedDir, stateArchiveSharedRoot, &entryCount, nil); err != nil {
			_ = tw.Close()
			return err
		}
	}
	return tw.Close()
}

// archiveTree writes dir into tw under prefix, refusing anything but regular
// files, directories and symlinks that stay inside the tree. Regular files
// whose slash path relative to dir is in skip are left out.
func archiveTree(tw *tar.Writer, dir, prefix string, entryCount *int, skip map[string]bool) error {
	rootInfo, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect %s root: %w", prefix, err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("%s root is not a directory", prefix)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s root: %w", prefix, err)
	}
	defer root.Close()
	openedRootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(rootInfo, openedRootInfo) {
		return fmt.Errorf("%s root changed while archiving", prefix)
	}

	return filepath.WalkDir(dir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		archiveName := prefix
		if rel != "." {
			archiveName += "/" + filepath.ToSlash(rel)
		}
		if skip[filepath.ToSlash(rel)] && entry.Type().IsRegular() {
			return nil
		}
		*entryCount++
		if *entryCount > maxClaudeArchiveEntries {
			return fmt.Errorf("workload archive exceeds %d entries", maxClaudeArchiveEntries)
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
			return fmt.Errorf("unsupported %s entry %s (%s)", prefix, name, info.Mode())
		}
		if len(archiveName) > maxClaudeArchivePathBytes || len(link) > maxClaudeArchivePathBytes {
			return errors.New("workload archive entry path is too long")
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
			return fmt.Errorf("%s entry changed while archiving: %s", prefix, name)
		}
		_, copyErr := io.CopyN(tw, f, info.Size())
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

type restoredDirMode struct {
	name    string
	mode    fs.FileMode
	modTime time.Time
}

// restoreStateArchive validates and extracts into a fresh sibling directory,
// then atomically swaps that directory into place. A malformed archive never
// partially mutates the live target. Shared-volume entries are expanded into
// sharedDir, which must then be set: dropping them would lose AC-F5 files.
func restoreStateArchive(r io.Reader, stateDir, sharedDir string, validate func(string) error) ([]byte, error) {
	stateDir = filepath.Clean(stateDir)
	if !filepath.IsAbs(stateDir) || stateDir == string(filepath.Separator) {
		return nil, fmt.Errorf("unsafe workload state target %q", stateDir)
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
	extractedShared := filepath.Join(tempRoot, stateArchiveSharedRoot)

	tr := tar.NewReader(r)
	var scrollback []byte
	var sawScrollback, sawState, sawShared bool
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
			return nil, fmt.Errorf("workload archive exceeds %d entries", maxClaudeArchiveEntries)
		}
		if len(header.Name) > maxClaudeArchivePathBytes || len(header.Linkname) > maxClaudeArchivePathBytes {
			return nil, errors.New("workload archive entry path is too long")
		}
		cleanName, err := cleanClaudeArchiveName(header.Name)
		if err != nil {
			return nil, err
		}
		if _, exists := seenEntries[cleanName]; exists {
			return nil, fmt.Errorf("duplicate workload archive entry %q", cleanName)
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
				return nil, fmt.Errorf("workload scrollback exceeds %d bytes", maxClaudeScrollbackBytes)
			}
			scrollback, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			sawScrollback = true
			continue
		}
		var treeRoot, rel string
		switch {
		case cleanName == claudeArchiveStateRoot || strings.HasPrefix(cleanName, claudeArchiveStateRoot+"/"):
			sawState = true
			treeRoot = extractedState
			rel = strings.TrimPrefix(strings.TrimPrefix(cleanName, claudeArchiveStateRoot), "/")
		case cleanName == stateArchiveSharedRoot || strings.HasPrefix(cleanName, stateArchiveSharedRoot+"/"):
			sawShared = true
			treeRoot = extractedShared
			rel = strings.TrimPrefix(strings.TrimPrefix(cleanName, stateArchiveSharedRoot), "/")
		default:
			return nil, fmt.Errorf("unexpected workload archive entry %q", header.Name)
		}
		if err := extractClaudeStateEntry(tr, header, treeRoot, rel, &dirModes); err != nil {
			return nil, err
		}
	}
	if !sawScrollback {
		return nil, errors.New("workload archive is missing scrollback")
	}
	if !sawState {
		return nil, errors.New("workload archive is missing state")
	}
	if info, err := os.Stat(extractedState); err != nil || !info.IsDir() {
		return nil, errors.New("workload archive state root is not a directory")
	}
	if sawShared {
		if sharedDir == "" {
			return nil, errors.New("workload archive carries shared volume files but this pod mounts no shared volume")
		}
		if info, err := os.Stat(extractedShared); err != nil || !info.IsDir() {
			return nil, errors.New("workload archive shared root is not a directory")
		}
	}
	// Validate every checkpoint invariant while the previous live state is
	// still intact. A checkpoint always contains these entries; silently
	// recreating a missing workspace/home would turn corruption into data loss.
	if err := validate(extractedState); err != nil {
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
		if err := restoreModTime(item.name, item.modTime); err != nil {
			return nil, fmt.Errorf("restore directory mtime %s: %w", item.name, err)
		}
	}
	if sawShared {
		// The shared volume is a mount point on another filesystem, so it is
		// filled by copying rather than renamed into place like the state root.
		if err := copyRestoredTree(extractedShared, sharedDir); err != nil {
			return nil, fmt.Errorf("restore shared volume: %w", err)
		}
	}
	if err := installClaudeState(extractedState, stateDir); err != nil {
		return nil, err
	}
	return scrollback, nil
}

// copyRestoredTree copies an already validated staging tree into target,
// creating what is missing and replacing regular files and symlinks that
// already exist.
func copyRestoredTree(staged, target string) error {
	var dirModes []restoredDirMode
	err := filepath.WalkDir(staged, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(staged, name)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if err := ensureSafeClaudeParents(target, filepath.Dir(rel)); err != nil {
			return err
		}
		dst := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			existing, err := os.Lstat(dst)
			switch {
			case errors.Is(err, os.ErrNotExist):
				if err := os.Mkdir(dst, 0o700); err != nil {
					return err
				}
			case err != nil:
				return err
			case !existing.IsDir():
				return fmt.Errorf("shared directory collides with non-directory %s", rel)
			}
			dirModes = append(dirModes, restoredDirMode{name: dst, mode: info.Mode(), modTime: info.ModTime()})
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if err := removeIfNotDir(dst, rel); err != nil {
				return err
			}
			return os.Symlink(link, dst)
		case info.Mode().IsRegular():
			if err := removeIfNotDir(dst, rel); err != nil {
				return err
			}
			src, err := os.Open(name)
			if err != nil {
				return err
			}
			defer src.Close()
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, src)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
				return err
			}
			return restoreModTime(dst, info.ModTime())
		default:
			return fmt.Errorf("unsupported shared entry %s (%s)", rel, info.Mode())
		}
	})
	if err != nil {
		return err
	}
	sort.Slice(dirModes, func(i, j int) bool {
		return strings.Count(dirModes[i].name, string(filepath.Separator)) >
			strings.Count(dirModes[j].name, string(filepath.Separator))
	})
	for _, item := range dirModes {
		if err := os.Chmod(item.name, item.mode.Perm()); err != nil {
			return fmt.Errorf("restore directory mode %s: %w", item.name, err)
		}
		if err := restoreModTime(item.name, item.modTime); err != nil {
			return fmt.Errorf("restore directory mtime %s: %w", item.name, err)
		}
	}
	return nil
}

func removeIfNotDir(dst, rel string) error {
	existing, err := os.Lstat(dst)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case existing.IsDir():
		return fmt.Errorf("shared file collides with directory %s", rel)
	default:
		return os.Remove(dst)
	}
}

type claudeArchiveLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *claudeArchiveLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("workload archive exceeds %d bytes", maxClaudeArchiveBytes)
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func cleanClaudeArchiveName(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return "", fmt.Errorf("unsafe workload archive entry %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe workload archive entry %q", name)
	}
	return clean, nil
}

func extractClaudeStateEntry(tr *tar.Reader, header *tar.Header, root, rel string, dirModes *[]restoredDirMode) error {
	if rel == "" && header.Typeflag != tar.TypeDir {
		return errors.New("workload archive state root must be a directory")
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
		*dirModes = append(*dirModes, restoredDirMode{name: dst, mode: mode, modTime: header.ModTime})
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
		return restoreModTime(dst, header.ModTime)
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
		return fmt.Errorf("unsupported workload archive entry %q (type %d)", header.Name, header.Typeflag)
	}
}

func safeClaudeArchiveDestination(root, rel string) (string, error) {
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute workload state entry %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workload state entry escapes root: %q", rel)
	}
	if clean == "." {
		return root, nil
	}
	dst := filepath.Join(root, clean)
	if dst != root && !strings.HasPrefix(dst, root+string(filepath.Separator)) {
		return "", fmt.Errorf("workload state entry escapes root: %q", rel)
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
		return fmt.Errorf("symlink escapes workload state: %q -> %q", rel, link)
	}
	return nil
}

func validateRestoredClaudeState(root string) error {
	if err := validateRestoredAgentState(root); err != nil {
		return err
	}
	return validateClaudeManagedSettings(filepath.Join(root, "home"))
}

func validateRestoredAgentState(root string) error {
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
	_, err = loadRequiredClaudeRuntimeState(root)
	return err
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
			return fmt.Errorf("move existing workload state aside: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.Rename(staged, target); err != nil {
		if backup != "" {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("install restored workload state: %w", err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove replaced workload state: %w", err)
		}
	}
	return nil
}
