package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestClaudeArgvCarriesEffort(t *testing.T) {
	got := (claudeCLI{effort: "max"}).argv(defaultClaudeRunnerBinary, "opus", "p", true)
	want := []string{defaultClaudeRunnerBinary, "--model", "opus", "--effort", "max", "--resume", "--", "p"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if got := (claudeCLI{}).argv(defaultClaudeRunnerBinary, platformDefaultModel, "p", false); slicesContain(got, "--effort") {
		t.Fatalf("no effort means no flag: %q", got)
	}
}

// AC-E2: a write's permission mode comes before the prompt delimiter, and a
// turn whose write chose none carries no flag, so the runner starts it in auto.
func TestClaudeArgvCarriesPermissionMode(t *testing.T) {
	got := (claudeCLI{effort: "max"}).argvWithPermissionMode(defaultClaudeRunnerBinary, "opus", "p", true, "acceptEdits")
	want := []string{
		defaultClaudeRunnerBinary, "--model", "opus", "--effort", "max",
		"--permission-mode", "acceptEdits", "--resume", "--", "p",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if got := (claudeCLI{}).argv(defaultClaudeRunnerBinary, platformDefaultModel, "p", false); slicesContain(got, "--permission-mode") {
		t.Fatalf("no mode means no flag: %q", got)
	}
	for mode, want := range map[string]bool{
		"auto": true, "acceptEdits": true,
		"": false, "default": false, "plan": false, "dontAsk": false, "bypassPermissions": false, "AUTO": false,
	} {
		if got := (claudeCLI{}).validPermissionMode(mode); got != want {
			t.Fatalf("validPermissionMode(%q) = %v, want %v", mode, got, want)
		}
	}
}

func setClaudeGatewayEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", claudeProxyBaseURL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", claudeProxyPlaceholderToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv(claudeOAuthTokenEnv, "")
}

// AC-E7/E8's start rules.
func TestClaudeAuthFromEnv(t *testing.T) {
	t.Setenv(claudeEffortEnv, "")

	setClaudeGatewayEnv(t)
	t.Setenv(claudeAuthModeEnv, "")
	auth, err := claudeAuthFromEnv(workloadClaudeCode)
	if err != nil || auth.mode != claudeAuthModeGateway || len(auth.tools.Plugins) != 0 {
		t.Fatalf("gateway = %+v, %v", auth, err)
	}
	t.Setenv(claudeOAuthTokenEnv, "sk-ant-oat01-x")
	if _, err := claudeAuthFromEnv(workloadClaudeCode); err == nil {
		t.Fatal("the gateway must refuse a direct OAuth token")
	}
	t.Setenv(claudeOAuthTokenEnv, "")

	t.Setenv(claudeAuthModeEnv, "sso")
	if _, err := claudeAuthFromEnv(workloadClaudeCode); err == nil {
		t.Fatal("an unknown mode must be refused")
	}

	t.Setenv(claudeAuthModeEnv, claudeAuthModeSubscription)
	t.Setenv(claudeOAuthTokenEnv, "sk-ant-oat01-x")
	if _, err := claudeAuthFromEnv(workloadClaudeCode); err == nil {
		t.Fatal("subscription must refuse the gateway's provider env")
	}
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api")
	if _, err := claudeAuthFromEnv(workloadClaudeCode); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err = %v, want the API key named", err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")

	t.Setenv(claudeOAuthTokenEnv, "")
	if _, err := claudeAuthFromEnv(workloadClaudeCode); err == nil || !strings.Contains(err.Error(), claudeOAuthTokenEnv) {
		t.Fatalf("err = %v, want the missing OAuth token named", err)
	}
	for _, bad := range []string{"sk-ant-oat01-x\n", " sk-ant-oat01-x", "sk-ant oat"} {
		t.Setenv(claudeOAuthTokenEnv, bad)
		if _, err := claudeAuthFromEnv(workloadClaudeCode); err == nil {
			t.Fatalf("token %q with whitespace accepted", bad)
		} else if strings.Contains(err.Error(), "sk-ant") {
			t.Fatalf("the refusal echoes the token: %v", err)
		}
	}

	t.Setenv(claudeOAuthTokenEnv, "sk-ant-oat01-x")
	t.Setenv(claudeEffortEnv, "xhigh")
	auth, err = claudeAuthFromEnv(workloadClaudeCode)
	if err != nil || auth.mode != claudeAuthModeSubscription || auth.effort != "xhigh" || len(auth.tools.Plugins) != 0 {
		t.Fatalf("subscription = %+v, %v", auth, err)
	}
	if !slices.Contains(credentialLiteralsFromEnv(), "sk-ant-oat01-x") {
		t.Fatal("the OAuth token must be redacted from session output")
	}
	t.Setenv(claudeEffortEnv, "minimal")
	if _, err := claudeAuthFromEnv(workloadClaudeCode); err == nil {
		t.Fatal("minimal is not a claude-code effort")
	}
}
