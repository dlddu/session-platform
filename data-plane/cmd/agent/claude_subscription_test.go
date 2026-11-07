package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClaudeLogin stands in for `claude auth status` / `claude auth login`.
type fakeClaudeLogin struct {
	mu        sync.Mutex
	loggedIn  bool
	startErr  error
	submitErr error
	starts    int
	codes     []string
	procs     []*fakeClaudeLoginProcess
}

func (f *fakeClaudeLogin) signedIn(_ context.Context, home string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loggedIn {
		return true
	}
	_, err := os.Stat(filepath.Join(home, claudeCredentialsHomeRel))
	return err == nil
}

func (f *fakeClaudeLogin) start(_ context.Context, home string) (claudeLoginProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		return nil, f.startErr
	}
	p := &fakeClaudeLoginProcess{login: f, home: home}
	f.procs = append(f.procs, p)
	return p, nil
}

type fakeClaudeLoginProcess struct {
	login  *fakeClaudeLogin
	home   string
	killed bool
}

func (p *fakeClaudeLoginProcess) url() string {
	return "https://claude.com/cai/oauth/authorize?code=true&state=s"
}

func (p *fakeClaudeLoginProcess) submit(_ context.Context, code string) error {
	p.login.mu.Lock()
	p.login.codes = append(p.login.codes, code)
	err := p.login.submitErr
	p.login.mu.Unlock()
	if err != nil {
		return err
	}
	return writeLoginFile(filepath.Join(p.home, claudeCredentialsHomeRel), []byte("signed-in:"+code))
}

func (p *fakeClaudeLoginProcess) kill() {
	p.login.mu.Lock()
	p.killed = true
	p.login.mu.Unlock()
}

func waitOutput(t *testing.T, c *claudeWorkload, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := c.out.Since(0)
		if strings.Contains(string(out), want) {
			return string(out)
		}
		time.Sleep(5 * time.Millisecond)
	}
	out, _ := c.out.Since(0)
	t.Fatalf("output never contained %q:\n%s", want, out)
	return ""
}

// AC-E7's two-write sign-in: the first prompt shows the URL and is held, the
// next write is the code — never recorded — and the held prompt then runs in
// that turn, with the login saved to the slot.
func TestClaudeSubscriptionSignInTakesTwoWrites(t *testing.T) {
	slot := newTestAuthSlot(t, claudeCredentialsFile, claudeCredentialsHomeRel)
	runner := newFakeClaudeRunner()
	login := &fakeClaudeLogin{}
	c, srv := newSlotClaudeTestServer(t, runner, slot, login)

	writeViaHTTP(t, srv, "fix the bug", http.StatusOK)
	out := waitOutput(t, c, turnClose(1, "error"))
	if !strings.Contains(out, "https://claude.com/cai/oauth/authorize") || !strings.Contains(out, "next message") {
		t.Fatalf("the first turn must show the sign-in URL and what to do:\n%s", out)
	}
	if runs, _ := runner.snapshot(); len(runs) != 0 {
		t.Fatalf("the held prompt ran before sign-in: %v", runs)
	}

	writeViaHTTP(t, srv, "  SECRET-CODE-42\n", http.StatusOK)
	run := receiveClaudeRun(t, runner.started)
	if got := run.argv[len(run.argv)-1]; got != "fix the bug" {
		t.Fatalf("the code turn ran %q, want the held prompt", got)
	}
	waitClaudeIdle(t, c)
	out = waitOutput(t, c, turnClose(2, "ok"))
	if strings.Contains(out, "SECRET-CODE-42") {
		t.Fatalf("the sign-in code reached the session output:\n%s", out)
	}
	if !strings.Contains(out, turnOpen(2, claudeLoginCodeInput)) {
		t.Fatalf("the code turn must record the placeholder:\n%s", out)
	}
	if !reflect.DeepEqual(login.codes, []string{"SECRET-CODE-42"}) {
		t.Fatalf("submitted codes = %q", login.codes)
	}
	if got, _ := readOptional(t, slot.slotPath()); got != "signed-in:SECRET-CODE-42" {
		t.Fatalf("slot = %q, want the new login", got)
	}

	// Signed in now: the next prompt just runs.
	writeViaHTTP(t, srv, "next", http.StatusOK)
	run = receiveClaudeRun(t, runner.started)
	if got := run.argv[len(run.argv)-1]; got != "next" {
		t.Fatalf("ran %q", got)
	}
	waitClaudeIdle(t, c)
	if login.starts != 1 {
		t.Fatalf("sign-in started %d times, want once", login.starts)
	}
}

func TestClaudeSubscriptionFailedSignInDropsHeldPrompt(t *testing.T) {
	slot := newTestAuthSlot(t, claudeCredentialsFile, claudeCredentialsHomeRel)
	runner := newFakeClaudeRunner()
	login := &fakeClaudeLogin{submitErr: errors.New("bad code")}
	c, srv := newSlotClaudeTestServer(t, runner, slot, login)

	writeViaHTTP(t, srv, "fix the bug", http.StatusOK)
	waitOutput(t, c, turnClose(1, "error"))
	writeViaHTTP(t, srv, "wrong", http.StatusOK)
	out := waitOutput(t, c, turnClose(2, "error"))
	if !strings.Contains(out, "did not complete") {
		t.Fatalf("a failed sign-in must say so:\n%s", out)
	}
	if runs, _ := runner.snapshot(); len(runs) != 0 {
		t.Fatalf("nothing may run after a failed sign-in: %v", runs)
	}
	// The next prompt starts over.
	writeViaHTTP(t, srv, "again", http.StatusOK)
	waitOutput(t, c, turnClose(3, "error"))
	if login.starts != 2 {
		t.Fatalf("sign-in started %d times, want a fresh start", login.starts)
	}
	if _, ok := readOptional(t, slot.slotPath()); ok {
		t.Fatal("a failed sign-in must not write the slot")
	}
}

