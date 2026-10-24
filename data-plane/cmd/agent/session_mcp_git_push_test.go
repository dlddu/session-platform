package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testPushedCommit = "0123456789abcdef0123456789abcdef01234567"

// fakeClone lays out what git_clone leaves on the shared volume: a request-id
// directory with a .git/config naming the origin. The push seam is faked, so
// nothing else in it is read.
func (m *repoAccessMCP) fakeClone(t *testing.T, origin string) string {
	t.Helper()
	dir := filepath.Join(m.sharedDir, cloneSubdir, "req-0a1b2c3d")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("[core]\n\tbare = false\n[remote \"origin\"]\n\turl = %s\n", origin)
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (m *repoAccessMCP) assertNothingLeft(t *testing.T, label string) {
	t.Helper()
	if len(m.pushes) != 0 || m.proxy.calls.Load() != 0 || m.gateway.created.Load() != 0 {
		t.Errorf("%s: pushes = %d, tokens = %d, approvals = %d; want all zero", label,
			len(m.pushes), m.proxy.calls.Load(), m.gateway.created.Load())
	}
}

// AC-H6 end to end inside the helper pod: the clone's origin, the allow list,
// then a write token for that one repository, carried only as a URL-scoped
// header to the push. No approval is asked.
func TestGitPushPushesAnAllowedCloneWithAScopedWriteToken(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "REJECTED")
	m.cp.allowed = true
	const origin = "https://github.com/dlddu/reconciler.git"
	dir := m.fakeClone(t, origin)

	isError, text := m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"})
	if isError {
		t.Fatalf("push failed: %s", text)
	}
	if got := m.gateway.created.Load(); got != 0 {
		t.Errorf("approval requests = %d, want none where the allow list is the gate", got)
	}
	m.cp.mu.Lock()
	checked := append([]string(nil), m.cp.checkedRepos...)
	m.cp.mu.Unlock()
	if len(checked) != 1 || checked[0] != "dlddu/reconciler" {
		t.Errorf("checked %v, want dlddu/reconciler", checked)
	}
	if got := m.proxy.calls.Load(); got != 1 {
		t.Fatalf("proxy got %d token requests, want 1", got)
	}
	m.proxy.mu.Lock()
	body := m.proxy.bodies[0]
	m.proxy.mu.Unlock()
	args, _ := body["params"].(map[string]any)["arguments"].(map[string]any)
	if repos, _ := args["repositories"].([]any); len(repos) != 1 || repos[0] != "reconciler" {
		t.Errorf("repositories = %v, want only this repository", args["repositories"])
	}
	if perms, _ := args["permissions"].(map[string]any); len(perms) != 1 || perms["contents"] != "write" {
		t.Errorf("permissions = %v, want contents: write only", args["permissions"])
	}

	if len(m.pushes) != 1 {
		t.Fatalf("pushed %d times, want 1", len(m.pushes))
	}
	req := m.pushes[0]
	if req.source != dir || req.branch != "main" || req.remote != origin {
		t.Errorf("push request = %+v, want %s main → %s", req, dir, origin)
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http." + origin + ".extraheader",
		"GIT_CONFIG_VALUE_0": "Authorization: Basic " + basicAuthFor(testGitHubToken),
	} {
		if got, _ := envEntry(req.env, key); got != want {
			t.Errorf("push env %s = %q, want %q", key, got, want)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("result %q is not JSON: %v", text, err)
	}
	if payload["success"] != true || payload["url"] != origin || payload["branch"] != "main" ||
		payload["commit"] != testPushedCommit || payload["path"] != dir {
		t.Errorf("payload = %v", payload)
	}
	m.assertNoSecret(t, text)
}

// R3: an argument that cannot name a clone and a branch reaches neither the
// allow list nor the token mint.
func TestGitPushValidatesBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   func(m *repoAccessMCP) string
		branch string
	}{
		{"no branch", nil, ""},
		{"option-like branch", nil, "-f"},
		{"force refspec", nil, "+main"},
		{"refspec", nil, "main:main"},
		{"delete refspec", nil, ":main"},
		{"dotdot branch", nil, "a..b"},
		{"lock branch", nil, "main.lock"},
		{"trailing slash", nil, "feature/"},
		{"reflog syntax", nil, "main@{1}"},
		{"space", nil, "ma in"},
		{"newline", nil, "main\nx"},
		{"no path", func(*repoAccessMCP) string { return "" }, "main"},
		{"relative path", func(*repoAccessMCP) string { return "git_clone/req-0a1b2c3d" }, "main"},
		{"outside the clone dir", func(m *repoAccessMCP) string { return m.sharedDir }, "main"},
		{"traversal", func(m *repoAccessMCP) string {
			return filepath.Join(m.sharedDir, cloneSubdir, "req-0a1b2c3d") + "/../req-0a1b2c3d"
		}, "main"},
		{"not a request id", func(m *repoAccessMCP) string { return filepath.Join(m.sharedDir, cloneSubdir, "mine") }, "main"},
		{"below a clone", func(m *repoAccessMCP) string {
			return filepath.Join(m.sharedDir, cloneSubdir, "req-0a1b2c3d", "sub")
		}, "main"},
		{"no such clone", func(m *repoAccessMCP) string { return filepath.Join(m.sharedDir, cloneSubdir, "req-ffffffff") }, "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
			m.cp.allowed = true
			dir := m.fakeClone(t, "https://github.com/dlddu/reconciler")
			path := dir
			if tc.path != nil {
				path = tc.path(m)
			}
			if isError, text := m.call(t, gitPushTool, map[string]any{"path": path, "branch": tc.branch}); !isError {
				t.Fatalf("path %q branch %q was accepted: %s", path, tc.branch, text)
			}
			if got := m.cp.checks.Load(); got != 0 {
				t.Errorf("allow list asked %d times, want 0", got)
			}
			m.assertNothingLeft(t, tc.name)
		})
	}
}

