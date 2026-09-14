package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// clonedMCP is the gated MCP with the two things these cases need: a volume to
// clone onto, and a counted clone in place of the real git. The counter is the
// assertion that matters most, for the same reason the upstream counter is in
// the fetch cases: "git did not run" is what an approval gate means here.
type clonedMCP struct {
	server    *httptest.Server
	gateway   *fakeGateway
	sharedDir string
	clones    atomic.Int32
	calls     []cloneCall
	cloneErr  error
}

type cloneCall struct {
	target string
	branch string
	dest   string
}

func newClonedMCP(t *testing.T, sharedDir string, statuses ...string) *clonedMCP {
	t.Helper()
	c := &clonedMCP{gateway: newFakeGateway(t, statuses...), sharedDir: sharedDir}
	gate := c.gateway.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }
	c.server = newSessionMCPServerWith(t, sessionMCPConfig{
		gateway:   gate,
		sharedDir: sharedDir,
		clone: func(_ context.Context, target, branch, dest string) error {
			c.clones.Add(1)
			c.calls = append(c.calls, cloneCall{target: target, branch: branch, dest: dest})
			if c.cloneErr != nil {
				return c.cloneErr
			}
			// A real clone leaves a directory behind, so the fake does too —
			// otherwise the cases that assert nothing is left cannot fail.
			return os.MkdirAll(filepath.Join(dest, ".git"), 0o700)
		},
	})
	return c
}

func (c *clonedMCP) call(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 21, "method": "tools/call",
		"params": map[string]any{"name": gitCloneTool, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, decoded := postMCP(t, c.server, string(body))
	if status != http.StatusOK {
		t.Fatalf("tools/call = %d, want 200", status)
	}
	return decoded
}

func (c *clonedMCP) payloadOf(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	_, isError, text := toolResult(t, c.call(t, args))
	if isError {
		t.Fatalf("approved clone returned a tool error: %s", text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool result text is not the JSON payload: %v", err)
	}
	return payload
}

// The roadmap's first entry, end to end minus the process: the clone happens
// once, and only after the decision.
func TestSessionMCPClonesOnlyAfterApproval(t *testing.T) {
	c := newClonedMCP(t, t.TempDir(), "PENDING", "PENDING", "APPROVED")
	payload := c.payloadOf(t, map[string]any{"url": "https://github.com/dlddu/pure-agent.git"})

	if got := c.clones.Load(); got != 1 {
		t.Fatalf("git ran %d times, want exactly 1", got)
	}
	if payload["success"] != true {
		t.Fatalf("payload = %v, want a successful clone result", payload)
	}
	if payload["url"] != "https://github.com/dlddu/pure-agent.git" {
		t.Errorf("payload url = %v, want the approved URL", payload["url"])
	}
	if payload["branch"] != noBranch {
		t.Errorf("payload branch = %v, want %q when none was asked for", payload["branch"], noBranch)
	}
	if c.calls[0].branch != noBranch {
		t.Errorf("clone branch = %q, want the default marker", c.calls[0].branch)
	}
}

// Every refusal path reaches the model as a tool failure and leaves git unrun —
// the clone equivalent of the fetch cases' "never reach the network".
func TestSessionMCPRefusedClonesNeverRunGit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		want     string
	}{
		{"rejected", []string{"REJECTED"}, "rejected"},
		{"expired", []string{"EXPIRED"}, "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClonedMCP(t, t.TempDir(), tc.statuses...)
			_, isError, text := toolResult(t, c.call(t, map[string]any{"url": "https://github.com/dlddu/pure-agent.git"}))
			if !isError {
				t.Fatalf("%s decision produced a success result: %s", tc.name, text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("failure text = %q, want it to name the decision %q", text, tc.want)
			}
			if got := c.clones.Load(); got != 0 {
				t.Fatalf("git ran %d times after a %s decision, want 0", got, tc.name)
			}
		})
	}
}