func TestClaudeSubscriptionSignInThatCannotStart(t *testing.T) {
	slot := newTestAuthSlot(t, claudeCredentialsFile, claudeCredentialsHomeRel)
	runner := newFakeClaudeRunner()
	c, srv := newSlotClaudeTestServer(t, runner, slot, &fakeClaudeLogin{startErr: errors.New("no cli")})
	writeViaHTTP(t, srv, "x", http.StatusOK)
	out := waitOutput(t, c, turnClose(1, "error"))
	if !strings.Contains(out, "could not start") {
		t.Fatalf("output:\n%s", out)
	}
	if c.currentPendingLogin() != nil {
		t.Fatal("no sign-in is pending when it could not start")
	}
}

// A sign-in cannot wait across an archive or a stop: its process is killed.
func TestClaudeSubscriptionCheckpointDropsPendingSignIn(t *testing.T) {
	slot := newTestAuthSlot(t, claudeCredentialsFile, claudeCredentialsHomeRel)
	login := &fakeClaudeLogin{}
	c, srv := newSlotClaudeTestServer(t, newFakeClaudeRunner(), slot, login)
	writeViaHTTP(t, srv, "x", http.StatusOK)
	waitOutput(t, c, turnClose(1, "error"))
	if c.currentPendingLogin() == nil {
		t.Fatal("a sign-in should be pending")
	}
	resp, err := http.Post(srv.URL+"/checkpoint", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("checkpoint = %d", resp.StatusCode)
	}
	if c.currentPendingLogin() != nil || !login.procs[0].killed {
		t.Fatal("the checkpoint must end the pending sign-in")
	}
	waitOutput(t, c, "archived during the Claude sign-in")
}

func TestClaudeSubscriptionNeedsSlot(t *testing.T) {
	_, err := newClaudeWorkload(claudeConfig{
		StateDir: filepath.Join(t.TempDir(), "state"),
		Logger:   testLogger(),
		Login:    &fakeClaudeLogin{},
	})
	if err == nil {
		t.Fatal("a sign-in without a slot must be refused")
	}
}

func TestClaudeArgvCarriesEffort(t *testing.T) {
	got := (claudeCLI{effort: "max"}).argv("claude", "opus", "p", true)
	want := []string{
		"claude", "--continue", "--model", "opus", "--effort", "max",
		"--permission-mode", "auto", "-p", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--", "p",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if got := (claudeCLI{}).argv("claude", platformDefaultModel, "p", false); slicesContain(got, "--effort") {
		t.Fatalf("no effort means no flag: %q", got)
	}
}

func setClaudeGatewayEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", claudeProxyBaseURL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", claudeProxyPlaceholderToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
}

// AC-E7/E8's start rules.
func TestClaudeAuthFromEnv(t *testing.T) {
	t.Setenv(claudeEffortEnv, "")
	t.Setenv(authSlotDirEnv, "")

	setClaudeGatewayEnv(t)
	t.Setenv(claudeAuthModeEnv, "")
	auth, err := claudeAuthFromEnv(workloadClaudeCode, testLogger())
	if err != nil || auth.mode != claudeAuthModeGateway || auth.slot != nil || !auth.tools.Plugin {
		t.Fatalf("gateway = %+v, %v", auth, err)
	}

	t.Setenv(claudeAuthModeEnv, "sso")
	if _, err := claudeAuthFromEnv(workloadClaudeCode, testLogger()); err == nil {
		t.Fatal("an unknown mode must be refused")
	}

	t.Setenv(claudeAuthModeEnv, claudeAuthModeSubscription)
	if _, err := claudeAuthFromEnv(workloadClaudeCode, testLogger()); err == nil {
		t.Fatal("subscription must refuse the gateway's provider env")
	}
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(name, "")
	}
	if _, err := claudeAuthFromEnv(workloadClaudeCode, testLogger()); err == nil {
		t.Fatal("subscription needs an auth slot")
	}
	t.Setenv(authSlotDirEnv, t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat")
	if _, err := claudeAuthFromEnv(workloadClaudeCode, testLogger()); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("err = %v, want the injected token named", err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv(claudeEffortEnv, "xhigh")
	auth, err = claudeAuthFromEnv(workloadClaudeCode, testLogger())
	if err != nil || auth.mode != claudeAuthModeSubscription || auth.slot == nil || auth.effort != "xhigh" || !auth.tools.Plugin {
		t.Fatalf("subscription = %+v, %v", auth, err)
	}
	if auth.slot.archiveExclusion() != "home/.claude/.credentials.json" {
		t.Fatalf("exclusion = %q", auth.slot.archiveExclusion())
	}
	t.Setenv(claudeEffortEnv, "minimal")
	if _, err := claudeAuthFromEnv(workloadClaudeCode, testLogger()); err == nil {
		t.Fatal("minimal is not a claude-code effort")
	}
}
