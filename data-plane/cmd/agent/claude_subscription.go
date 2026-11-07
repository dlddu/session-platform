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
	// claudeLoginDroppedWindow is how long a code sent for an ended sign-in is
	// still recognised as one rather than run as a prompt.
	claudeLoginDroppedWindow = 30 * time.Minute
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
	// signedIn reports whether the CLI in this HOME has a usable login. An
	// error means the check itself did not run to an answer — not "signed
	// out", which would start a sign-in nobody needs.
	signedIn(ctx context.Context, home string) (bool, error)
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
	// after is the arrival count when the URL was shown: only writes that
	// came after it can be the code the user copied from that page.
	after uint64
}

// droppedClaudeLogin is a sign-in that ended — it expired or the session was
// archived — while its code may still be on its way. The first write after
// its URL is still read as that code, so the code never reaches the record
// or the model.
type droppedClaudeLogin struct {
	after uint64
	until time.Time
}

// turnInput is what the worker decided a dequeued write is (AC-E7).
type turnInput struct {
	// code is the pending sign-in this write completes.
	code *pendingClaudeLogin
	// staleCode is a write for a sign-in that already ended.
	staleCode bool
	// beforeSignIn is a prompt queued before the sign-in's URL was shown.
	beforeSignIn bool
}

// execClaudeLogin runs the real CLI.
type execClaudeLogin struct {
	binary string
}

func (l execClaudeLogin) signedIn(ctx context.Context, home string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, claudeLoginStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.binary, "auth", "status", "--json")
	cmd.Dir = home
	cmd.Env = environmentWithHome(home)
	out, err := cmd.Output()
	return parseClaudeAuthStatus(out, err, ctx.Err())
}

// parseClaudeAuthStatus separates the CLI's answer from a check that never
// produced one. A non-zero exit is the CLI's own "not logged in"; a status
// that could not run, timed out, or printed no readable answer is an error.
func parseClaudeAuthStatus(out []byte, runErr, ctxErr error) (bool, error) {
	if ctxErr != nil {
		return false, fmt.Errorf("claude auth status: %w", ctxErr)
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return false, fmt.Errorf("run claude auth status: %w", runErr)
	}
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(out, &status) == nil && status.LoggedIn != nil {
		return *status.LoggedIn, nil
	}
	if runErr != nil {
		return false, nil
	}
	return false, errors.New("claude auth status printed no loggedIn answer")
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
	signedIn, err := c.login.signedIn(c.ctx, c.homeDir)
	if err != nil {
		c.logger.Warn("check claude sign-in", "err", c.redactString(err.Error()))
		c.appendPlatformNotice("[session-platform: could not check the Claude sign-in; the prompt did not run. Try again.]\n")
		return false
	}
	if signedIn {
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
	pending.after = c.enqueued
	c.pendingLogin = pending
	c.droppedLogin = nil
	pending.timer = time.AfterFunc(claudeLoginPendingTimeout, func() {
		c.dropPendingLogin(pending, "[session-platform: the Claude sign-in expired; the held prompt was dropped. A code sent now is discarded — send the prompt again to start over.]\n")
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

// classifyInputLocked decides what a dequeued write is (AC-E7). With a
// sign-in pending, a write that arrived after its URL was shown is the code
// and takes the pending sign-in; one queued before is a prompt that cannot
// run yet. With a sign-in that just ended, the first write after its URL is
// still its code. Everything else is a prompt.
func (c *claudeWorkload) classifyInputLocked(job promptJob) turnInput {
	if pending := c.pendingLogin; pending != nil {
		if job.seq <= pending.after {
			return turnInput{beforeSignIn: true}
		}
		c.pendingLogin = nil
		if pending.timer != nil {
			pending.timer.Stop()
		}
		return turnInput{code: pending}
	}
	if dropped := c.droppedLogin; dropped != nil {
		if c.now().After(dropped.until) {
			c.droppedLogin = nil
		} else if job.seq > dropped.after {
			c.droppedLogin = nil
			return turnInput{staleCode: true}
		}
	}
	return turnInput{}
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
	if !c.stopping {
		c.droppedLogin = &droppedClaudeLogin{after: pending.after, until: c.now().Add(claudeLoginDroppedWindow)}
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
