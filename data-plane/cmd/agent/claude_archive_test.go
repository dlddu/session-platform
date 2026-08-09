package main

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type checkpointHTTPResult struct {
	status int
	body   []byte
	err    error
}

func abortClaudeCheckpoint(t *testing.T, serverURL, checkpointID string, wantStatus int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+"/checkpoint/abort", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(claudeCheckpointIDHeader, checkpointID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("abort checkpoint %q status=%d, want %d", checkpointID, resp.StatusCode, wantStatus)
	}
}

func waitClaudeAdmissionClosed(t *testing.T, c *claudeWorkload) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		closed := !c.accepting
		c.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("checkpoint did not close write admission")
}

func waitClaudeAdmissionOpen(t *testing.T, c *claudeWorkload) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		open := c.accepting && !c.checkpointing
		c.mu.Unlock()
		if open {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("checkpoint did not reopen write admission")
}

// AC-E5: checkpoint closes admission first, but all prompts accepted before that
// barrier finish in FIFO order before archive creation.
func TestClaudeCheckpointDrainsAcceptedQueue(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, "first", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	writeViaHTTP(t, srv, "second", http.StatusOK)

	result := make(chan checkpointHTTPResult, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
		if err != nil {
			result <- checkpointHTTPResult{err: err}
			return
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		result <- checkpointHTTPResult{status: resp.StatusCode, body: body, err: readErr}
	}()
	waitClaudeAdmissionClosed(t, c)
	writeViaHTTP(t, srv, "too late", http.StatusConflict)

	select {
	case got := <-result:
		t.Fatalf("checkpoint returned before active job drained: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	runner.release <- struct{}{}
	second := receiveClaudeRun(t, runner.started)
	if got := second.argv[len(second.argv)-1]; got != "second" {
		t.Fatalf("second queued prompt = %q", got)
	}
	select {
	case got := <-result:
		t.Fatalf("checkpoint returned before queued job drained: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	runner.release <- struct{}{}

	select {
	case got := <-result:
		if got.err != nil || got.status != http.StatusOK || len(got.body) == 0 {
			t.Fatalf("checkpoint result = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint did not finish after queue drained")
	}
	if runs, maxActive := runner.snapshot(); len(runs) != 2 || maxActive != 1 {
		t.Fatalf("runs=%d maxActive=%d, want 2 and 1", len(runs), maxActive)
	}
}

// State files, conversation HOME, output bytes, and the --continue flag survive
// a filesystem archive round trip. A pre-snapshot cursor still points exactly
// at the end of the restored history.
func TestClaudeArchiveRestoreRoundTripPreservesStateCursorAndResume(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.output = func(_ int, opts runnerOptions) {
		_, _ = io.WriteString(opts.Stdout, "before-snapshot\n")
	}
	c1, srv1 := newClaudeTestServer(t, runner, "claude-roundtrip", false, nil)

	if err := os.WriteFile(filepath.Join(c1.workDir, "marker.txt"), []byte("workspace-state"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c1.homeDir, "conversation.json"), []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsBefore, err := os.ReadFile(filepath.Join(c1.homeDir, claudeSettingsDir, claudeSettingsFile))
	if err != nil {
		t.Fatalf("read managed settings before checkpoint: %v", err)
	}
	if err := os.Symlink("marker.txt", filepath.Join(c1.workDir, "marker-link")); err != nil {
		t.Fatal(err)
	}
	writeViaHTTP(t, srv1, "remember this", http.StatusOK)
	before, cursorBefore := eventuallyRead(t, srv1, 0, func(payload string) bool {
		return strings.Contains(payload, "before-snapshot")
	})
	waitClaudeIdle(t, c1)

	resp, err := http.Post(srv1.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	archive, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status=%d err=%v", resp.StatusCode, err)
	}

	runner2 := newFakeClaudeRunner()
	runner2.output = func(_ int, opts runnerOptions) {
		_, _ = io.WriteString(opts.Stdout, "after-restore\n")
	}
	state2 := filepath.Join(t.TempDir(), "restored-state")
	c2, srv2 := newClaudeTestServerAt(t, runner2, "claude-roundtrip", true, nil, state2)
	restoreResp, err := http.Post(srv2.URL+"/restore", "application/x-tar", bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	_, _ = io.Copy(io.Discard, restoreResp.Body)
	restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		t.Fatalf("restore status=%d", restoreResp.StatusCode)
	}

	full, restoredCursor := readViaHTTP(t, srv2, 0)
	if full != before || restoredCursor != cursorBefore {
		t.Fatalf("restored output/cursor = (%q,%d), want (%q,%d)", full, restoredCursor, before, cursorBefore)
	}
	if delta, next := readViaHTTP(t, srv2, cursorBefore); delta != "" || next != cursorBefore {
		t.Fatalf("pre-snapshot cursor after restore = (%q,%d)", delta, next)
	}
	marker, err := os.ReadFile(filepath.Join(c2.workDir, "marker.txt"))
	if err != nil || string(marker) != "workspace-state" {
		t.Fatalf("workspace marker = %q err=%v", marker, err)
	}
	history, err := os.ReadFile(filepath.Join(c2.homeDir, "conversation.json"))
	if err != nil || string(history) != "history" {
		t.Fatalf("conversation history = %q err=%v", history, err)
	}
	settingsAfter, err := os.ReadFile(filepath.Join(c2.homeDir, claudeSettingsDir, claudeSettingsFile))
	if err != nil || !bytes.Equal(settingsAfter, settingsBefore) {
		t.Fatalf("managed settings after restore = %q err=%v, want %q", settingsAfter, err, settingsBefore)
	}
	link, err := os.Readlink(filepath.Join(c2.workDir, "marker-link"))
	if err != nil || link != "marker.txt" {
		t.Fatalf("restored symlink = %q err=%v", link, err)
	}

	writeViaHTTP(t, srv2, "continue", http.StatusOK)
	run := receiveClaudeRun(t, runner2.started)
	want := []string{
		"claude", "--continue", "--model", "claude-roundtrip", "-p", "--output-format",
		"stream-json", "--verbose", "--include-partial-messages", "--", "continue",
	}
	if !reflect.DeepEqual(run.argv, want) {
		t.Fatalf("post-restore argv = %q, want %q", run.argv, want)
	}
	after, _ := eventuallyRead(t, srv2, cursorBefore, func(payload string) bool {
		return strings.Contains(payload, "after-restore")
	})
	if strings.Contains(after, "before-snapshot") {
		t.Fatalf("post-restore delta replayed old output: %q", after)
	}
}

func TestClaudeFullOutputDrainsAcceptedQueueAndSurvivesArchiveRestore(t *testing.T) {
	limit := len(claudeOutputLimitMarker) + 6
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	runner.output = func(_ int, opts runnerOptions) {
		_, _ = io.WriteString(opts.Stdout, "abcdefghij")
	}
	c1, srv1 := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)
	c1.scrollbackLimit = limit

	writeViaHTTP(t, srv1, "fills output", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	// This prompt is accepted before the first invocation fills the scrollback.
	// It must still execute even though its output will be discarded.
	writeViaHTTP(t, srv1, "already queued", http.StatusOK)
	runner.release <- struct{}{}
	_ = receiveClaudeRun(t, runner.started)
	runner.release <- struct{}{}
	waitClaudeIdle(t, c1)

	full, cursor := readViaHTTP(t, srv1, 0)
	if got, want := full, "abcdef"+claudeOutputLimitMarker; got != want {
		t.Fatalf("full bounded output = %q, want %q", got, want)
	}
	if cursor != limit {
		t.Fatalf("bounded output cursor = %d, want %d", cursor, limit)
	}
	if runs, _ := runner.snapshot(); len(runs) != 2 {
		t.Fatalf("accepted runs = %d, want 2", len(runs))
	}
	writeViaHTTP(t, srv1, "too late", http.StatusInsufficientStorage)

	checkpointResp, err := http.Post(srv1.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatalf("checkpoint full output: %v", err)
	}
	archive, readErr := io.ReadAll(checkpointResp.Body)
	checkpointResp.Body.Close()
	if readErr != nil || checkpointResp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint full output status=%d err=%v", checkpointResp.StatusCode, readErr)
	}

	c2, srv2 := newClaudeTestServer(t, newFakeClaudeRunner(), platformDefaultModel, true, nil)
	c2.scrollbackLimit = limit
	restoreResp, err := http.Post(srv2.URL+"/restore", "application/x-tar", bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("restore full output: %v", err)
	}
	_, _ = io.Copy(io.Discard, restoreResp.Body)
	restoreResp.Body.Close()
	if restoreResp.StatusCode != http.StatusOK {
		t.Fatalf("restore full output status=%d", restoreResp.StatusCode)
	}
	restored, restoredCursor := readViaHTTP(t, srv2, 0)
	if restored != full || restoredCursor != cursor {
		t.Fatalf("restored bounded output = (%q,%d), want (%q,%d)", restored, restoredCursor, full, cursor)
	}
	writeViaHTTP(t, srv2, "still full", http.StatusInsufficientStorage)
}

func maliciousClaudeArchive(t *testing.T, badName, link string) []byte {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: claudeArchiveScrollback, Mode: 0o600, Size: 0}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: claudeArchiveStateRoot, Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
		t.Fatal(err)
	}
	header := &tar.Header{Name: badName, Mode: 0o600}
	if link == "" {
		header.Typeflag = tar.TypeReg
		header.Size = 3
	} else {
		header.Typeflag = tar.TypeSymlink
		header.Linkname = link
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if link == "" {
		_, _ = tw.Write([]byte("bad"))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func incompleteClaudeArchive(t *testing.T, workspace, home bool, runtimeJSON string) []byte {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	writeHeader := func(header *tar.Header, body string) {
		t.Helper()
		header.Size = int64(len(body))
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := io.WriteString(tw, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeHeader(&tar.Header{Name: claudeArchiveScrollback, Typeflag: tar.TypeReg, Mode: 0o600}, "")
	writeHeader(&tar.Header{Name: claudeArchiveStateRoot, Typeflag: tar.TypeDir, Mode: 0o700}, "")
	if workspace {
		writeHeader(&tar.Header{Name: "state/workspace", Typeflag: tar.TypeDir, Mode: 0o700}, "")
	}
	if home {
		writeHeader(&tar.Header{Name: "state/home", Typeflag: tar.TypeDir, Mode: 0o700}, "")
	}
	if runtimeJSON != "" {
		writeHeader(&tar.Header{Name: "state/" + claudeRuntimeStateFile, Typeflag: tar.TypeReg, Mode: 0o600}, runtimeJSON)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

// A hostile archive cannot use a path or a symlink to write outside the fresh
// staging root. The target remains absent after either rejection.
func TestClaudeRestoreRejectsTraversal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
		link  string
	}{
		{name: "path traversal", entry: "state/../../escape"},
		{name: "symlink traversal", entry: "state/link", link: "../../escape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeClaudeRunner()
			parent := t.TempDir()
			stateDir := filepath.Join(parent, "state")
			_, srv := newClaudeTestServerAt(t, runner, platformDefaultModel, true, nil, stateDir)
			archive := maliciousClaudeArchive(t, tc.entry, tc.link)

			resp, err := http.Post(srv.URL+"/restore", "application/x-tar", bytes.NewReader(archive))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("restore status=%d, want 400", resp.StatusCode)
			}
			if _, err := os.Lstat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
				t.Fatalf("escape path exists after rejected restore: %v", err)
			}
			if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
				t.Fatalf("state target mutated after rejected restore: %v", err)
			}
		})
	}
}

func TestClaudeSymlinkValidationRejectsChainedEscapeButAllowsLeadingParent(t *testing.T) {
	if err := validateClaudeSymlink("a", "dir/link/.."); err == nil ||
		!strings.Contains(err.Error(), "chained") {
		t.Fatalf("chained symlink escape was accepted: %v", err)
	}
	if err := validateClaudeSymlink("workspace/node_modules/.bin/tool", "../tool/bin"); err != nil {
		t.Fatalf("ordinary leading-parent symlink was rejected: %v", err)
	}
}

func TestClaudeRestoreRequiresCompleteState(t *testing.T) {
	const validRuntime = "{\"version\":1,\"hasRun\":true}\n"
	for _, tc := range []struct {
		name        string
		workspace   bool
		home        bool
		runtimeJSON string
		wantError   string
	}{
		{name: "missing runtime", workspace: true, home: true, wantError: "runtime state"},
		{name: "missing workspace", home: true, runtimeJSON: validRuntime, wantError: "workspace"},
		{name: "missing home", workspace: true, runtimeJSON: validRuntime, wantError: "home"},
		{name: "runtime missing hasRun", workspace: true, home: true, runtimeJSON: "{\"version\":1}\n", wantError: "hasRun"},
		{name: "runtime trailing JSON", workspace: true, home: true, runtimeJSON: validRuntime + "{}", wantError: "trailing JSON"},
		{name: "missing managed settings", workspace: true, home: true, runtimeJSON: validRuntime, wantError: "managed settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			_, srv := newClaudeTestServerAt(t, newFakeClaudeRunner(), platformDefaultModel, true, nil, stateDir)
			archive := incompleteClaudeArchive(t, tc.workspace, tc.home, tc.runtimeJSON)
			resp, err := http.Post(srv.URL+"/restore", "application/x-tar", bytes.NewReader(archive))
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("restore status=%d err=%v body=%q", resp.StatusCode, readErr, body)
			}
			if !strings.Contains(string(body), tc.wantError) {
				t.Fatalf("restore body=%q, want %q", body, tc.wantError)
			}
			if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
				t.Fatalf("state target mutated after rejected restore: %v", err)
			}
		})
	}
}

// A local archive/stream failure means no durable snapshot exists. The session
// remains live, so admission must reopen instead of becoming permanently
// write-closed.
func TestClaudeCheckpointFailureReopensAdmission(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	badLink := filepath.Join(c.workDir, "escaping-link")
	if err := os.Symlink("../../outside-state", badLink); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("checkpoint status=%d, want 500", resp.StatusCode)
	}

	c.mu.Lock()
	accepting, checkpointing := c.accepting, c.checkpointing
	c.mu.Unlock()
	if !accepting || checkpointing {
		t.Fatalf("after failed checkpoint accepting=%v checkpointing=%v", accepting, checkpointing)
	}
	if err := os.Remove(badLink); err != nil {
		t.Fatal(err)
	}
	writeViaHTTP(t, srv, "still live", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
}

func TestClaudeCheckpointRejectsUnrestorableLiveState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(*testing.T, *claudeWorkload)
	}{
		{
			name: "missing workspace",
			tamper: func(t *testing.T, c *claudeWorkload) {
				t.Helper()
				if err := os.RemoveAll(c.workDir); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "expanded permission policy",
			tamper: func(t *testing.T, c *claudeWorkload) {
				t.Helper()
				settingsPath := filepath.Join(c.homeDir, claudeSettingsDir, claudeSettingsFile)
				data := []byte(`{"permissions":{"allow":["Read","Write","Edit","Glob","Grep","Bash"],"defaultMode":"bypassPermissions"}}`)
				if err := os.WriteFile(settingsPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := newClaudeTestServer(t, newFakeClaudeRunner(), platformDefaultModel, false, nil)
			tc.tamper(t, c)
			resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("checkpoint status=%d, want 500", resp.StatusCode)
			}
			waitClaudeAdmissionOpen(t, c)
		})
	}
}

func TestClaudeCompletedCheckpointAbortIsIdempotentAndReopensAdmission(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint status=%d err=%v", resp.StatusCode, readErr)
	}
	checkpointID := resp.Header.Get(claudeCheckpointIDHeader)
	if checkpointID == "" {
		t.Fatal("checkpoint response is missing generation id")
	}
	c.mu.Lock()
	accepting, checkpointing, ready := c.accepting, c.checkpointing, c.checkpointReady
	c.mu.Unlock()
	if accepting || !checkpointing || !ready {
		t.Fatalf("completed checkpoint accepting=%v checkpointing=%v ready=%v", accepting, checkpointing, ready)
	}
	writeViaHTTP(t, srv, "closed until decision", http.StatusConflict)

	for i := 0; i < 2; i++ {
		abortClaudeCheckpoint(t, srv.URL, checkpointID, http.StatusOK)
	}
	waitClaudeAdmissionOpen(t, c)
	writeViaHTTP(t, srv, "still live", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
}

func TestClaudeStaleCheckpointAbortCannotReopenNewGeneration(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	checkpoint := func(requestedID string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/checkpoint", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(claudeCheckpointIDHeader, requestedID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("checkpoint status=%d err=%v", resp.StatusCode, readErr)
		}
		got := resp.Header.Get(claudeCheckpointIDHeader)
		if got != requestedID {
			t.Fatalf("checkpoint generation=%q, want CP-owned %q", got, requestedID)
		}
		return got
	}

	firstID := checkpoint("00000000000000000000000000000001")
	abortClaudeCheckpoint(t, srv.URL, firstID, http.StatusOK)
	secondID := checkpoint("00000000000000000000000000000002")
	if firstID == secondID || secondID == "" {
		t.Fatalf("checkpoint ids first=%q second=%q", firstID, secondID)
	}
	abortClaudeCheckpoint(t, srv.URL, firstID, http.StatusConflict)
	c.mu.Lock()
	stillClosed := c.checkpointing && c.checkpointReady && !c.accepting
	c.mu.Unlock()
	if !stillClosed {
		t.Fatal("stale abort reopened a newer checkpoint generation")
	}
	abortClaudeCheckpoint(t, srv.URL, secondID, http.StatusOK)
	waitClaudeAdmissionOpen(t, c)
}

