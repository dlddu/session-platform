package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type recordedClaudeRun struct {
	argv []string
	dir  string
	env  []string
}

type fakeClaudeRunner struct {
	mu        sync.Mutex
	runs      []recordedClaudeRun
	active    int
	maxActive int

	started chan recordedClaudeRun
	release chan struct{}
	output  func(index int, opts runnerOptions)
	runErr  func(index int) error
}

func newFakeClaudeRunner() *fakeClaudeRunner {
	return &fakeClaudeRunner{started: make(chan recordedClaudeRun, 16)}
}

func (f *fakeClaudeRunner) Run(ctx context.Context, argv []string, opts runnerOptions) error {
	f.mu.Lock()
	index := len(f.runs)
	run := recordedClaudeRun{
		argv: append([]string(nil), argv...),
		dir:  opts.Dir,
		env:  append([]string(nil), opts.Env...),
	}
	f.runs = append(f.runs, run)
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()

	f.started <- run
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			f.finish()
			return ctx.Err()
		}
	}
	if f.output != nil {
		f.output(index, opts)
	}
	f.finish()
	if f.runErr != nil {
		return f.runErr(index)
	}
	return nil
}

func (f *fakeClaudeRunner) finish() {
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
}

func (f *fakeClaudeRunner) snapshot() ([]recordedClaudeRun, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedClaudeRun, len(f.runs))
	copy(out, f.runs)
	return out, f.maxActive
}

func newClaudeTestServer(t *testing.T, runner commandRunner, model string, restoreMode bool, redact []string) (*claudeWorkload, *httptest.Server) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	return newClaudeTestServerAt(t, runner, model, restoreMode, redact, stateDir)
}

func newClaudeTestServerAt(t *testing.T, runner commandRunner, model string, restoreMode bool, redact []string, stateDir string) (*claudeWorkload, *httptest.Server) {
	t.Helper()
	c, err := newClaudeWorkload(claudeConfig{
		StateDir:    stateDir,
		Model:       model,
		Binary:      defaultClaudeBinary,
		RestoreMode: restoreMode,
		Runner:      runner,
		Logger:      testLogger(),
		Redact:      redact,
	})
	if err != nil {
		t.Fatalf("new claude workload: %v", err)
	}
	srv := httptest.NewServer(routes(testLogger(), &agent{claude: c}))
	t.Cleanup(func() {
		srv.Close()
		c.Close()
	})
	return c, srv
}

func waitClaudeIdle(t *testing.T, c *claudeWorkload) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		idle := !c.active && c.queue.Len() == 0
		c.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("claude worker did not become idle")
}

func receiveClaudeRun(t *testing.T, ch <-chan recordedClaudeRun) recordedClaudeRun {
	t.Helper()
	select {
	case run := <-ch:
		return run
	case <-time.After(5 * time.Second):
		t.Fatal("claude invocation did not start")
		return recordedClaudeRun{}
	}
}

func envValue(items []string, key string) string {
	prefix := key + "="
	for _, item := range items {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

// AC-E2: /write only enqueues, and the one worker does not start prompt B until
// prompt A has completely returned.
func TestClaudeWriteIsNonBlockingAndSerial(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, srv := newClaudeTestServer(t, runner, "claude-test-model", false, nil)

	start := time.Now()
	writeViaHTTP(t, srv, "first prompt", http.StatusOK)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("first write blocked for %v", took)
	}
	first := receiveClaudeRun(t, runner.started)

	start = time.Now()
	writeViaHTTP(t, srv, "second prompt", http.StatusOK)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("queued write blocked for %v", took)
	}
	select {
	case second := <-runner.started:
		t.Fatalf("second invocation started before first finished: %+v", second)
	case <-time.After(100 * time.Millisecond):
	}

	runner.release <- struct{}{}
	second := receiveClaudeRun(t, runner.started)
	runner.release <- struct{}{}
	waitClaudeIdle(t, c)

	wantFirst := []string{
		"claude", "--model", "claude-test-model", "-p", "--output-format",
		"stream-json", "--verbose", "--include-partial-messages", "--", "first prompt",
	}
	wantSecond := []string{
		"claude", "--continue", "--model", "claude-test-model", "-p", "--output-format",
		"stream-json", "--verbose", "--include-partial-messages", "--", "second prompt",
	}
	if !reflect.DeepEqual(first.argv, wantFirst) {
		t.Fatalf("first argv = %q, want %q", first.argv, wantFirst)
	}
	if !reflect.DeepEqual(second.argv, wantSecond) {
		t.Fatalf("second argv = %q, want %q", second.argv, wantSecond)
	}
	if first.dir != c.workDir {
		t.Fatalf("cwd = %q, want %q", first.dir, c.workDir)
	}
	if got := envValue(first.env, "HOME"); got != c.homeDir {
		t.Fatalf("HOME = %q, want %q", got, c.homeDir)
	}
	if runs, maxActive := runner.snapshot(); len(runs) != 2 || maxActive != 1 {
		t.Fatalf("runs=%d maxActive=%d, want 2 and 1", len(runs), maxActive)
	}
}

