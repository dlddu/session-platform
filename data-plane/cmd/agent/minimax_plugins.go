package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ensureHome stages the platform's MiniMax plugins outside the state tree
// (AC-G5, AC-G6) and copies them into the place mcode scans, ~/.minimax/plugins.
// HOME is archived, so the copy is redone on every boot and restore: an archive
// can never bring back a plugin the current pod did not stage.
//
// miniMaxManagedPluginMarker tags a plugin directory the platform installed.
// Only tagged directories are ever removed, so a plugin the user installed by
// hand inside the session survives every sync.
const miniMaxManagedPluginMarker = ".session-platform-managed"

func miniMaxPluginsDir(homeDir string) string {
	return filepath.Join(homeDir, ".minimax", "plugins")
}

// syncMiniMaxPlugins makes the managed plugins in homeDir exactly the seed. An
// empty seedDir means this pod staged nothing, which removes every managed
// plugin a previous pod left in the archived HOME.
func syncMiniMaxPlugins(homeDir, seedDir string) error {
	pluginsDir := miniMaxPluginsDir(homeDir)
	if err := os.MkdirAll(pluginsDir, 0o700); err != nil {
		return fmt.Errorf("create minimax plugins directory: %w", err)
	}
	seeded := map[string]bool{}
	if seedDir != "" {
		entries, err := os.ReadDir(seedDir)
		if err != nil {
			return fmt.Errorf("read minimax plugin seed: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				seeded[entry.Name()] = true
			}
		}
	}
	installed, err := os.ReadDir(pluginsDir)
	if err != nil {
		return fmt.Errorf("read minimax plugins directory: %w", err)
	}
	for _, entry := range installed {
		if !entry.IsDir() || seeded[entry.Name()] {
			continue
		}
		dir := filepath.Join(pluginsDir, entry.Name())
		if _, err := os.Lstat(filepath.Join(dir, miniMaxManagedPluginMarker)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspect minimax plugin %s: %w", entry.Name(), err)
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove stale minimax plugin %s: %w", entry.Name(), err)
		}
	}
	for name := range seeded {
		target := filepath.Join(pluginsDir, name)
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("replace minimax plugin %s: %w", name, err)
		}
		if err := copyMiniMaxPluginTree(filepath.Join(seedDir, name), target); err != nil {
			return fmt.Errorf("install minimax plugin %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(target, miniMaxManagedPluginMarker), nil, 0o600); err != nil {
			return fmt.Errorf("mark minimax plugin %s: %w", name, err)
		}
	}
	return nil
}

// copyMiniMaxPluginTree copies directories and regular files only. mcode's
// local scan rejects symlinked plugin roots, and a link here could point the
// copy outside the seed.
func copyMiniMaxPluginTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o700)
		case entry.Type().IsRegular():
			return copyMiniMaxPluginFile(path, target)
		default:
			return fmt.Errorf("%s is not a regular file or directory", rel)
		}
	})
}

func copyMiniMaxPluginFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
