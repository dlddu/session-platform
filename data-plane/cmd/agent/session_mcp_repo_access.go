// Repo access (docs/prd/repo-access.md): the session MCP's side of the allow
// list the control plane keeps. `request_repo_access` (AC-H4) records a request
// a person decides in the control plane; `git_clone` asks whether a github.com
// repository is on the list and, if it is, clones it with a token minted for
// that one repository through the helper pod's K3s MCP proxy (AC-H5).
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// Keep in sync with control-plane/internal/adapter/k8s. The control plane
	// sets both only where the feature is enabled (AC-H1).
	repoAccessURLEnv       = "REPO_ACCESS_URL"
	repoAccessTokenFileEnv = "REPO_ACCESS_TOKEN_FILE"

	requestRepoAccessTool = "request_repo_access"
	repoAccessCheckPath   = "/api/v1/internal/repo-access/check"
	repoAccessRequestPath = "/api/v1/internal/repo-access/requests"

	repoAccessStatusPending = "pending"
	repoAccessStatusAllowed = "allowed"

	maxRepoAccessReasonChars    = 500
	repoAccessTimeout           = 15 * time.Second
	githubTokenTimeout          = 30 * time.Second
	maxRepoAccessResponseBytes  = 1 << 20
	maxRepoAccessErrorTextBytes = 200

	githubHost = "github.com"
)

var repoAccessRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// githubPathPattern is the whole path of a repository URL on github.com: the
// owner, the repository and an optional .git suffix, nothing below it.
var githubPathPattern = regexp.MustCompile(`^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(\.git)?/?$`)

// repoAccessClient calls the control plane's internal repo-access API as this
// helper pod (AC-H3).
type repoAccessClient struct {
	baseURL   string
	tokenFile string
	client    *http.Client
}

// newRepoAccessClient returns nil unless both halves are configured, which is
// how a deployment without the feature keeps today's git_clone and lists no
// request tool.
func newRepoAccessClient(baseURL, tokenFile string) *repoAccessClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	tokenFile = strings.TrimSpace(tokenFile)
	if baseURL == "" || tokenFile == "" {
		return nil
	}
	return &repoAccessClient{baseURL: baseURL, tokenFile: tokenFile, client: &http.Client{Timeout: repoAccessTimeout}}
}

type repoAccessCheck struct {
	Repo    string `json:"repo"`
	Allowed bool   `json:"allowed"`
}

type repoAccessRequest struct {
	Repo      string `json:"repo"`
	Status    string `json:"status"`
	RequestID string `json:"requestId"`
}

func (c *repoAccessClient) check(ctx context.Context, repo string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+repoAccessCheckPath+"?repo="+url.QueryEscape(repo), nil)
	if err != nil {
		return false, fmt.Errorf("build the allow list check: %w", err)
	}
	var out repoAccessCheck
	if err := c.do(req, &out); err != nil {
		return false, err
	}
	return out.Allowed, nil
}

func (c *repoAccessClient) request(ctx context.Context, repo, reason string) (repoAccessRequest, error) {
	body, err := json.Marshal(map[string]string{"repo": repo, "reason": reason})
	if err != nil {
		return repoAccessRequest{}, fmt.Errorf("encode the access request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+repoAccessRequestPath, bytes.NewReader(body))
	if err != nil {
		return repoAccessRequest{}, fmt.Errorf("build the access request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	var out repoAccessRequest
	if err := c.do(req, &out); err != nil {
		return repoAccessRequest{}, err
	}
	return out, nil
}

// do re-reads the token for every call, for readApprovalToken's reason.
func (c *repoAccessClient) do(req *http.Request, out any) error {
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return errors.New("the control plane service account token could not be read")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return errors.New("the control plane service account token is empty")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return errors.New("the control plane could not be reached")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRepoAccessResponseBytes))
	if err != nil {
		return errors.New("the control plane response could not be read")
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return errors.New("the control plane did not accept this helper pod's token (401)")
	case http.StatusServiceUnavailable:
		return errors.New("repo access is not enabled on the control plane (503)")
	case http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Error) != "" {
			return fmt.Errorf("the control plane refused the request: %s", truncateText(strings.TrimSpace(e.Error), maxRepoAccessErrorTextBytes))
		}
		return errors.New("the control plane refused the request (400)")
	default:
		return fmt.Errorf("the control plane returned %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("the control plane returned a non-JSON body")
	}
	return nil
}

func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:validUTF8PrefixAtMost([]byte(s), limit)] + "…"
}

