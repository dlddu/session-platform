package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// claude-code's provider auth modes (docs/prd/claude-code-workload.md, AC-E7).
// gateway is AC-E6's loopback credential proxy; subscription signs the CLI in
// to a Claude subscription with the platform's long-lived OAuth token — the
// one `claude setup-token` prints — which the control plane projects from its
// Secret as CLAUDE_CODE_OAUTH_TOKEN.
const (
	// Keep in sync with control-plane/internal/adapter/k8s (the claude-code
	// auth mode, OAuth token and effort env).
	claudeAuthModeEnv          = "CLAUDE_CODE_AUTH_MODE"
	claudeAuthModeGateway      = "gateway"
	claudeAuthModeSubscription = "subscription"
	claudeOAuthTokenEnv        = "CLAUDE_CODE_OAUTH_TOKEN"
	claudeEffortEnv            = "CLAUDE_CODE_EFFORT"
)

// claudeEfforts is AC-E8's list for claude-code: the values the CLI's
// --effort accepts.
var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

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
// the CLI talks to the provider itself with the platform's OAuth token, so
// that token must be there, and a provider address or other credential in the
// environment would replace it with an account the platform did not choose.
func validateClaudeSubscriptionClientEnv() error {
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("claude-code subscription container must not receive %s", name)
		}
	}
	token := os.Getenv(claudeOAuthTokenEnv)
	if token == "" {
		return errors.New("claude-code subscription container needs " + claudeOAuthTokenEnv)
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return errors.New(claudeOAuthTokenEnv + " must be a single token without whitespace")
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
