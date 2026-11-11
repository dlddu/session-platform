package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

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