// githubTokenMinter returns the token mint that goes through the helper pod's
// K3s MCP proxy, or nil without one. The proxy holds the K3s MCP token and
// injects it, so this side sends none.
func githubTokenMinter(proxyMCPURL string) func(context.Context, string) (string, error) {
	if strings.TrimSpace(proxyMCPURL) == "" {
		return nil
	}
	client := &http.Client{Timeout: githubTokenTimeout}
	return func(ctx context.Context, repository string) (string, error) {
		return githubTokenFromProxy(ctx, client, proxyMCPURL, repository)
	}
}

func githubTokenFromProxy(ctx context.Context, client *http.Client, proxyMCPURL, repository string) (string, error) {
	body, err := githubTokenCallBody([]string{repository})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyMCPURL, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("the K3s MCP proxy URL is invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("the K3s MCP proxy could not be reached")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRepoAccessResponseBytes))
	if err != nil {
		return "", errors.New("the K3s MCP proxy response could not be read")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("K3s MCP GitHub token request returned status %d", resp.StatusCode)
	}
	return parseGitHubTokenResponse(raw)
}

// githubRepo is a clone target this feature applies to.
type githubRepo struct {
	owner, name string
	// scope is the URL the auth header is bound to: the target itself, so the
	// header never reaches a URL other than the one a human approved.
	scope string
}

func (r githubRepo) fullName() string { return r.owner + "/" + r.name }

// githubRepoOf recognises an https://github.com/owner/repo(.git) URL. Anything
// else — another host, plain http, a query, a deeper path — is not a candidate
// for a token and keeps today's anonymous clone.
func githubRepoOf(target string) (githubRepo, bool) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.Host != githubHost ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return githubRepo{}, false
	}
	m := githubPathPattern.FindStringSubmatch(parsed.Path)
	if m == nil {
		return githubRepo{}, false
	}
	return githubRepo{
		owner: m[1],
		name:  m[2],
		scope: "https://" + githubHost + strings.TrimRight(parsed.Path, "/"),
	}, true
}

// scopedAuthEnv is the clone's only carrier of the token: config given through
// the environment applies to that one git process and is never written to the
// clone's .git/config.
func scopedAuthEnv(scope, header string) []string {
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + scope + ".extraheader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}

func githubBasicAuthHeader(token string) (header, encoded string) {
	encoded = base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return "Authorization: Basic " + encoded, encoded
}

func redactSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, redactedLiteral)
		}
	}
	return text
}

func validateRepoAccessArgs(repo, reason string) (string, string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", "", errors.New("repo is required, as owner/repo")
	}
	if !repoAccessRepoPattern.MatchString(repo) {
		return "", "", errors.New("repo must be owner/repo with letters, digits, '.', '_' or '-'")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", "", errors.New("reason is required")
	}
	if utf8.RuneCountInString(reason) > maxRepoAccessReasonChars {
		return "", "", fmt.Errorf("reason must be at most %d characters", maxRepoAccessReasonChars)
	}
	return repo, reason, nil
}

// callRequestRepoAccess is AC-H4's tool. It does not pass the approval gate:
// it opens nothing, and the person deciding in the control plane is its gate.
func (c sessionMCPConfig) callRequestRepoAccess(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	var args struct {
		Repo   string `json:"repo"`
		Reason string `json:"reason"`
	}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return mcpToolError("arguments must be an object with a repo and a reason"), nil
		}
	}
	repo, reason, err := validateRepoAccessArgs(args.Repo, args.Reason)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	if c.repoAccess == nil {
		return mcpToolError("repo access is not configured on this deployment"), nil
	}

	out, err := c.repoAccess.request(ctx, repo, reason)
	if err != nil {
		logger.Error("repo access request failed", "tool", requestRepoAccessTool, "repo", repo, "err", err)
		return mcpToolError("access request failed: " + err.Error()), nil
	}
	if out.Repo == "" {
		out.Repo = repo
	}
	var message string
	switch out.Status {
	case repoAccessStatusPending:
		message = "Request recorded. A person decides in the control plane's Repo access screen; nothing opens until then."
		c.notices.publish(approvalNotice{Kind: noticeAccessRequested, Tool: requestRepoAccessTool, Repo: repo})
	case repoAccessStatusAllowed:
		message = "Already on the allow list."
	default:
		logger.Error("repo access request returned an unknown status", "tool", requestRepoAccessTool, "status", out.Status)
		return mcpToolError(fmt.Sprintf("access request failed: the control plane answered with status %q", truncateText(out.Status, 40))), nil
	}
	logger.Info("repo access requested", "tool", requestRepoAccessTool, "repo", out.Repo, "status", out.Status)
	payload, err := json.Marshal(map[string]string{
		"repo":      out.Repo,
		"status":    out.Status,
		"requestId": out.RequestID,
		"message":   message,
	})
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	return mcpToolText(string(payload)), nil
}
