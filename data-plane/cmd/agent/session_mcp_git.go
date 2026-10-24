// `git_clone`, the session MCP's second gated tool. The name, the arguments and
// the result shape are the reference implementation's (dlddu/pure-agent,
// mcp-server/src/tools/git-clone.ts). Where this one deviates, and which rule
// made it deviate: the `git_clone` paragraphs of
// docs/session-mcp-tool-surface.md's 「지금 있는 것」.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	gitCloneTool = "git_clone"
	cloneSubdir  = "git_clone"
	// cloneTimeout is the reference implementation's five minutes. It bounds the
	// approved clone; the wait before it is bounded by the gateway's timeout.
	cloneTimeout = 5 * time.Minute
	// maxCloneBranchChars is the reference implementation's branch bound.
	maxCloneBranchChars = 256
	// maxCloneErrorBytes bounds what a failed clone can put in a tool result. A
	// remote controls this text, so it is capped rather than trusted.
	maxCloneErrorBytes = 4 << 10
	// gitBin — what keeps it on PATH: data-plane/Dockerfile's runtime stage.
	gitBin = "git"
	// noBranch keeps the branch key present in both the approval context and the
	// result without claiming a branch nobody asked for.
	noBranch = "default"
)

// callGitClone runs one clone. Where the deployment keeps an allow list the
// list is the gate and no approval is asked (AC-H5); elsewhere every call is
// approved first. Either way validation comes before anything leaves (R3).
func (c sessionMCPConfig) callGitClone(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	var args struct {
		URL    string `json:"url"`
		Branch string `json:"branch"`
	}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return mcpToolError("arguments must be an object with a url and an optional branch"), nil
		}
	}
	target, err := validateCloneTarget(args.URL)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	branch, err := validateCloneBranch(args.Branch)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	if c.sharedDir == "" {
		return mcpToolError("no shared volume is configured for this session, so there is nowhere to clone into"), nil
	}
	if c.repoAccess != nil {
		return c.cloneFromAllowList(ctx, logger, target, branch)
	}

	requestID, err := newApprovalRequestID()
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not generate an approval request id"}
	}
	dest := c.clonePath(requestID)
	approvalContext, err := json.Marshal(map[string]string{"url": target, "branch": branch, "path": dest})
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the approval context"}
	}

	externalID := c.gateway.externalID(requestID)
	logger.Info("awaiting approval", "tool", gitCloneTool, "external_id", externalID)
	c.notices.publish(approvalNotice{Kind: noticeAwaiting, Tool: gitCloneTool, ExternalID: externalID})
	outcome, err := c.gateway.await(ctx, requestID, string(approvalContext))
	if err != nil {
		logger.Error("approval could not be obtained", "tool", gitCloneTool, "err", err)
		c.notices.publish(approvalNotice{Kind: noticeUnavailable, Tool: gitCloneTool, ExternalID: externalID})
		return mcpToolError("approval could not be obtained: " + err.Error()), nil
	}
	c.notices.publish(approvalNotice{
		Kind: noticeDecided, Tool: gitCloneTool, ExternalID: externalID,
		Decision: string(outcome.Decision),
	})
	if outcome.Decision != approvalApproved {
		logger.Info("approval refused", "tool", gitCloneTool, "decision", string(outcome.Decision))
		return mcpToolError(fmt.Sprintf("clone refused at approval: git clone request %s", strings.ToLower(string(outcome.Decision)))), nil
	}

	logger.Info("approved, cloning", "tool", gitCloneTool, "path", dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return mcpToolError("prepare the shared volume directory: " + err.Error()), nil
	}
	if err := c.runClone(ctx, logger, target, branch, dest, nil); err != nil {
		return mcpToolError("clone failed: " + err.Error()), nil
	}
	return cloneResult(logger, dest, target, branch)
}

func cloneResult(logger *slog.Logger, dest, target, branch string) (any, *jsonRPCError) {
	payload, err := json.Marshal(map[string]any{
		"success": true,
		"message": "Repository cloned successfully",
		"path":    dest,
		"url":     target,
		"branch":  branch,
	})
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	logger.Info("cloned onto the shared volume", "tool", gitCloneTool, "path", dest)
	return mcpToolText(string(payload)), nil
}

func (c sessionMCPConfig) cloneFromAllowList(ctx context.Context, logger *slog.Logger, target, branch string) (any, *jsonRPCError) {
	repo, ok := githubRepoOf(target)
	if !ok {
		return mcpToolError("only https://github.com/<owner>/<repo> repositories on the allow list can be cloned here"), nil
	}
	allowed, err := c.repoAccess.check(ctx, repo.fullName())
	if err != nil {
		logger.Error("allow list check failed", "tool", gitCloneTool, "repo", repo.fullName(), "err", err)
		return mcpToolError("the allow list could not be checked, so nothing was cloned: " + err.Error()), nil
	}
	if !allowed {
		logger.Info("not on the allow list", "tool", gitCloneTool, "repo", repo.fullName())
		return mcpToolError(fmt.Sprintf("repository %s is not on the allow list. Ask for it with %s; a person "+
			"decides in the control plane, and nothing is cloned until then.", repo.fullName(), requestRepoAccessTool)), nil
	}

	requestID, err := newApprovalRequestID()
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not generate a request id"}
	}
	dest := c.clonePath(requestID)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return mcpToolError("prepare the shared volume directory: " + err.Error()), nil
	}
	logger.Info("on the allow list, cloning", "tool", gitCloneTool, "repo", repo.fullName(), "path", dest)
	if failure := c.cloneAllowed(ctx, logger, repo, target, branch, dest); failure != "" {
		return mcpToolError(failure), nil
	}
	return cloneResult(logger, dest, target, branch)
}