// The platform default is selected outside the CLI. Passing its sentinel to
// Claude would name a non-existent model, so --model is intentionally omitted.
func TestClaudePlatformDefaultOmitsModelFlag(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, "hello", http.StatusOK)
	run := receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
	want := []string{
		"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--", "hello",
	}
	if !reflect.DeepEqual(run.argv, want) {
		t.Fatalf("argv = %q, want %q", run.argv, want)
	}
}

func TestClaudeRejectsInvalidConfiguredModel(t *testing.T) {
	for _, model := range []string{
		"--dangerously-skip-permissions",
		"bad model",
		strings.Repeat("a", 129),
	} {
		t.Run(model, func(t *testing.T) {
			workload, err := newClaudeWorkload(claudeConfig{
				StateDir: t.TempDir(),
				Model:    model,
				Runner:   newFakeClaudeRunner(),
			})
			if err == nil {
				workload.Close()
				t.Fatalf("newClaudeWorkload accepted invalid model %q", model)
			}
			if !strings.Contains(err.Error(), "model identifier pattern") {
				t.Fatalf("error = %q, want model identifier pattern", err)
			}
		})
	}
}

func TestClaudeManagedToolPolicyIsProvisionedUnderSessionHome(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, _ := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	if err := validateClaudeManagedSettings(c.homeDir); err != nil {
		t.Fatalf("managed settings are invalid: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(c.homeDir, claudeSettingsDir, claudeSettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range claudeManagedTools {
		if !strings.Contains(string(data), `"`+tool+`"`) {
			t.Fatalf("managed settings %q are missing %s", data, tool)
		}
	}
	if strings.Contains(string(data), "bypassPermissions") {
		t.Fatalf("managed settings bypass permissions: %q", data)
	}
}

func TestClaudePromptCannotInjectCLIOptions(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, "immutable-model", false, nil)

	prompt := "--model=opus --dangerously-skip-permissions"
	writeViaHTTP(t, srv, prompt, http.StatusOK)
	run := receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
	want := []string{
		"claude", "--model", "immutable-model", "-p", "--output-format",
		"stream-json", "--verbose", "--include-partial-messages", "--", prompt,
	}
	if !reflect.DeepEqual(run.argv, want) {
		t.Fatalf("argv = %q, want %q", run.argv, want)
	}
}

func TestClaudeFailedFirstInvocationDoesNotResume(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.runErr = func(index int) error {
		if index == 0 {
			return fmt.Errorf("binary or authentication failed")
		}
		return nil
	}
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, "failed first", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
	writeViaHTTP(t, srv, "new conversation", http.StatusOK)
	second := receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
	if slicesContain(second.argv, "--continue") {
		t.Fatalf("failed first invocation was resumed: %q", second.argv)
	}

	writeViaHTTP(t, srv, "continue successful conversation", http.StatusOK)
	third := receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
	if !slicesContain(third.argv, "--continue") {
		t.Fatalf("third invocation did not resume successful conversation: %q", third.argv)
	}
}

func TestClaudeInvocationTimeoutDoesNotCreateResumeState(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)
	c.runTimeout = 50 * time.Millisecond

	writeViaHTTP(t, srv, "hang", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	waitClaudeIdle(t, c)
	c.mu.Lock()
	hasRun := c.hasRun
	c.mu.Unlock()
	if hasRun {
		t.Fatal("timed-out first invocation created resume state")
	}
}

func TestClaudeRejectsOversizedPromptBeforeQueue(t *testing.T) {
	runner := newFakeClaudeRunner()
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, strings.Repeat("x", maxClaudePromptBytes+1), http.StatusRequestEntityTooLarge)

	c.mu.Lock()
	queued := c.queue.Len()
	queuedBytes := c.queuedBytes
	active := c.active
	c.mu.Unlock()
	runs, _ := runner.snapshot()
	if queued != 0 || queuedBytes != 0 || active || len(runs) != 0 {
		t.Fatalf("oversized prompt admitted: queued=%d bytes=%d active=%v runs=%d", queued, queuedBytes, active, len(runs))
	}
}

func TestClaudeQueueHasTotalByteBudget(t *testing.T) {
	runner := newFakeClaudeRunner()
	runner.release = make(chan struct{})
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	writeViaHTTP(t, srv, "active", http.StatusOK)
	_ = receiveClaudeRun(t, runner.started)
	prompt := strings.Repeat("x", maxClaudePromptBytes)
	accepted := maxClaudeQueuedBytes / maxClaudePromptBytes
	for i := 0; i < accepted; i++ {
		writeViaHTTP(t, srv, prompt, http.StatusOK)
	}
	writeViaHTTP(t, srv, prompt, http.StatusTooManyRequests)
	c.mu.Lock()
	queuedBytes := c.queuedBytes
	c.mu.Unlock()
	if queuedBytes != maxClaudeQueuedBytes {
		t.Fatalf("queued bytes=%d, want %d", queuedBytes, maxClaudeQueuedBytes)
	}
}

