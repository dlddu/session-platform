package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// claude-code's provider auth modes (docs/prd/claude-code-workload.md, AC-E7).
// gateway is AC-E6's loopback credential proxy; subscription signs the CLI in
// to a Claude subscription whose login lives in this session's auth slot
// (docs/prd/agent-auth-pool.md).
const (
	// Keep in sync with control-plane/internal/adapter/k8s (the claude-code
	// auth mode and effort env).
	claudeAuthModeEnv          = "CLAUDE_CODE_AUTH_MODE"
	claudeAuthModeGateway      = "gateway"
	claudeAuthModeSubscription = "subscription"
	claudeEffortEnv            = "CLAUDE_CODE_EFFORT"

	// claudeCredentialsFile is the slot's login file and its HOME place.
	claudeCredentialsFile    = ".credentials.json"
	claudeCredentialsHomeRel = claudeSettingsDir + "/" + claudeCredentialsFile

	// claudeLoginCodeInput is what the turn's input record shows instead of a
	// pasted sign-in code: the code is a credential and never reaches output.
	claudeLoginCodeInput = "[login code]"

	claudeLoginPendingTimeout = 15 * time.Minute
	claudeLoginSubmitTimeout  = 2 * time.Minute
	claudeLoginURLTimeout     = time.Minute
	claudeLoginStatusTimeout  = 30 * time.Second
)

// claudeEfforts is AC-E8's list for claude-code: the values the CLI's
// --effort accepts.
var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// claudeLoginURLPattern finds the authorize URL in `claude auth login`'s
// output ("If the browser didn't open, visit: https://…").
var claudeLoginURLPattern = regexp.MustCompile(`https://[^\s"'<>]+`)

// claudeAuthModeFromEnv reads AC-E7's mode. The control plane sets it only in
// subscription mode; an absent value is gateway, exactly as before the mode
// existed, and anything else is a configuration error.
func claudeAuthModeFromEnv() (string, error) {
	switch mode := os.Getenv(claudeAuthModeEnv); mode {
	case "", claudeAuthModeGateway:
		return claudeAuthModeGateway, nil
	case claudeAuthModeSubscription:
		return claudeAuthModeSubscription, nil
	default:
		return "", fmt.Errorf("%s must be %q or %q, got %q", claudeAuthModeEnv, claudeAuthModeGateway, claudeAuthModeSubscription, mode)
	}
}

// validateClaudeSubscriptionClientEnv is subscription mode's placement rule:
// the CLI talks to the provider itself with the slot's login, so a provider
// address or credential in the environment would replace that login with an
// account the session did not sign in with.
func validateClaudeSubscriptionClientEnv() error {
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("claude-code subscription container must not receive %s", name)
		}
	}
	return nil
}

// claudeEffortFromEnv reads AC-E8's session effort; empty means the CLI's own
// default and no flag.
func claudeEffortFromEnv() (string, error) {
	effort := os.Getenv(claudeEffortEnv)
	if effort == "" || slices.Contains(claudeEfforts, effort) {
		return effort, nil
	}
	return "", fmt.Errorf("%s must be one of %s, got %q", claudeEffortEnv, strings.Join(claudeEfforts, ", "), effort)
}

// claudeLogin is the subscription sign-in seam, so tests need no CLI.
type claudeLogin interface {
	// signedIn reports whether the CLI in this HOME has a usable login.
	signedIn(ctx context.Context, home string) bool
	// start begins `claude auth login` and returns once it has printed the
	// URL the user must open.
	start(ctx context.Context, home string) (claudeLoginProcess, error)
}

// claudeLoginProcess is one running `claude auth login` waiting for its code.
type claudeLoginProcess interface {
	url() string
	// submit hands the pasted code to the process and waits for it to exit.
	submit(ctx context.Context, code string) error
	kill()
}

// pendingClaudeLogin is a sign-in between its two writes (AC-E7): the URL has
// been shown and the prompt that asked for it is held until the code arrives.
type pendingClaudeLogin struct {
	proc   claudeLoginProcess
	prompt string
	timer  *time.Timer
}

// execClaudeLogin runs the real CLI.
type execClaudeLogin struct {
	binary string
}

func (l execClaudeLogin) signedIn(ctx context.Context, home string) bool {
	ctx, cancel := context.WithTimeout(ctx, claudeLoginStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.binary, "auth", "status", "--json")
	cmd.Dir = home
	cmd.Env = environmentWithHome(home)
	out, err := cmd.Output()
	if err != nil {
		// A non-zero exit is the CLI's own "not logged in".
		return false
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	return json.Unmarshal(out, &status) == nil && status.LoggedIn
}

func (l execClaudeLogin) start(ctx context.Context, home string) (claudeLoginProcess, error) {
	cmd := exec.Command(l.binary, "auth", "login", "--claudeai")
	cmd.Dir = home
	cmd.Env = environmentWithHome(home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude auth login: %w", err)
	}
	p := &execClaudeLoginProcess{cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	urls := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 64<<10), 64<<10)
		sent := false
		for scanner.Scan() {
			if match := claudeLoginURLPattern.FindString(scanner.Text()); match != "" && !sent {
				urls <- match
				sent = true
			}
		}
		// Keep draining so the CLI never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, output)
	}()
	go func() {
		p.waitErr = cmd.Wait()
		_ = writer.Close()
		close(p.exited)
	}()
	timer := time.NewTimer(claudeLoginURLTimeout)
	defer timer.Stop()
	select {
	case found := <-urls:
		p.loginURL = found
		return p, nil
	case <-p.exited:
		return nil, errors.New("claude auth login exited before printing a sign-in URL")
	case <-timer.C:
		p.kill()
		return nil, errors.New("claude auth login printed no sign-in URL")
	case <-ctx.Done():
		p.kill()
		return nil, ctx.Err()
	}
}

type execClaudeLoginProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	loginURL string
	exited   chan struct{}
	waitErr  error
	killOnce sync.Once
}

func (p *execClaudeLoginProcess) url() string { return p.loginURL }

func (p *execClaudeLoginProcess) submit(ctx context.Context, code string) error {
	if _, err := io.WriteString(p.stdin, code+"\n"); err != nil {
		p.kill()
		return errors.New("the sign-in process is no longer waiting for a code")
	}
	_ = p.stdin.Close()
	ctx, cancel := context.WithTimeout(ctx, claudeLoginSubmitTimeout)
	defer cancel()
	select {
	case <-p.exited:
		if p.waitErr != nil {
			return errors.New("claude auth login did not accept the code")
		}
		return nil
	case <-ctx.Done():
		p.kill()
		return errors.New("claude auth login did not finish")
	}
}

func (p *execClaudeLoginProcess) kill() {
	p.killOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
}

// subscriptionGate runs at the start of a subscription turn whose input is a
// prompt. It reports whether the prompt may run now; when the CLI is not
// signed in it starts the sign-in, holds the prompt and tells the user what
// to do, and the turn ends without running it.
func (c *claudeWorkload) subscriptionGate(prompt string) bool {
	if c.login.signedIn(c.ctx, c.homeDir) {
		return true
	}
	proc, err := c.login.start(c.ctx, c.homeDir)
	if err != nil {
		c.logger.Warn("start claude sign-in", "err", c.redactString(err.Error()))
		c.appendPlatformNotice("[session-platform: this session is not signed in to a Claude subscription, and the sign-in could not start. Send the prompt again to retry.]\n")
		return false
	}
	pending := &pendingClaudeLogin{proc: proc, prompt: prompt}
	c.mu.Lock()
	c.pendingLogin = pending
	pending.timer = time.AfterFunc(claudeLoginPendingTimeout, func() {
		c.dropPendingLogin(pending, "[session-platform: the Claude sign-in expired; the held prompt was dropped. Send it again to start over.]\n")
	})
	c.mu.Unlock()
	c.appendPlatformNotice("[session-platform: this session's auth slot is not signed in to a Claude subscription yet.]\n" +
		"[session-platform: open this link in a browser and sign in: " + proc.url() + "]\n" +
		"[session-platform: then send the code the page shows as your next message. Your prompt runs once sign-in completes.]\n")
	return false
}

// completeLogin feeds a pending sign-in the code from this turn's write and
// reports whether the held prompt may now run.
func (c *claudeWorkload) completeLogin(pending *pendingClaudeLogin, code string) bool {
	err := pending.proc.submit(c.ctx, strings.TrimSpace(code))
	if err != nil {
		c.logger.Warn("claude sign-in failed", "err", c.redactString(err.Error()))
		c.appendPlatformNotice("[session-platform: Claude sign-in did not complete (" + err.Error() + "); the held prompt was dropped. Send it again to start over.]\n")
		return false
	}
	if c.authSlot != nil {
		if err := c.authSlot.push(c.homeDir); err != nil {
			c.logger.Error("save the Claude login to the auth slot", "err", c.redactString(err.Error()))
		}
	}
	c.appendPlatformNotice("[session-platform: signed in to Claude. Running the held prompt.]\n")
	return true
}

// takePendingLogin hands the worker the pending sign-in, if any, so the next
// write is read as its code.
func (c *claudeWorkload) takePendingLoginLocked() *pendingClaudeLogin {
	pending := c.pendingLogin
	c.pendingLogin = nil
	if pending != nil && pending.timer != nil {
		pending.timer.Stop()
	}
	return pending
}

// dropPendingLogin ends a sign-in that will not be completed: it expired, the
// session is being archived, or the agent is stopping.
func (c *claudeWorkload) dropPendingLogin(pending *pendingClaudeLogin, notice string) {
	c.mu.Lock()
	if pending == nil || c.pendingLogin != pending {
		c.mu.Unlock()
		return
	}
	c.pendingLogin = nil
	if pending.timer != nil {
		pending.timer.Stop()
	}
	c.mu.Unlock()
	pending.proc.kill()
	if notice != "" {
		c.appendPlatformNotice(notice)
	}
}

func (c *claudeWorkload) currentPendingLogin() *pendingClaudeLogin {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingLogin
}
