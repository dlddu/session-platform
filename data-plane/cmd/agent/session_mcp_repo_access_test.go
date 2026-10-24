package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testHelperToken = "helper-sa-token"
	testGitHubToken = "ghs_test"
)

// syncBuffer collects log output written from the server's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeControlPlane stands in for the internal repo-access API (AC-H3).
type fakeControlPlane struct {
	server   *httptest.Server
	checks   atomic.Int32
	requests atomic.Int32

	mu            sync.Mutex
	allowed       bool
	requestStatus string
	statusCode    int
	errorBody     string
	authHeaders   []string
	checkedRepos  []string
	requestBodies []map[string]any
	// onCheck runs inside the check handler, so a test can observe what had
	// already happened when the allow list was asked.
	onCheck func()
}

func newFakeControlPlane(t *testing.T) *fakeControlPlane {
	t.Helper()
	cp := &fakeControlPlane{requestStatus: repoAccessStatusPending, statusCode: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+repoAccessCheckPath, func(w http.ResponseWriter, r *http.Request) {
		cp.checks.Add(1)
		cp.mu.Lock()
		defer cp.mu.Unlock()
		cp.authHeaders = append(cp.authHeaders, r.Header.Get("Authorization"))
		repo := r.URL.Query().Get("repo")
		cp.checkedRepos = append(cp.checkedRepos, repo)
		if cp.onCheck != nil {
			cp.onCheck()
		}
		if cp.statusCode != http.StatusOK {
			w.WriteHeader(cp.statusCode)
			_, _ = w.Write([]byte(cp.errorBody))
			return
		}
		_ = json.NewEncoder(w).Encode(repoAccessCheck{Repo: repo, Allowed: cp.allowed})
	})
	mux.HandleFunc("POST "+repoAccessRequestPath, func(w http.ResponseWriter, r *http.Request) {
		cp.requests.Add(1)
		cp.mu.Lock()
		defer cp.mu.Unlock()
		cp.authHeaders = append(cp.authHeaders, r.Header.Get("Authorization"))
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.requestBodies = append(cp.requestBodies, body)
		if cp.statusCode != http.StatusOK {
			w.WriteHeader(cp.statusCode)
			_, _ = w.Write([]byte(cp.errorBody))
			return
		}
		repo, _ := body["repo"].(string)
		_ = json.NewEncoder(w).Encode(repoAccessRequest{Repo: repo, Status: cp.requestStatus, RequestID: "rar-1"})
	})
	cp.server = httptest.NewServer(mux)
	t.Cleanup(cp.server.Close)
	return cp
}

// fakeK3SProxy stands in for the helper pod's k3s-mcp-proxy container.
type fakeK3SProxy struct {
	server *httptest.Server
	calls  atomic.Int32

	mu       sync.Mutex
	body     string
	bodies   []map[string]any
	headers  []http.Header
	response string
}

func newFakeK3SProxy(t *testing.T) *fakeK3SProxy {
	t.Helper()
	p := &fakeK3SProxy{
		response: `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"resource","resource":{"text":"# Expires at: x\nGITHUB_TOKEN=` + testGitHubToken + `\n"}}]}}`,
	}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		p.mu.Lock()
		defer p.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/mcp" {
			t.Errorf("proxy got %s %s, want POST /mcp", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.bodies = append(p.bodies, body)
		p.headers = append(p.headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(p.response))
	}))
	t.Cleanup(p.server.Close)
	return p
}

type repoAccessMCP struct {
	server  *httptest.Server
	gateway *fakeGateway
	cp      *fakeControlPlane
	proxy   *fakeK3SProxy
	logs    *syncBuffer
	notices *noticeFeed
	clones  atomic.Int32
	calls   []cloneCall
	// cloneErr, when set, is what the clone seam fails with.
	cloneErr func(env []string) error
}

type repoAccessOptions struct {
	noRepoAccess bool
	noProxy      bool
	tokenFile    string
}

