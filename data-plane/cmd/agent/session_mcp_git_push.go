// `git_push` (docs/prd/repo-access.md AC-H6): the write half of the allow-list
// path `git_clone` opened. The workload pod has no egress, so a commit made in a
// clone on the shared volume can leave the session only through this tool.
//
// The gate is the allow list, as for `git_clone` in the same deployment — it is
// the third of R1's explicit exceptions (docs/session-mcp-tool-surface.md), and
// the tool is listed only where that list exists.
//
// The clone is the agent's: everything under the shared volume, its .git/config
// and hooks included, is written from the workload pod. So no git command that
// could act on that configuration runs with the write token in reach. The
// branch is first fetched into a scratch bare repository this container owns —
// fetching runs only upload-pack in the clone, which executes nothing from the
// repository's own configuration — and the push leaves from there, with hooks
// off, to the URL the allow list was asked about.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	gitPushTool = "git_push"
	// pushTimeout bounds the fetch into the scratch repository and the push
	// together, the same five minutes a clone gets.
	pushTimeout = 5 * time.Minute
)

// clonePathBase is what clonePath names a clone: the request id, nothing else.
var clonePathBase = regexp.MustCompile(`^` + requestIDPrefix + `[0-9a-f]{` + fmt.Sprint(requestIDBytes*2) + `}$`)

// pushBranchPattern is a conservative subset of git's ref-name rules, checked
// here so that a bad name never reaches the allow list or the token mint (R3).
var pushBranchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// pushRequest is everything the push seam needs; env carries the scoped auth
// header and nothing else secret.
type pushRequest struct {
	source string // the clone's working tree
	branch string
	remote string // the URL the allow list was asked about
	env    []string
}

type pushOutcome struct {
	commit string
}

// errPushRejected marks a push the remote refused as not a fast-forward.
var errPushRejected = errors.New("rejected by the remote")

func (c sessionMCPConfig) callGitPush(ctx context.Context, logger *slog.Logger, arguments json.RawMessage) (any, *jsonRPCError) {
	var args struct {
		Path   string `json:"path"`
		Branch string `json:"branch"`
	}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return mcpToolError("arguments must be an object with a path and a branch"), nil
		}
	}
	if c.repoAccess == nil {
		return mcpToolError("repo access is not configured on this deployment, so nothing can be pushed"), nil
	}
	if c.sharedDir == "" {
		return mcpToolError("no shared volume is configured for this session, so there is no clone to push from"), nil
	}
	branch, err := validatePushBranch(args.Branch)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	source, err := c.resolveClonePath(args.Path)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	origin, err := readCloneOrigin(ctx, source)
	if err != nil {
		return mcpToolError(err.Error()), nil
	}
	repo, ok := githubRepoOf(origin)
	if !ok {
		return mcpToolError(fmt.Sprintf("the clone's origin %q is not an https://github.com/<owner>/<repo> URL, "+
			"so it cannot be pushed here", truncateText(origin, 200))), nil
	}

	allowed, err := c.repoAccess.check(ctx, repo.fullName())
	if err != nil {
		logger.Error("allow list check failed", "tool", gitPushTool, "repo", repo.fullName(), "err", err)
		return mcpToolError("the allow list could not be checked, so nothing was pushed: " + err.Error()), nil
	}
	if !allowed {
		logger.Info("not on the allow list", "tool", gitPushTool, "repo", repo.fullName())
		return mcpToolError(fmt.Sprintf("repository %s is not on the allow list. Ask for it with %s; a person "+
			"decides in the control plane, and nothing is pushed until then.", repo.fullName(), requestRepoAccessTool)), nil
	}

	// No anonymous fallback, unlike git_clone: an anonymous push cannot succeed.
	if c.mintGitHubToken == nil {
		return mcpToolError(fmt.Sprintf("a write token could not be issued for %s (this helper pod has no K3s MCP proxy), "+
			"so nothing was pushed", repo.fullName())), nil
	}
	token, err := c.mintGitHubToken(ctx, repo.name, githubContentsWrite)
	if err != nil {
		logger.Error("write token could not be issued", "tool", gitPushTool, "repo", repo.fullName(), "err", err)
		return mcpToolError(fmt.Sprintf("a write token could not be issued for %s, so nothing was pushed: %s",
			repo.fullName(), err.Error())), nil
	}
	header, encoded := githubBasicAuthHeader(token)

	push := c.push
	if push == nil {
		push = pushRepository
	}
	logger.Info("on the allow list, pushing", "tool", gitPushTool, "repo", repo.fullName(), "branch", branch, "path", source)
	outcome, err := push(ctx, pushRequest{source: source, branch: branch, remote: repo.scope, env: scopedAuthEnv(repo.scope, header)})
	if err != nil {
		reason := redactSecrets(err.Error(), header, encoded, token)
		logger.Error("push failed", "tool", gitPushTool, "repo", repo.fullName(), "branch", branch, "err", reason)
		if errors.Is(err, errPushRejected) {
			return mcpToolError(fmt.Sprintf("push of %s to %s was rejected: %s. The remote has commits this clone "+
				"lacks; clone again with %s, re-apply the change and push from the new clone.",
				branch, repo.fullName(), reason, gitCloneTool)), nil
		}
		return mcpToolError(fmt.Sprintf("push of %s to %s failed: %s", branch, repo.fullName(), reason)), nil
	}

	payload, err := json.Marshal(map[string]any{
		"success": true,
		"message": "Branch pushed successfully",
		"path":    source,
		"url":     repo.scope,
		"branch":  branch,
		"commit":  outcome.commit,
	})
	if err != nil {
		return nil, &jsonRPCError{Code: jsonRPCInternalError, Message: "could not encode the tool result"}
	}
	logger.Info("pushed", "tool", gitPushTool, "repo", repo.fullName(), "branch", branch, "commit", outcome.commit)
	return mcpToolText(string(payload)), nil
}