// A gateway that cannot be asked is not a decision, and it still must not clone.
func TestSessionMCPCloneGatewayOutageIsAToolFailure(t *testing.T) {
	c := newClonedMCP(t, t.TempDir(), "PENDING")
	c.gateway.createStatus = http.StatusServiceUnavailable
	_, isError, text := toolResult(t, c.call(t, map[string]any{"url": "https://github.com/dlddu/pure-agent.git"}))
	if !isError {
		t.Fatalf("gateway outage produced a success result: %s", text)
	}
	if got := c.clones.Load(); got != 0 {
		t.Fatalf("git ran %d times without an approval, want 0", got)
	}
}

// Arguments a human could not meaningfully approve — and a credential smuggled
// into the URL, which R8 keeps out of the context a human reads — are refused
// before any request is created.
func TestSessionMCPCloneRejectsUnapprovableTargets(t *testing.T) {
	for _, tc := range []struct{ name, url, branch string }{
		{"empty", "", ""},
		{"ssh shorthand", "git@github.com:dlddu/pure-agent.git", ""},
		{"ssh scheme", "ssh://git@github.com/dlddu/pure-agent.git", ""},
		{"file scheme", "file:///etc/passwd", ""},
		{"no host", "https://", ""},
		{"leading dash", "--upload-pack=touch /tmp/x", ""},
		{"control character", "https://github.com/a\nb.git", ""},
		{"credentials in url", "https://user:token@github.com/dlddu/private.git", ""},
		{"branch with a leading dash", "https://github.com/dlddu/pure-agent.git", "--config=core.pager=sh"},
		{"branch with a control character", "https://github.com/dlddu/pure-agent.git", "main\nx"},
		{"overlong branch", "https://github.com/dlddu/pure-agent.git", strings.Repeat("b", maxCloneBranchChars+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClonedMCP(t, t.TempDir(), "APPROVED")
			args := map[string]any{"url": tc.url}
			if tc.branch != "" {
				args["branch"] = tc.branch
			}
			_, isError, text := toolResult(t, c.call(t, args))
			if !isError {
				t.Fatalf("%q/%q was accepted: %s", tc.url, tc.branch, text)
			}
			if got := c.gateway.created.Load(); got != 0 {
				t.Errorf("created %d approval requests for an invalid argument, want 0", got)
			}
			if got := c.clones.Load(); got != 0 {
				t.Errorf("git ran %d times for an invalid argument, want 0", got)
			}
		})
	}
}

// Why it refuses before asking a human: docs/session-mcp-tool-surface.md.
func TestSessionMCPCloneNeedsASharedVolume(t *testing.T) {
	c := newClonedMCP(t, "", "APPROVED")
	_, isError, text := toolResult(t, c.call(t, map[string]any{"url": "https://github.com/dlddu/pure-agent.git"}))
	if !isError {
		t.Fatalf("a clone was accepted with no volume to clone into: %s", text)
	}
	if !strings.Contains(text, "shared volume") {
		t.Errorf("failure text = %q, want it to name the missing volume", text)
	}
	if got := c.gateway.created.Load(); got != 0 {
		t.Errorf("created %d approval requests with no volume configured, want 0", got)
	}
	if got := c.clones.Load(); got != 0 {
		t.Errorf("git ran %d times with no volume configured, want 0", got)
	}
}