func newRepoAccessMCP(t *testing.T, opts repoAccessOptions, statuses ...string) *repoAccessMCP {
	t.Helper()
	m := &repoAccessMCP{
		gateway: newFakeGateway(t, statuses...),
		cp:      newFakeControlPlane(t),
		proxy:   newFakeK3SProxy(t),
		logs:    &syncBuffer{},
		notices: newNoticeFeed(),
	}
	gate := m.gateway.gate(t)
	gate.sleep = func(context.Context, time.Duration) error { return nil }
	config := sessionMCPConfig{
		gateway:   gate,
		sharedDir: t.TempDir(),
		notices:   m.notices,
		clone: func(_ context.Context, target, branch, dest string, env []string) error {
			m.clones.Add(1)
			m.calls = append(m.calls, cloneCall{target: target, branch: branch, dest: dest, env: env})
			if m.cloneErr != nil {
				if err := m.cloneErr(env); err != nil {
					return err
				}
			}
			return os.MkdirAll(filepath.Join(dest, ".git"), 0o700)
		},
	}
	if !opts.noRepoAccess {
		tokenFile := opts.tokenFile
		if tokenFile == "" {
			tokenFile = writeToken(t, testHelperToken)
		}
		config.repoAccess = newRepoAccessClient(m.cp.server.URL, tokenFile)
	}
	if !opts.noProxy {
		config.mintGitHubToken = githubTokenMinter(m.proxy.server.URL + "/mcp")
	}
	logger := slog.New(slog.NewTextHandler(m.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m.server = httptest.NewServer(sessionMCPRoutes(logger, config))
	t.Cleanup(m.server.Close)
	return m
}

func (m *repoAccessMCP) call(t *testing.T, tool string, args map[string]any) (bool, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 31, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, decoded := postMCP(t, m.server, string(body))
	if status != http.StatusOK {
		t.Fatalf("tools/call = %d, want 200", status)
	}
	_, isError, text := toolResult(t, decoded)
	return isError, text
}

func (m *repoAccessMCP) toolNames(t *testing.T) map[string]bool {
	t.Helper()
	_, body := postMCP(t, m.server, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ := body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
	}
	return names
}

func basicAuthFor(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// assertNoSecret is AC-H5's "토큰은 … 어디에도 남지 않는다" for everything a
// test can see from outside: the tool result, the logs and the approval
// context a human read.
func (m *repoAccessMCP) assertNoSecret(t *testing.T, texts ...string) {
	t.Helper()
	m.gateway.mu.Lock()
	approvalContext, _ := m.gateway.lastBody["context"].(string)
	m.gateway.mu.Unlock()
	for _, haystack := range append(texts, m.logs.String(), approvalContext) {
		for _, secret := range []string{testGitHubToken, basicAuthFor(testGitHubToken), testHelperToken} {
			if strings.Contains(haystack, secret) {
				t.Errorf("a secret (%q) leaked into %q", secret, haystack)
			}
		}
	}
}

func envEntry(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// R2 plus AC-H1: the request tool stands only where both the gate and the
// allow list do.
func TestRequestRepoAccessIsListedOnlyWithTheGateAndTheAllowList(t *testing.T) {
	with := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	if names := with.toolNames(t); !names[requestRepoAccessTool] || len(names) != 4 {
		t.Fatalf("tools = %v, want the three tools plus %s", names, requestRepoAccessTool)
	}
	without := newRepoAccessMCP(t, repoAccessOptions{noRepoAccess: true}, "APPROVED")
	if names := without.toolNames(t); names[requestRepoAccessTool] || len(names) != 3 {
		t.Fatalf("tools = %v, want no %s without an allow list", names, requestRepoAccessTool)
	}
	if isError, text := without.call(t, requestRepoAccessTool, map[string]any{"repo": "a/b", "reason": "x"}); !isError {
		t.Fatalf("an unlisted request tool answered: %s", text)
	}

	ungated := newSessionMCPServerWith(t, sessionMCPConfig{repoAccess: newRepoAccessClient("http://cp.test", writeToken(t, "x"))})
	_, body := postMCP(t, ungated, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if tools := body["result"].(map[string]any)["tools"].([]any); len(tools) != 0 {
		t.Fatalf("ungated tools = %v, want none", tools)
	}

	for _, tc := range []struct{ url, file string }{{"", "/tok"}, {"http://cp.test", ""}, {"  ", "  "}} {
		if c := newRepoAccessClient(tc.url, tc.file); c != nil {
			t.Errorf("newRepoAccessClient(%q, %q) = %+v, want nil", tc.url, tc.file, c)
		}
	}
}

// R3: an argument that cannot become a request never reaches the control plane.
func TestRequestRepoAccessValidatesBeforeAnyCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"no repo", map[string]any{"reason": "need it"}},
		{"owner only", map[string]any{"repo": "dlddu", "reason": "need it"}},
		{"too deep", map[string]any{"repo": "dlddu/a/b", "reason": "need it"}},
		{"a URL", map[string]any{"repo": "https://github.com/dlddu/a", "reason": "need it"}},
		{"a space", map[string]any{"repo": "dl ddu/a", "reason": "need it"}},
		{"a newline", map[string]any{"repo": "dlddu/a\nb", "reason": "need it"}},
		{"no reason", map[string]any{"repo": "dlddu/a"}},
		{"blank reason", map[string]any{"repo": "dlddu/a", "reason": "   "}},
		{"overlong reason", map[string]any{"repo": "dlddu/a", "reason": strings.Repeat("가", maxRepoAccessReasonChars+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
			if isError, text := m.call(t, requestRepoAccessTool, tc.args); !isError {
				t.Fatalf("%v was accepted: %s", tc.args, text)
			}
			if got := m.cp.requests.Load(); got != 0 {
				t.Errorf("control plane got %d requests for an invalid argument, want 0", got)
			}
			if got := m.gateway.created.Load(); got != 0 {
				t.Errorf("approval gateway got %d requests, want 0", got)
			}
		})
	}
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	if isError, text := m.call(t, requestRepoAccessTool, map[string]any{
		"repo": "dlddu/a", "reason": strings.Repeat("가", maxRepoAccessReasonChars),
	}); isError {
		t.Fatalf("a reason at the limit was refused: %s", text)
	}
}

// AC-H4: the request goes to the control plane, never to the approval gateway,
// and leaves a marker saying where it is decided.
func TestRequestRepoAccessRecordsAPendingRequestWithoutTheGate(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	isError, text := m.call(t, requestRepoAccessTool, map[string]any{"repo": "dlddu/private-repo", "reason": "read the schema"})
	if isError {
		t.Fatalf("request failed: %s", text)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("result %q is not JSON: %v", text, err)
	}
	if payload["repo"] != "dlddu/private-repo" || payload["status"] != repoAccessStatusPending || payload["requestId"] != "rar-1" {
		t.Errorf("payload = %v", payload)
	}
	if want := "Request recorded. A person decides in the control plane's Repo access screen; nothing opens until then."; payload["message"] != want {
		t.Errorf("message = %q, want %q", payload["message"], want)
	}
	if got := m.gateway.created.Load(); got != 0 {
		t.Errorf("approval gateway got %d requests, want 0", got)
	}
	if got := m.cp.requests.Load(); got != 1 {
		t.Fatalf("control plane got %d requests, want 1", got)
	}
	m.cp.mu.Lock()
	body, auth := m.cp.requestBodies[0], m.cp.authHeaders[0]
	m.cp.mu.Unlock()
	if body["repo"] != "dlddu/private-repo" || body["reason"] != "read the schema" {
		t.Errorf("request body = %v", body)
	}
	if auth != "Bearer "+testHelperToken {
		t.Errorf("Authorization = %q, want the helper's projected token", auth)
	}
	m.assertNoSecret(t, text)

	notices, _, _, _ := m.notices.since(0)
	if len(notices) != 1 || notices[0].Kind != noticeAccessRequested || notices[0].Repo != "dlddu/private-repo" {
		t.Fatalf("notices = %+v, want one access-requested notice", notices)
	}
	got := collectMarkers(t, m.server.URL, 1)
	if want := "\n[session-platform: access requested — dlddu/private-repo · decide in the control plane's Repo access screen]\n"; got != want {
		t.Errorf("marker = %q, want %q", got, want)
	}
}

func TestRequestRepoAccessReportsAnAlreadyAllowedRepository(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	m.cp.requestStatus = repoAccessStatusAllowed
	isError, text := m.call(t, requestRepoAccessTool, map[string]any{"repo": "dlddu/private-repo", "reason": "x"})
	if isError {
		t.Fatalf("request failed: %s", text)
	}
	var payload map[string]string
	_ = json.Unmarshal([]byte(text), &payload)
	if payload["status"] != repoAccessStatusAllowed || payload["message"] != "Already on the allow list." {
		t.Errorf("payload = %v", payload)
	}
	if notices, _, _, _ := m.notices.since(0); len(notices) != 0 {
		t.Errorf("notices = %+v, want none when nothing waits on a person", notices)
	}
}

// R7: every control plane failure is a tool error with a short reason.
func TestRequestRepoAccessFailuresAreToolErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"bad request", http.StatusBadRequest, `{"error":"invalid repo"}`, "invalid repo"},
		{"unauthenticated", http.StatusUnauthorized, `{"error":"unauthenticated"}`, "401"},
		{"disabled", http.StatusServiceUnavailable, `{"error":"repo access is not enabled"}`, "503"},
		{"broken", http.StatusInternalServerError, ``, "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
			m.cp.statusCode, m.cp.errorBody = tc.status, tc.body
			isError, text := m.call(t, requestRepoAccessTool, map[string]any{"repo": "dlddu/a", "reason": "x"})
			if !isError || !strings.Contains(text, tc.want) {
				t.Fatalf("result = %v %q, want a tool error naming %q", isError, text, tc.want)
			}
			m.assertNoSecret(t, text)
		})
	}
}

