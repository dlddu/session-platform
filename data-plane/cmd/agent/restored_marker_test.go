package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A restored claude-family pod whose container restarts (an OOM kill) starts
// from the restored state on its volume instead of waiting for an archive the
// control plane will not send again.
func TestRestoredClaudeWorkloadSurvivesContainerRestart(t *testing.T) {
	src, srcSrv := newClaudeTestServer(t, newFakeClaudeRunner(), platformDefaultModel, false, nil)
	writeViaHTTP(t, srcSrv, "before", http.StatusOK)
	eventuallyRead(t, srcSrv, 0, func(p string) bool { return strings.Contains(p, "status=ok") })
	var archive bytes.Buffer
	if skipped, err := src.autoArchiveTo(context.Background(), &archive); err != nil || skipped {
		t.Fatalf("archive source: skipped=%v err=%v", skipped, err)
	}

	stateDir := filepath.Join(t.TempDir(), "state")
	restored, rsrv := newClaudeTestServerAt(t, newFakeClaudeRunner(), platformDefaultModel, true, nil, stateDir)
	if restored.isReady() {
		t.Fatal("a restore target was ready before its restore")
	}
	resp, err := http.Post(rsrv.URL+"/restore", "application/x-tar", bytes.NewReader(archive.Bytes()))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("restore: %v %v", resp, err)
	}
	resp.Body.Close()
	if strings.HasPrefix(restoredMarkerPath(stateDir), filepath.Clean(stateDir)+string(filepath.Separator)) {
		t.Fatalf("marker %s lies inside the archived tree %s", restoredMarkerPath(stateDir), stateDir)
	}
	restored.Close()

	runner := newFakeClaudeRunner()
	again, asrv := newClaudeTestServerAt(t, runner, platformDefaultModel, true, nil, stateDir)
	if !again.isReady() {
		t.Fatal("the restarted container waits for a restore that already landed")
	}
	writeViaHTTP(t, asrv, "after restart", http.StatusOK)
	if got := receiveClaudeRun(t, runner.started); !slicesContain(got.argv, "--resume") {
		t.Fatalf("the restarted workload did not continue the restored conversation: %v", got.argv)
	}
}

// A restore target whose restore never landed still waits for one.
func TestRestoreTargetWithoutMarkerStillAwaitsRestore(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if !awaitsRestore(true, stateDir) {
		t.Fatal("a fresh restore target did not await its restore")
	}
	if awaitsRestore(false, stateDir) {
		t.Fatal("a non-restore pod awaited a restore")
	}
	c, _ := newClaudeTestServerAt(t, newFakeClaudeRunner(), platformDefaultModel, true, nil, stateDir)
	if c.isReady() {
		t.Fatal("a restore target without a landed restore was ready")
	}
}

// The shell agent records a landed restore the same way.
func TestRestoredShellMarksItsRestore(t *testing.T) {
	src, srcSrv := newShellAgentServer(t)
	writeViaHTTP(t, srcSrv, "echo kept > note.txt; echo done-$((1+1))\n", http.StatusOK)
	eventuallyRead(t, srcSrv, 0, func(p string) bool { return strings.Contains(p, "done-2") })
	var archive bytes.Buffer
	if skipped, err := src.autoArchiveTo(context.Background(), &archive); err != nil || skipped {
		t.Fatalf("archive source: skipped=%v err=%v", skipped, err)
	}

	dst, restored := newRestoreAgentServer(t)
	if !awaitsRestore(true, dst.stateDir) {
		t.Fatal("restore target did not await its restore")
	}
	if code, body := postRestore(t, restored, archive.Bytes()); code != http.StatusOK {
		t.Fatalf("restore status=%d body=%q", code, body)
	}
	if awaitsRestore(true, dst.stateDir) {
		t.Fatal("a restarted shell container would wait for a restore that already landed")
	}
	if _, err := os.Stat(filepath.Join(dst.homeDir(), "note.txt")); err != nil {
		t.Fatalf("restored home missing its file: %v", err)
	}
}