// The workload pod writes the shared volume, so a clone directory (or its
// .git) replaced by a symlink could point anywhere in the helper container.
func TestGitPushRefusesSymlinkedClones(t *testing.T) {
	for _, which := range []string{"clone", ".git"} {
		m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
		m.cp.allowed = true
		elsewhere := t.TempDir()
		if err := os.MkdirAll(filepath.Join(elsewhere, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(m.sharedDir, cloneSubdir, "req-0a1b2c3d")
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if which == "clone" {
			if err := os.Symlink(elsewhere, dir); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(elsewhere, ".git"), filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
		}
		if isError, text := m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"}); !isError {
			t.Fatalf("%s symlink: push accepted: %s", which, text)
		}
		m.assertNothingLeft(t, which)
	}
}

// The origin the agent left in its clone decides the repository, and only a
// github.com repository URL can be one.
func TestGitPushRefusesANonGitHubOrigin(t *testing.T) {
	for _, origin := range []string{"https://gitlab.com/dlddu/reconciler.git", "http://github.com/dlddu/reconciler",
		"https://x-access-token:abc@github.com/dlddu/reconciler", "/srv/repo.git", "https://github.com/dlddu"} {
		m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
		m.cp.allowed = true
		dir := m.fakeClone(t, origin)
		isError, text := m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"})
		if !isError || !strings.Contains(text, "github.com") {
			t.Fatalf("%s: result = %v %q, want the github.com-only refusal", origin, isError, text)
		}
		if m.cp.checks.Load() != 0 {
			t.Errorf("%s: the allow list was asked", origin)
		}
		m.assertNothingLeft(t, origin)
	}
}

// Off the list nothing is pushed and the result points at the request tool;
// a list that cannot be asked pushes nothing either.
func TestGitPushIsClosedOffTheAllowList(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	dir := m.fakeClone(t, "https://github.com/dlddu/reconciler")
	isError, text := m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"})
	if !isError || !strings.Contains(text, "not on the allow list") || !strings.Contains(text, requestRepoAccessTool) {
		t.Fatalf("result = %v %q, want the allow-list refusal", isError, text)
	}
	m.assertNothingLeft(t, "off the list")

	m = newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	m.cp.statusCode = http.StatusServiceUnavailable
	dir = m.fakeClone(t, "https://github.com/dlddu/reconciler")
	isError, text = m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"})
	if !isError || !strings.Contains(text, "allow list could not be checked") {
		t.Fatalf("result = %v %q, want the check failure", isError, text)
	}
	m.assertNothingLeft(t, "check failed")
}

// Unlike git_clone there is no anonymous fallback: without a write token
// nothing is attempted.
func TestGitPushNeedsAWriteToken(t *testing.T) {
	for _, opts := range []repoAccessOptions{{}, {noProxy: true}} {
		m := newRepoAccessMCP(t, opts, "APPROVED")
		m.cp.allowed = true
		m.proxy.response = `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"no installation"}]}}`
		dir := m.fakeClone(t, "https://github.com/dlddu/reconciler")
		isError, text := m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"})
		if !isError || !strings.Contains(text, "write token could not be issued") {
			t.Fatalf("noProxy=%v: result = %v %q, want the token failure", opts.noProxy, isError, text)
		}
		if len(m.pushes) != 0 {
			t.Errorf("noProxy=%v: pushed without a token", opts.noProxy)
		}
	}
}

// A rejected push tells the agent how to recover; any failure is scrubbed of
// the credential.
func TestGitPushFailuresAreRedactedAndNamed(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	m.cp.allowed = true
	dir := m.fakeClone(t, "https://github.com/dlddu/reconciler")
	m.pushErr = func(req pushRequest) error {
		value, _ := envEntry(req.env, "GIT_CONFIG_VALUE_0")
		return fmt.Errorf("%w: ! [rejected] main -> main (fetch first) sent %s token %s", errPushRejected, value, testGitHubToken)
	}
	isError, text := m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"})
	if !isError || !strings.Contains(text, "was rejected") || !strings.Contains(text, gitCloneTool) {
		t.Fatalf("result = %v %q, want the rejection with the recovery hint", isError, text)
	}
	if !strings.Contains(text, redactedLiteral) {
		t.Errorf("failure text = %q, want the secret redacted", text)
	}
	m.assertNoSecret(t, text)

	m.pushErr = func(pushRequest) error { return errors.New("exit status 128: remote: Permission denied") }
	isError, text = m.call(t, gitPushTool, map[string]any{"path": dir, "branch": "main"})
	if !isError || !strings.Contains(text, "failed") || !strings.Contains(text, "Permission denied") {
		t.Fatalf("result = %v %q, want the generic failure", isError, text)
	}
}

// The real git, against a local smart-HTTP remote that accepts pushes: the
// branch arrives, the header is on every request, nothing the agent planted in
// the clone runs, and a non-fast-forward is refused rather than forced.
func TestPushRepositoryPushesThroughAScratchRepository(t *testing.T) {
	backend := filepath.Join("/usr/lib/git-core", "git-http-backend")
	if out, err := exec.Command("git", "--exec-path").Output(); err == nil {
		backend = filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	}
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not installed")
	}
	gitRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(dir, msg string) string {
		gitRun("-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", msg)
		return gitRun("-C", dir, "rev-parse", "HEAD")
	}

	root := t.TempDir()
	seed := filepath.Join(t.TempDir(), "seed")
	gitRun("init", "-q", "-b", "main", seed)
	commit(seed, "init")
	remoteDir := filepath.Join(root, "reconciler.git")
	gitRun("clone", "-q", "--bare", seed, remoteDir)
	gitRun("-C", remoteDir, "config", "http.receivepack", "true")

	var mu sync.Mutex
	var seen []string
	cgiHandler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		cgiHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	remote := srv.URL + "/reconciler.git"

	// The agent's clone, with traps that would run if git acted on its config.
	// They go in after the test's own commits, which would set them off.
	clone := filepath.Join(t.TempDir(), "req-0a1b2c3d")
	gitRun("clone", "-q", remoteDir, clone)
	want := commit(clone, "change")
	marker := filepath.Join(t.TempDir(), "ran")
	plantTraps := func() {
		t.Helper()
		hook := "#!/bin/sh\necho ran >> " + marker + "\n"
		for _, name := range []string{"pre-push", "reference-transaction", "post-checkout", "pre-receive"} {
			if err := os.WriteFile(filepath.Join(clone, ".git", "hooks", name), []byte(hook), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		gitRun("-C", clone, "config", "core.fsmonitor", "sh -c 'echo ran >> "+marker+"'")
		gitRun("-C", clone, "config", "core.sshCommand", "sh -c 'echo ran >> "+marker+"'")
		gitRun("-C", clone, "config", "uploadpack.packObjectsHook", "sh -c 'echo ran >> "+marker+"'")
	}
	disarm := func() {
		t.Helper()
		for _, name := range []string{"pre-push", "reference-transaction", "post-checkout", "pre-receive"} {
			_ = os.Remove(filepath.Join(clone, ".git", "hooks", name))
		}
		for _, key := range []string{"core.fsmonitor", "core.sshCommand", "uploadpack.packObjectsHook"} {
			gitRun("-C", clone, "config", "--unset", key)
		}
	}
	plantTraps()

	header, encoded := githubBasicAuthHeader(testGitHubToken)
	outcome, err := pushRepository(context.Background(), pushRequest{
		source: clone, branch: "main", remote: remote, env: scopedAuthEnv(remote, header),
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if outcome.commit != want {
		t.Errorf("pushed commit = %s, want %s", outcome.commit, want)
	}
	if got := gitRun("-C", remoteDir, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("remote main = %s, want %s", got, want)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("something the agent planted in the clone ran")
	}
	mu.Lock()
	if len(seen) == 0 {
		t.Error("the remote saw no request")
	}
	for _, got := range seen {
		if got != "Basic "+encoded {
			t.Errorf("remote saw Authorization %q, want the scoped header on every request", got)
		}
	}
	mu.Unlock()
	cfg, _ := os.ReadFile(filepath.Join(clone, ".git", "config"))
	if strings.Contains(string(cfg), encoded) || strings.Contains(string(cfg), "extraheader") {
		t.Errorf("the clone's config holds the credential:\n%s", cfg)
	}

	// The remote moves on; a stale clone must be refused, not forced.
	other := filepath.Join(t.TempDir(), "other")
	gitRun("clone", "-q", remoteDir, other)
	moved := commit(other, "elsewhere")
	gitRun("-C", other, "push", "-q", "origin", "main")
	disarm()
	commit(clone, "stale")
	plantTraps()
	_, err = pushRepository(context.Background(), pushRequest{
		source: clone, branch: "main", remote: remote, env: scopedAuthEnv(remote, header),
	})
	if !errors.Is(err, errPushRejected) {
		t.Fatalf("stale push err = %v, want errPushRejected", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("something the agent planted in the clone ran during the refused push")
	}
	if got := gitRun("-C", remoteDir, "rev-parse", "refs/heads/main"); got != moved {
		t.Errorf("remote main = %s after a refused push, want it untouched at %s", got, moved)
	}

	// A branch the clone does not have is a clear failure, not an empty push.
	if _, err := pushRepository(context.Background(), pushRequest{
		source: clone, branch: "nope", remote: remote, env: scopedAuthEnv(remote, header),
	}); err == nil || errors.Is(err, errPushRejected) || !strings.Contains(err.Error(), "could not be read from the clone") {
		t.Fatalf("missing branch err = %v", err)
	}
}

func TestGitHubTokenCallBodyAcceptsOnlyReadOrWrite(t *testing.T) {
	for _, contents := range []string{githubContentsRead, githubContentsWrite} {
		if _, err := githubTokenCallBody([]string{"r"}, contents); err != nil {
			t.Errorf("contents %q refused: %v", contents, err)
		}
	}
	for _, contents := range []string{"", "admin", "READ"} {
		if _, err := githubTokenCallBody([]string{"r"}, contents); err == nil {
			t.Errorf("contents %q accepted", contents)
		}
	}
}
