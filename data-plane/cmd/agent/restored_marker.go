package main

import (
	"os"
	"path/filepath"
)

// A restore-target pod is told so by DATA_PLANE_RESTORE_MODE, which stays set
// for the pod's life. Once a restore has landed, a container restart (an OOM
// kill, say) must start from the restored state on the pod's volume instead
// of waiting for an archive that will never come again. The marker records
// that the restore landed. It sits beside the state tree, not in it, so no
// archive carries it and a restore cut short never leaves it behind.
const restoredMarkerName = ".session-restored"

func restoredMarkerPath(stateDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(stateDir)), restoredMarkerName)
}

func markRestored(stateDir string) error {
	return os.WriteFile(restoredMarkerPath(stateDir), []byte("restored\n"), 0o600)
}

func restoredBefore(stateDir string) bool {
	_, err := os.Stat(restoredMarkerPath(stateDir))
	return err == nil
}

// awaitsRestore reports whether the agent should start empty and wait for an
// archive.
func awaitsRestore(restoreMode bool, stateDir string) bool {
	return restoreMode && !restoredBefore(stateDir)
}