func validatePushBranch(raw string) (string, error) {
	branch := strings.TrimSpace(raw)
	switch {
	case branch == "":
		return "", errors.New("branch is required")
	case len(branch) > maxCloneBranchChars:
		return "", fmt.Errorf("branch must be at most %d characters", maxCloneBranchChars)
	case !pushBranchPattern.MatchString(branch),
		strings.Contains(branch, ".."), strings.Contains(branch, "//"), strings.Contains(branch, "/."),
		strings.HasSuffix(branch, "/"), strings.HasSuffix(branch, "."), strings.HasSuffix(branch, ".lock"):
		return "", fmt.Errorf("branch %q is not a plain branch name (letters, digits, '.', '_', '-', '/')", truncateText(branch, 80))
	}
	return branch, nil
}

// resolveClonePath accepts only a directory git_clone itself made in this
// session: <shared>/git_clone/req-<hex>, a real directory (not a symlink, which
// the workload pod could plant to point elsewhere in this container) with a
// real .git directory in it.
func (c sessionMCPConfig) resolveClonePath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", errors.New("path is required — the path git_clone returned")
	}
	cloneRoot := filepath.Join(c.sharedDir, cloneSubdir)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		filepath.Dir(path) != cloneRoot || !clonePathBase.MatchString(filepath.Base(path)) {
		return "", fmt.Errorf("path must be a clone directory %s returned (%s/%s<id>)", gitCloneTool, cloneRoot, requestIDPrefix)
	}
	for _, p := range []string{path, filepath.Join(path, ".git")} {
		info, err := os.Lstat(p)
		if err != nil {
			return "", fmt.Errorf("no clone at %s", path)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%s is not a plain directory, so it is not pushed from", p)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(cloneRoot)
	if err != nil {
		return "", errors.New("the clone directory could not be resolved")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != filepath.Join(resolvedRoot, filepath.Base(path)) {
		return "", fmt.Errorf("%s does not resolve inside the clone directory", path)
	}
	return path, nil
}

// readCloneOrigin reads remote.origin.url from the clone's config file as a
// plain file: --file reads that one file and follows no includes, so nothing
// the agent wrote into it is executed.
func readCloneOrigin(ctx context.Context, source string) (string, error) {
	configPath := filepath.Join(source, ".git", "config")
	if info, err := os.Lstat(configPath); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("the clone has no regular .git/config to read its origin from")
	}
	cmd := exec.CommandContext(ctx, gitBin, "config", "--file", configPath, "--get", "remote.origin.url")
	cmd.Env = pushGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("the clone has no single remote.origin.url")
	}
	return strings.TrimSpace(string(out)), nil
}