// The kubelet rotates the projected token in place (approval_gateway.go).
func TestRepoAccessRereadsTheTokenFileOnEveryCall(t *testing.T) {
	tokenFile := writeToken(t, "first")
	m := newRepoAccessMCP(t, repoAccessOptions{tokenFile: tokenFile}, "APPROVED")
	m.call(t, requestRepoAccessTool, map[string]any{"repo": "dlddu/a", "reason": "x"})
	if err := os.WriteFile(tokenFile, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.call(t, requestRepoAccessTool, map[string]any{"repo": "dlddu/a", "reason": "x"})
	m.cp.mu.Lock()
	defer m.cp.mu.Unlock()
	if len(m.cp.authHeaders) != 2 || m.cp.authHeaders[0] != "Bearer first" || m.cp.authHeaders[1] != "Bearer second" {
		t.Fatalf("Authorization headers = %v, want the token as it was at each call", m.cp.authHeaders)
	}
}

// The new kind is not a wait: it must neither open nor close an entry of the
// set AC-F3's idle exception reads.
func TestAccessRequestedNoticeLeavesTheWaitSetAlone(t *testing.T) {
	var w approvalWaits
	w.observe([]approvalNotice{
		awaitingNotice("s:1"),
		{Kind: noticeAccessRequested, Tool: requestRepoAccessTool, Repo: "dlddu/a"},
		{Kind: noticeAccessRequested, Tool: requestRepoAccessTool, Repo: "dlddu/a", ExternalID: "s:1"},
		{Kind: noticeAccessRequested, Tool: requestRepoAccessTool, Repo: "dlddu/b", ExternalID: "s:2"},
	}, 0)
	if awaiting, pending := w.state(); !awaiting || pending != 1 {
		t.Fatalf("state = %v/%d, want exactly the one real wait", awaiting, pending)
	}
	if got := renderApprovalNotice(approvalNotice{Kind: noticeAccessRequested, Repo: "a/b]\n[session-platform: x"}); got != "" {
		t.Errorf("a malformed repository rendered as %q, want nothing", got)
	}
}

func TestGitHubRepoOfRecognisesOnlyGitHubRepositoryURLs(t *testing.T) {
	for _, tc := range []struct {
		url              string
		ok               bool
		owner, name, uri string
	}{
		{"https://github.com/dlddu/private-repo", true, "dlddu", "private-repo", "https://github.com/dlddu/private-repo"},
		{"https://github.com/dlddu/private-repo.git", true, "dlddu", "private-repo", "https://github.com/dlddu/private-repo.git"},
		{"https://github.com/dlddu/private-repo/", true, "dlddu", "private-repo", "https://github.com/dlddu/private-repo"},
		{"https://github.com/dlddu/repo.name.git", true, "dlddu", "repo.name", "https://github.com/dlddu/repo.name.git"},
		{"http://github.com/dlddu/private-repo", false, "", "", ""},
		{"https://www.github.com/dlddu/private-repo", false, "", "", ""},
		{"https://github.com:443/dlddu/private-repo", false, "", "", ""},
		{"https://gitlab.com/dlddu/private-repo", false, "", "", ""},
		{"https://github.com/dlddu", false, "", "", ""},
		{"https://github.com/dlddu/private-repo/tree/main", false, "", "", ""},
		{"https://github.com/dlddu/private-repo?x=1", false, "", "", ""},
		{"https://github.com/dlddu/private%2Frepo", false, "", "", ""},
	} {
		got, ok := githubRepoOf(tc.url)
		if ok != tc.ok || got.owner != tc.owner || got.name != tc.name || got.scope != tc.uri {
			t.Errorf("githubRepoOf(%q) = %+v, %v; want %s/%s scoped to %q, %v", tc.url, got, ok, tc.owner, tc.name, tc.uri, tc.ok)
		}
	}
}

// AC-H5 end to end inside the helper pod: approval, then the allow list, then a
// token for that one repository, carried to git only as a URL-scoped header.
func TestGitCloneClonesAnAllowedPrivateRepositoryWithAScopedToken(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	m.cp.allowed = true
	var createdAtCheck, pollsAtCheck int32
	m.cp.onCheck = func() { createdAtCheck, pollsAtCheck = m.gateway.created.Load(), m.gateway.polls.Load() }
	const target = "https://github.com/dlddu/private-repo.git"

	isError, text := m.call(t, gitCloneTool, map[string]any{"url": target})
	if isError {
		t.Fatalf("private clone failed: %s", text)
	}
	if createdAtCheck != 1 || pollsAtCheck < 1 {
		t.Errorf("the allow list was asked with %d approval requests and %d polls behind it, want it after the decision", createdAtCheck, pollsAtCheck)
	}
	m.cp.mu.Lock()
	checked := append([]string(nil), m.cp.checkedRepos...)
	m.cp.mu.Unlock()
	if len(checked) != 1 || checked[0] != "dlddu/private-repo" {
		t.Errorf("checked %v, want dlddu/private-repo", checked)
	}

	if got := m.proxy.calls.Load(); got != 1 {
		t.Fatalf("proxy got %d token requests, want 1", got)
	}
	m.proxy.mu.Lock()
	body, header := m.proxy.bodies[0], m.proxy.headers[0]
	m.proxy.mu.Unlock()
	params, _ := body["params"].(map[string]any)
	args, _ := params["arguments"].(map[string]any)
	if body["method"] != "tools/call" || params["name"] != githubTokenTool {
		t.Errorf("proxy body = %v, want a tools/call of %s", body, githubTokenTool)
	}
	if repos, _ := args["repositories"].([]any); len(repos) != 1 || repos[0] != "private-repo" {
		t.Errorf("repositories = %v, want only this repository", args["repositories"])
	}
	if perms, _ := args["permissions"].(map[string]any); len(perms) != 1 || perms["contents"] != "read" {
		t.Errorf("permissions = %v, want contents: read only", args["permissions"])
	}
	if header.Get("Content-Type") != "application/json" || header.Get("Accept") != "application/json" {
		t.Errorf("proxy headers = %v", header)
	}
	if header.Get("Authorization") != "" {
		t.Errorf("the session MCP sent its own Authorization %q to the proxy", header.Get("Authorization"))
	}

	if got := m.clones.Load(); got != 1 {
		t.Fatalf("git ran %d times, want 1", got)
	}
	env := m.calls[0].env
	for key, want := range map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http." + target + ".extraheader",
		"GIT_CONFIG_VALUE_0": "Authorization: Basic " + basicAuthFor(testGitHubToken),
	} {
		if got, _ := envEntry(env, key); got != want {
			t.Errorf("clone env %s = %q, want %q", key, got, want)
		}
	}
	if m.calls[0].target != target {
		t.Errorf("cloned %q, want the approved URL unchanged", m.calls[0].target)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil || payload["success"] != true || payload["url"] != target {
		t.Errorf("payload = %v (%v)", payload, err)
	}
	m.assertNoSecret(t, text)
}

// A token that git could not use surfaces as AC-H5's "not found or no
// permission", with whatever git said scrubbed of the credential.
func TestGitClonePrivateFailureIsRedactedAndNamed(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	m.cp.allowed = true
	m.cloneErr = func(env []string) error {
		value, _ := envEntry(env, "GIT_CONFIG_VALUE_0")
		return fmt.Errorf("exit status 128: fatal: repository not found (sent %s, token %s)", value, testGitHubToken)
	}
	isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/private-repo"})
	if !isError || !strings.Contains(text, "repository not found or no permission") {
		t.Fatalf("result = %v %q, want the not-found/no-permission failure", isError, text)
	}
	if !strings.Contains(text, redactedLiteral) {
		t.Errorf("failure text = %q, want git's reason with the secret redacted", text)
	}
	m.assertNoSecret(t, text)
}

func TestGitCloneOfARepositoryOffTheAllowList(t *testing.T) {
	t.Run("anonymous clone fails", func(t *testing.T) {
		m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
		m.cloneErr = func([]string) error { return errors.New("exit status 128: fatal: could not read Username") }
		isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/private-repo.git"})
		if !isError || !strings.Contains(text, "not on the allow list") || !strings.Contains(text, requestRepoAccessTool) {
			t.Fatalf("result = %v %q, want the allow-list failure pointing at %s", isError, text, requestRepoAccessTool)
		}
		if !strings.Contains(text, "could not read Username") {
			t.Errorf("failure text = %q, want git's own reason too", text)
		}
		if got := m.proxy.calls.Load(); got != 0 {
			t.Errorf("proxy got %d token requests for a repository off the list, want 0", got)
		}
		if m.clones.Load() != 1 || m.calls[0].env != nil {
			t.Errorf("clones = %d with env %v, want one anonymous attempt", m.clones.Load(), m.calls)
		}
	})
	t.Run("a public repository still clones", func(t *testing.T) {
		m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
		isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/pure-agent.git"})
		if isError {
			t.Fatalf("public clone failed: %s", text)
		}
		if m.calls[0].env != nil || m.proxy.calls.Load() != 0 {
			t.Errorf("a public clone carried env %v / asked for %d tokens", m.calls[0].env, m.proxy.calls.Load())
		}
	})
}

func TestGitCloneWithoutAnIssuedTokenDoesNotClone(t *testing.T) {
	t.Run("the tool refuses", func(t *testing.T) {
		m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
		m.cp.allowed = true
		m.proxy.response = `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"no installation"}]}}`
		isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/private-repo"})
		if !isError || !strings.Contains(text, "token could not be issued") {
			t.Fatalf("result = %v %q, want the token failure", isError, text)
		}
		if got := m.clones.Load(); got != 0 {
			t.Errorf("git ran %d times without a token, want 0", got)
		}
	})
	t.Run("no proxy in the helper pod", func(t *testing.T) {
		m := newRepoAccessMCP(t, repoAccessOptions{noProxy: true}, "APPROVED")
		m.cp.allowed = true
		isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/private-repo"})
		if !isError || !strings.Contains(text, "token could not be issued") {
			t.Fatalf("result = %v %q, want the token failure", isError, text)
		}
		if got := m.clones.Load(); got != 0 {
			t.Errorf("git ran %d times without a token, want 0", got)
		}
	})
}

// Only a github.com repository is a question for the allow list, and only on a
// deployment that has one.
func TestGitCloneAsksTheAllowListOnlyForGitHubRepositories(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts repoAccessOptions
		url  string
	}{
		{"another host", repoAccessOptions{}, "https://gitlab.com/dlddu/private-repo.git"},
		{"plain http to github", repoAccessOptions{}, "http://github.com/dlddu/private-repo.git"},
		{"no allow list", repoAccessOptions{noRepoAccess: true}, "https://github.com/dlddu/private-repo.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newRepoAccessMCP(t, tc.opts, "APPROVED")
			m.cp.allowed = true
			isError, text := m.call(t, gitCloneTool, map[string]any{"url": tc.url})
			if isError {
				t.Fatalf("clone failed: %s", text)
			}
			if m.cp.checks.Load() != 0 || m.proxy.calls.Load() != 0 {
				t.Errorf("checks = %d, token requests = %d; want neither", m.cp.checks.Load(), m.proxy.calls.Load())
			}
			if m.clones.Load() != 1 || m.calls[0].env != nil {
				t.Errorf("want one anonymous clone, got %+v", m.calls)
			}
		})
	}
}