func TestClaudeScrollbackCapPreservesIssuedOffsets(t *testing.T) {
	const (
		limit  = 12
		marker = "[full]"
	)
	var output scrollback
	if full := output.appendClaudeBoundedAt([]byte("abc"), limit, marker); full {
		t.Fatal("short first append filled the scrollback")
	}
	_, cursor := output.Since(0)
	if cursor != 3 {
		t.Fatalf("issued cursor = %d, want 3", cursor)
	}
	if full := output.appendClaudeBoundedAt([]byte("def"), limit, marker); full {
		t.Fatal("append exactly to the reserved data limit reported full")
	}
	if full := output.appendClaudeBoundedAt([]byte("gh"), limit, marker); !full {
		t.Fatal("overflow did not seal the scrollback")
	}
	if !output.claudeFullAt(limit, marker) {
		t.Fatal("sealed scrollback still accepts prompts")
	}
	delta, next := output.Since(cursor)
	if got, want := string(delta), "def[full]"; got != want || next != limit {
		t.Fatalf("delta after issued cursor = (%q,%d), want (%q,%d)", got, next, want, limit)
	}
	if full := output.appendClaudeBoundedAt([]byte("discarded"), limit, marker); full {
		t.Fatal("an already full scrollback reported a second transition")
	}
	if all, next := output.Since(0); len(all) != limit || next != limit {
		t.Fatalf("sealed scrollback = %d bytes, cursor %d, want %d", len(all), next, limit)
	}
}

func slicesContain(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// AC-E3/E6: stdout and stderr share one ordered buffer, reads use byte offsets,
// and credential literals are replaced before they can enter that buffer.
func TestClaudeReadCursorAndCredentialRedaction(t *testing.T) {
	const secret = "token-super-secret"
	runner := newFakeClaudeRunner()
	runner.output = func(index int, opts runnerOptions) {
		_, _ = fmt.Fprintf(opts.Stdout, "stdout-%d %s\n", index+1, secret)
		_, _ = fmt.Fprintf(opts.Stderr, "stderr-%d %s\n", index+1, secret)
	}
	c, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, []string{secret})

	writeViaHTTP(t, srv, "one", http.StatusOK)
	first, cursor := eventuallyRead(t, srv, 0, func(payload string) bool {
		return strings.Contains(payload, "stderr-1")
	})
	if strings.Contains(first, secret) {
		t.Fatalf("read leaked credential literal: %q", first)
	}
	if strings.Count(first, redactedLiteral) != 2 {
		t.Fatalf("redacted output = %q, want two replacements", first)
	}
	if delta, next := readViaHTTP(t, srv, cursor); delta != "" || next != cursor {
		t.Fatalf("read at cursor = (%q,%d), want empty,%d", delta, next, cursor)
	}

	writeViaHTTP(t, srv, "two", http.StatusOK)
	delta, next := eventuallyRead(t, srv, cursor, func(payload string) bool {
		return strings.Contains(payload, "stderr-2")
	})
	if strings.Contains(delta, "stdout-1") || strings.Contains(delta, secret) {
		t.Fatalf("delta contains old output or secret: %q", delta)
	}
	full, fullNext := readViaHTTP(t, srv, 0)
	if i, j := strings.Index(full, "stdout-1"), strings.Index(full, "stdout-2"); i < 0 || j < 0 || i > j {
		t.Fatalf("full output is not in invocation order: %q", full)
	}
	if fullNext != next {
		t.Fatalf("full cursor=%d, delta cursor=%d", fullNext, next)
	}
	waitClaudeIdle(t, c)
}

func TestClaudeHealthAndAttachEndpoints(t *testing.T) {
	runner := newFakeClaudeRunner()
	_, srv := newClaudeTestServer(t, runner, platformDefaultModel, false, nil)

	if code := healthzCode(t, srv); code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", code)
	}
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/attach"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach = %d, want 101", resp.StatusCode)
	}
	_ = conn.Close()
}

func TestClaudeRestoreModeRejectsIOUntilRestore(t *testing.T) {
	runner := newFakeClaudeRunner()
	_, srv := newClaudeTestServer(t, runner, platformDefaultModel, true, nil)

	if code := healthzCode(t, srv); code != http.StatusOK {
		t.Fatalf("restore-mode healthz = %d, want 200", code)
	}
	writeViaHTTP(t, srv, "not yet", http.StatusServiceUnavailable)
	resp, err := http.Get(srv.URL + "/read?offset=0")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("read before restore = %d, want 503", resp.StatusCode)
	}
}

func TestClaudeCloseIsIdempotent(t *testing.T) {
	c, err := newClaudeWorkload(claudeConfig{
		StateDir: filepath.Join(t.TempDir(), "state"),
		Runner:   newFakeClaudeRunner(),
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			c.Close()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Close did not return")
		}
	}
}