// The destination is the second test of the convention web_fetch_get set: every
// segment is this server's, so the caller cannot steer one.
func TestSessionMCPCloneNamesItsOwnDestination(t *testing.T) {
	dir := t.TempDir()
	c := newClonedMCP(t, dir, "APPROVED")
	payload := c.payloadOf(t, map[string]any{"url": "https://github.com/dlddu/pure-agent.git", "branch": "main"})

	path, ok := payload["path"].(string)
	if !ok {
		t.Fatalf("payload = %v, want the clone path", payload)
	}
	requestID, _ := c.gateway.lastBody["externalId"].(string)
	requestID = strings.TrimPrefix(requestID, "sess-abc:")
	if want := filepath.Join(dir, cloneSubdir, requestID); path != want {
		t.Fatalf("clone path = %q, want %q", path, want)
	}
	if c.calls[0].dest != path {
		t.Errorf("git cloned into %q but the result names %q", c.calls[0].dest, path)
	}
	if payload["branch"] != "main" || c.calls[0].branch != "main" {
		t.Errorf("branch = %v / %q, want the requested main", payload["branch"], c.calls[0].branch)
	}
	// The approval a human reads carries the destination, which is the half of
	// R4's context this tool adds to a URL.
	approvalContext, _ := c.gateway.lastBody["context"].(string)
	if !strings.Contains(approvalContext, path) {
		t.Errorf("approval context = %q, want it to carry the destination path", approvalContext)
	}
	if !strings.Contains(approvalContext, "https://github.com/dlddu/pure-agent.git") {
		t.Errorf("approval context = %q, want the full URL", approvalContext)
	}
}

// Two calls in one session must not be able to land in the same directory: the
// id that names the destination is the one AC-F3 already requires to be unique.
func TestSessionMCPCloneGivesEachCallItsOwnDestination(t *testing.T) {
	c := newClonedMCP(t, t.TempDir(), "APPROVED")
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		payload := c.payloadOf(t, map[string]any{"url": "https://github.com/dlddu/pure-agent.git"})
		path, _ := payload["path"].(string)
		if seen[path] {
			t.Fatalf("clone path %q repeated between two calls of one session", path)
		}
		seen[path] = true
	}
}

// A clone that failed halfway would otherwise leave the agent a path the tool
// never named and a repository git did not finish.
func TestSessionMCPFailedCloneLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	c := newClonedMCP(t, dir, "APPROVED")
	c.cloneErr = errors.New("fatal: repository not found")

	result, isError, text := toolResult(t, c.call(t, map[string]any{"url": "https://github.com/dlddu/missing.git"}))
	if !isError {
		t.Fatalf("a failed clone produced a success result: %s", text)
	}
	if !strings.Contains(text, "repository not found") {
		t.Errorf("failure text = %q, want git's own reason", text)
	}
	if _, ok := result["path"]; ok {
		t.Errorf("result names %v for a clone that failed", result["path"])
	}
	entries, err := os.ReadDir(filepath.Join(dir, cloneSubdir))
	if err != nil {
		t.Fatalf("read the clone directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("clone directory holds %v after a failure, want nothing", entries)
	}
}

// The command line, asserted directly: the emptied credential helper (see
// session_mcp_git.go's cloneArgs), and `--`, which stops git from reading a
// repository URL as an option even if the validation above ever gains a gap.
func TestCloneArgsRefuseCredentialsAndStopFlagParsing(t *testing.T) {
	withDefault := cloneArgs("https://github.com/dlddu/pure-agent.git", noBranch, "/shared/git_clone/req-1")
	want := []string{"-c", "credential.helper=", "clone", "--", "https://github.com/dlddu/pure-agent.git", "/shared/git_clone/req-1"}
	if strings.Join(withDefault, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", withDefault, want)
	}
	withBranch := cloneArgs("https://github.com/dlddu/pure-agent.git", "release", "/shared/git_clone/req-2")
	want = []string{"-c", "credential.helper=", "clone", "--branch", "release", "--", "https://github.com/dlddu/pure-agent.git", "/shared/git_clone/req-2"}
	if strings.Join(withBranch, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", withBranch, want)
	}
}

// A remote controls this text, so it is capped rather than trusted — and the
// cap says how much it dropped instead of trimming silently.
func TestBoundedBufferCapsWhatARemoteCanSay(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	if _, err := b.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("67890")); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.HasPrefix(got, "12345678") {
		t.Fatalf("buffer = %q, want the first 8 bytes", got)
	}
	if !strings.Contains(got, "2 more bytes") {
		t.Errorf("buffer = %q, want it to report the dropped bytes", got)
	}
}