// A refused call stops before the allow list is consulted at all.
func TestGitCloneRefusalNeverAsksTheAllowList(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "REJECTED")
	m.cp.allowed = true
	isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/private-repo"})
	if !isError || !strings.Contains(text, "refused at approval") {
		t.Fatalf("result = %v %q, want the approval refusal", isError, text)
	}
	if m.cp.checks.Load() != 0 || m.proxy.calls.Load() != 0 || m.clones.Load() != 0 {
		t.Errorf("checks = %d, tokens = %d, clones = %d; want all zero", m.cp.checks.Load(), m.proxy.calls.Load(), m.clones.Load())
	}
}

// An allow list that cannot be asked does not cost a public clone.
func TestGitCloneFallsBackToAnonymousWhenTheCheckFails(t *testing.T) {
	m := newRepoAccessMCP(t, repoAccessOptions{}, "APPROVED")
	m.cp.statusCode = http.StatusServiceUnavailable
	if isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/pure-agent"}); isError {
		t.Fatalf("public clone failed while the check was down: %s", text)
	}
	m.cloneErr = func([]string) error { return errors.New("fatal: could not read Username") }
	isError, text := m.call(t, gitCloneTool, map[string]any{"url": "https://github.com/dlddu/private-repo"})
	if !isError || !strings.Contains(text, "allow list could not be checked") || !strings.Contains(text, "503") {
		t.Fatalf("result = %v %q, want the clone failure with the check's reason", isError, text)
	}
	if m.proxy.calls.Load() != 0 {
		t.Errorf("a token was requested without an allow list answer")
	}
	m.assertNoSecret(t, text)
}

