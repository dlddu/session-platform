// The approval gateway client (AC-F3). Every external tool the session MCP
// offers has to come through here first.
//
// The wire contract is not invented here: it is the one the PRD's named
// reference implementation speaks (dlddu/pure-agent,
// mcp-server/src/services/gatekeeper.ts). Keeping it identical is what lets one
// gateway serve both.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// approvalDecision is what the gate settles on. PENDING is deliberately absent:
// it is a state of the request, not an outcome of the wait.
type approvalDecision string

const (
	approvalApproved approvalDecision = "APPROVED"
	approvalRejected approvalDecision = "REJECTED"
	approvalExpired  approvalDecision = "EXPIRED"
	// approvalTimeout is ours, not the gateway's: it is what we call the wait
	// when we stop waiting before the gateway decides anything.
	approvalTimeout approvalDecision = "TIMEOUT"
)

const (
	// The reference implementation's defaults. Approval is a human round trip,
	// so the timeout is minutes, not seconds.
	defaultApprovalPollInterval = 3 * time.Second
	defaultApprovalTimeout      = 10 * time.Minute
	approvalRequestsPath        = "/api/requests"
	// approvalAPIKeyHeader carries the gateway key. It never goes in a body, so
	// it cannot end up in the approval context a human reads (AC-F6).
	approvalAPIKeyHeader = "x-api-key"
	// approvalRequesterName identifies this platform to the gateway operator.
	approvalRequesterName = "session-platform"
	// maxApprovalResponseBytes bounds a gateway response. The bodies are two
	// small JSON objects; anything larger is a misconfigured endpoint.
	maxApprovalResponseBytes = 1 << 20
)

// approvalGateway talks to the external gateway on behalf of one session. It is
// created once at container start and holds no per-call state.
type approvalGateway struct {
	baseURL      string
	apiKey       string
	userID       string
	sessionID    string
	pollInterval time.Duration
	timeout      time.Duration
	client       *http.Client
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
}

// approvalOutcome is a settled wait. RequestID is the gateway's own id for the
// request, kept for logs and for the caller's failure message.
type approvalOutcome struct {
	Decision  approvalDecision
	RequestID string
}

// newApprovalGateway builds the gate from the MCP container's environment. Every
// field is required: a gate that cannot reach the gateway must not be built,
// because the caller's fallback for "no gate" is to offer no tool at all rather
// than to let a call through ungated.
func newApprovalGateway(baseURL, apiKey, userID, sessionID string) (*approvalGateway, error) {
	missing := make([]string, 0, 4)
	for _, f := range []struct{ name, value string }{
		{approvalGatewayURLEnv, baseURL},
		{approvalGatewayAPIKeyEnv, apiKey},
		{approvalGatewayUserIDEnv, userID},
		{sessionIDEnv, sessionID},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("approval gateway is not configured: %s", strings.Join(missing, ", "))
	}
	return &approvalGateway{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		userID:       userID,
		sessionID:    sessionID,
		pollInterval: defaultApprovalPollInterval,
		timeout:      defaultApprovalTimeout,
		client:       &http.Client{},
		now:          time.Now,
		sleep:        sleepContext,
	}, nil
}

// externalID is the identifier the gateway dedupes on: `{sessionID}:{requestID}`
// exactly as AC-F3 specifies it.
func (g *approvalGateway) externalID(requestID string) string {
	return g.sessionID + ":" + requestID
}

// await creates an approval request and blocks until it is decided, the wait
// times out, or ctx is cancelled. It returns an error only when the gateway
// itself failed — a REJECTED or EXPIRED decision is a successful wait with an
// unfavourable answer, and the caller must be able to tell those apart.
//
// Blocking is the point rather than a limitation — see AC-F3's 대기 위치.
func (g *approvalGateway) await(ctx context.Context, requestID, approvalContext string) (approvalOutcome, error) {
	gatewayRequestID, err := g.create(ctx, g.externalID(requestID), approvalContext)
	if err != nil {
		return approvalOutcome{}, err
	}
	deadline := g.now().Add(g.timeout)
	for {
		decision, settled, err := g.poll(ctx, gatewayRequestID)
		if err != nil {
			return approvalOutcome{}, err
		}
		if settled {
			return approvalOutcome{Decision: decision, RequestID: gatewayRequestID}, nil
		}
		if !g.now().Before(deadline) {
			return approvalOutcome{Decision: approvalTimeout, RequestID: gatewayRequestID}, nil
		}
		if err := g.sleep(ctx, g.pollInterval); err != nil {
			return approvalOutcome{}, err
		}
	}
}

// create opens the request a human will decide on. The context string is what
// that human sees, and carries no credential material (AC-F6).
func (g *approvalGateway) create(ctx context.Context, externalID, approvalContext string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"externalId":    externalID,
		"context":       approvalContext,
		"requesterName": approvalRequesterName,
		"userId":        g.userID,
	})
	if err != nil {
		return "", fmt.Errorf("encode approval request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+approvalRequestsPath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build approval request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(approvalAPIKeyHeader, g.apiKey)

	decoded, err := g.do(req, "create approval request")
	if err != nil {
		return "", err
	}
	id, _ := decoded["id"].(string)
	if id == "" {
		return "", errors.New("approval gateway returned a request without an id")
	}
	return id, nil
}

// poll reads the request's current status. It reports settled=false while the
// gateway still says PENDING (or anything else it has not decided), so an
// unfamiliar interim status keeps the caller waiting rather than being read as
// a decision.
func (g *approvalGateway) poll(ctx context.Context, gatewayRequestID string) (approvalDecision, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		g.baseURL+approvalRequestsPath+"/"+gatewayRequestID, nil)
	if err != nil {
		return "", false, fmt.Errorf("build approval poll: %w", err)
	}
	req.Header.Set(approvalAPIKeyHeader, g.apiKey)

	decoded, err := g.do(req, "poll approval request")
	if err != nil {
		return "", false, err
	}
	status, _ := decoded["status"].(string)
	switch approvalDecision(status) {
	case approvalApproved, approvalRejected, approvalExpired:
		return approvalDecision(status), true, nil
	default:
		return "", false, nil
	}
}

// do runs one gateway call and decodes its JSON body. Any non-2xx is an error
// about the gateway, never a decision: failing to ask is not the same as being
// told no, and only the second one may be reported to the model as a refusal.
func (g *approvalGateway) do(req *http.Request, what string) (map[string]any, error) {
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxApprovalResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", what, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s: gateway returned %d", what, resp.StatusCode)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("%s: gateway returned a non-JSON body", what)
	}
	return decoded, nil
}

// sleepContext waits out d unless ctx ends first. A cancelled wait returns the
// context's error, which is how a torn-down pod stops a pending approval.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