func TestClaudeAbortBeforeCheckpointRequestIsIdempotent(t *testing.T) {
	c, srv := newClaudeTestServer(t, newFakeClaudeRunner(), platformDefaultModel, false, nil)
	const generation = "00000000000000000000000000000003"

	// The control plane persists prepare before calling /checkpoint. A crash in
	// that gap must recover as an already-aborted operation, not wedge forever.
	abortClaudeCheckpoint(t, srv.URL, generation, http.StatusOK)
	waitClaudeAdmissionOpen(t, c)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/checkpoint", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(claudeCheckpointIDHeader, "not-a-128-bit-id")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid CP generation status=%d, want 400", resp.StatusCode)
	}
}

func TestClaudeCanceledCheckpointWaitReopensAdmission(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, "long-running", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/checkpoint", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()
	waitClaudeAdmissionClosed(t, c)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled checkpoint request remained blocked")
	}
	waitClaudeAdmissionOpen(t, c)
	writeViaHTTP(t, srv, "accepted after cancel", http.StatusOK)
}

func TestClaudeCheckpointErrorsRedactCredentialLiterals(t *testing.T) {
	const secret = "credential-in-path"
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, []string{secret})

	if err := os.Symlink("../../"+secret, filepath.Join(c.workDir, secret)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("checkpoint status=%d err=%v", resp.StatusCode, readErr)
	}
	if strings.Contains(string(body), secret) || !strings.Contains(string(body), redactedLiteral) {
		t.Fatalf("checkpoint error was not redacted: %q", body)
	}
}