// pushGitEnv keeps every config source but the command line and the scratch
// repository's own out of reach, and every prompt off.
func pushGitEnv(extra ...string) []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	return append(env, extra...)
}

// pushRepository is the real push, and the seam sessionMCPConfig.push replaces
// in tests. See this file's header for why it goes through a scratch repository.
func pushRepository(ctx context.Context, req pushRequest) (pushOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	scratch, err := os.MkdirTemp("", "git-push-")
	if err != nil {
		return pushOutcome{}, fmt.Errorf("prepare a scratch repository: %w", err)
	}
	defer os.RemoveAll(scratch)

	ref := "refs/heads/" + req.branch
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, gitBin, args...)
		cmd.Env = env
		var stdout bytes.Buffer
		stderr := &boundedBuffer{limit: maxCloneErrorBytes}
		cmd.Stdout = &stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("git push did not finish within %s", pushTimeout)
			}
			detail := strings.TrimSpace(strings.TrimSpace(stdout.String()) + "\n" + stderr.String())
			if detail != "" {
				return stdout.String(), fmt.Errorf("%w: %s", err, truncateText(detail, maxCloneErrorBytes))
			}
			return stdout.String(), err
		}
		return stdout.String(), nil
	}

	plain := pushGitEnv()
	if _, err := run(plain, "init", "--bare", "-q", scratch); err != nil {
		return pushOutcome{}, fmt.Errorf("prepare a scratch repository: %w", err)
	}
	ownScratch := "safe.directory=" + scratch
	// The clone is owned by another uid (EFS forces 50000), so upload-pack in it
	// needs safe.directory — trusting the agent's repo there is an accepted risk
	// (docs/prd/repo-access.md, 2026-10-25). It rides on --upload-pack because
	// git drops -c (GIT_CONFIG_PARAMETERS) when it spawns a local transport.
	if _, err := run(plain, "-C", scratch,
		"-c", ownScratch, "-c", "protocol.file.allow=always", "-c", "credential.helper=",
		"fetch", "--no-tags", "-q", "--upload-pack="+gitBin+" -c safe.directory='*' upload-pack",
		"--", filepath.Join(req.source, ".git"), ref+":"+ref); err != nil {
		return pushOutcome{}, fmt.Errorf("branch %s could not be read from the clone: %w", req.branch, err)
	}
	commit, err := run(plain, "-C", scratch, "-c", ownScratch, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil {
		return pushOutcome{}, fmt.Errorf("branch %s has no commit in the clone", req.branch)
	}
	// No leading '+' on the refspec: a non-fast-forward is refused, never forced.
	out, err := run(pushGitEnv(req.env...), "-C", scratch, "-c", ownScratch,
		"-c", "credential.helper=", "-c", "core.hooksPath="+os.DevNull,
		"push", "--no-verify", "--porcelain", "--", req.remote, ref+":"+ref)
	if err != nil {
		if strings.Contains(out, "[rejected]") || strings.Contains(err.Error(), "non-fast-forward") ||
			strings.Contains(err.Error(), "fetch first") {
			return pushOutcome{}, fmt.Errorf("%w: %v", errPushRejected, err)
		}
		return pushOutcome{}, err
	}
	return pushOutcome{commit: strings.TrimSpace(commit)}, nil
}
