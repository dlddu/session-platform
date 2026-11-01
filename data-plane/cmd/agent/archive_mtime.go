package main

import (
	"os"
	"time"
)

// restoreModTime gives a restored entry the modification time it had when it
// was archived. The archive already carries it — tar.FileInfoHeader records
// ModTime, rounded to whole seconds by the default writer — so this only stops
// the restore from discarding it.
//
// Access time is set to the same value: the default tar format does not carry
// one, and leaving it at "now" would make a restored file look freshly read.
// Symlinks are left alone because os.Chtimes follows them and would stamp the
// target instead. A zero time (an entry with no recorded mtime) is skipped.
//
// Directories must be stamped after everything inside them is created, since
// creating a child moves the parent's mtime; callers do this in the same
// deepest-first pass that restores directory modes.
func restoreModTime(name string, modTime time.Time) error {
	if modTime.IsZero() {
		return nil
	}
	return os.Chtimes(name, modTime, modTime)
}