// cloneAllowed falls back to an anonymous clone when no token can be issued:
// a public repository on the list may sit outside the GitHub App's
// installation, and the list already said yes to it.
func (c sessionMCPConfig) cloneAllowed(ctx context.Context, logger *slog.Logger, repo githubRepo, target, branch, dest string) string {
	mintErr := errors.New("this helper pod has no K3s MCP proxy")
	if c.mintGitHubToken != nil {
		token, err := c.mintGitHubToken(ctx, repo.name)
		if err == nil {
			header, encoded := githubBasicAuthHeader(token)
			if err := c.runClone(ctx, logger, target, branch, dest, scopedAuthEnv(repo.scope, header)); err != nil {
				reason := redactSecrets(err.Error(), header, encoded, token)
				logger.Error("clone with an issued token failed", "tool", gitCloneTool, "repo", repo.fullName(), "err", reason)
				return fmt.Sprintf("repository not found or no permission: %s is on the allow list, but the clone with its "+
					"issued token failed: %s", repo.fullName(), reason)
			}
			return ""
		}
		mintErr = err
	}
	logger.Info("no token issued; trying an anonymous clone", "tool", gitCloneTool, "repo", repo.fullName(), "err", mintErr)
	if err := c.runClone(ctx, logger, target, branch, dest, nil); err != nil {
		return fmt.Sprintf("token could not be issued for %s (%s), and an anonymous clone failed: %s",
			repo.fullName(), mintErr.Error(), err.Error())
	}
	return ""
}

// runClone leaves nothing at dest when it fails; spillBody publishes only
// complete files for the same reason.
func (c sessionMCPConfig) runClone(ctx context.Context, logger *slog.Logger, target, branch, dest string, env []string) error {
	clone := c.clone
	if clone == nil {
		clone = cloneRepository
	}
	err := clone(ctx, target, branch, dest, env)
	if err != nil {
		if rmErr := os.RemoveAll(dest); rmErr != nil {
			logger.Error("could not remove the failed clone", "tool", gitCloneTool, "path", dest, "err", rmErr)
		}
	}
	return err
}

// clonePath is where this tool's artifacts go — no segment of it is the
// caller's, for session_mcp_files.go's reason.
func (c sessionMCPConfig) clonePath(requestID string) string {
	return filepath.Join(c.sharedDir, cloneSubdir, requestID)
}

// validateCloneTarget keeps the tool to a repository URL a human can approve by
// reading it. Which rule draws each of these lines: this file's header.
func validateCloneTarget(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("url is required")
	}
	if strings.HasPrefix(trimmed, "-") {
		return "", errors.New("url must not start with a dash")
	}
	if i := strings.IndexFunc(trimmed, isCloneControlChar); i >= 0 {
		return "", errors.New("url contains a control character")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("url is not a valid URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("url must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("url has no host")
	}
	if parsed.User != nil {
		return "", errors.New("url must not carry credentials")
	}
	return parsed.String(), nil
}

// validateCloneBranch accepts the absent branch: the repository's default is
// what the reference implementation falls back to, and git resolves it.
func validateCloneBranch(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return noBranch, nil
	}
	if len(trimmed) > maxCloneBranchChars {
		return "", fmt.Errorf("branch must be at most %d characters", maxCloneBranchChars)
	}
	if strings.HasPrefix(trimmed, "-") {
		return "", errors.New("branch must not start with a dash")
	}
	if i := strings.IndexFunc(trimmed, isCloneControlChar); i >= 0 {
		return "", errors.New("branch contains a control character")
	}
	return trimmed, nil
}

func isCloneControlChar(r rune) bool { return r < 0x20 || r == 0x7f }

// cloneArgs builds the command line. Its two guards: the emptied credential
// helper list (see this file's header), and `--`, which stops option parsing
// even though a leading dash is already refused above — the second guard is
// there for a validation gap the first one might one day have.
func cloneArgs(target, branch, dest string) []string {
	args := []string{"-c", "credential.helper=", "clone"}
	if branch != noBranch {
		args = append(args, "--branch", branch)
	}
	return append(args, "--", target, dest)
}

// cloneRepository is the real clone, and the seam sessionMCPConfig.clone
// replaces in tests — the same arrangement fetchURL has for web_fetch_get.
func cloneRepository(ctx context.Context, target, branch, dest string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, gitBin, cloneArgs(target, branch, dest)...)
	// Prompt off so a URL needing credentials fails instead of blocking until
	// the timeout with nothing to report; NOSYSTEM so a helper in
	// /etc/gitconfig cannot answer in place of the prompt.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, env...)
	stderr := &boundedBuffer{limit: maxCloneErrorBytes}
	cmd.Stdout = nil
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			return fmt.Errorf("git clone did not finish within %s", cloneTimeout)
		}
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

// boundedBuffer keeps a remote's diagnostics from becoming this container's
// memory footprint: writes past the limit are counted and dropped.
type boundedBuffer struct {
	limit   int
	buf     []byte
	dropped int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.dropped += len(p) - room
		}
	} else {
		b.dropped += len(p)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	if b.dropped > 0 {
		return string(b.buf) + fmt.Sprintf(" … (%d more bytes)", b.dropped)
	}
	return string(b.buf)
}