// The real git, against a local smart-HTTP remote: the header reaches the
// server, and nothing of it is written into the clone's configuration.
func TestCloneRepositorySendsTheScopedHeaderOnlyThroughTheEnvironment(t *testing.T) {
	backend := filepath.Join("/usr/lib/git-core", "git-http-backend")
	if out, err := exec.Command("git", "--exec-path").Output(); err == nil {
		backend = filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	}
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend not installed")
	}
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "src")
	for _, args := range [][]string{
		{"init", "-q", src},
		{"-C", src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"clone", "-q", "--bare", src, filepath.Join(root, "private-repo.git")},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
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

	target := srv.URL + "/private-repo.git"
	header, encoded := githubBasicAuthHeader(testGitHubToken)
	dest := filepath.Join(t.TempDir(), "clone")
	if err := cloneRepository(context.Background(), target, noBranch, dest, scopedAuthEnv(target, header)); err != nil {
		t.Fatalf("clone: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the remote saw no request")
	}
	for _, got := range seen {
		if got != "Basic "+encoded {
			t.Errorf("remote saw Authorization %q, want the scoped header on every request", got)
		}
	}
	config, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"extraheader", encoded, testGitHubToken} {
		if strings.Contains(string(config), secret) {
			t.Errorf(".git/config holds %q:\n%s", secret, config)
		}
	}
}
