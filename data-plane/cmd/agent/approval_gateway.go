// The approval gateway client (AC-F3). Every external tool the session MCP
// offers has to come through here first.
//
// The gateway is PocketAide's external approval API (dlddu/pocket-aide,
// PRD-12). The caller authenticates with a Kubernetes ServiceAccount token
// that PocketAide verifies through TokenReview and maps to the caller the
// approver registered in the app.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	defaultApprovalPollInterval = 3 * time.Second
	defaultApprovalTimeout      = 10 * time.Minute
	approvalRequestsPath        = "/api/external/approvals"
	approvalRequesterName       = "session-platform"
	// defaultApprovalTokenFile is where the control plane projects the helper
	// pod's ServiceAccount token for the gateway's audience.
	defaultApprovalTokenFile = "/var/run/secrets/pocket-aide/token"
	// maxApprovalResponseBytes bounds a gateway response. The bodies are two
	// small JSON objects; anything larger is a misconfigured endpoint.
	maxApprovalResponseBytes = 1 << 20
)

// approvalGateway talks to the external gateway on behalf of one session. It is
// created once at container start and holds no per-call state.
type approvalGateway struct {
	baseURL      string
	tokenFile    string
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

// newApprovalGateway builds the gate from the MCP container's environment. A
// gate that cannot reach the gateway must not be built, because the caller's
// fallback for "no gate" is to offer no tool at all rather than to let a call
// through ungated — so the token has to be readable now, not only later.
func newApprovalGateway(baseURL, tokenFile, sessionID string) (*approvalGateway, error) {
	missing := make([]string, 0, 2)
	for _, f := range []struct{ name, value string }{
		{approvalGatewayURLEnv, baseURL},
		{sessionIDEnv, sessionID},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("approval gateway is not configured: %s", strings.Join(missing, ", "))
	}
	if strings.TrimSpace(tokenFile) == "" {
		tokenFile = defaultApprovalTokenFile
	}
	if _, err := readApprovalToken(tokenFile); err != nil {
		return nil, err
	}
	return &approvalGateway{
		baseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		tokenFile:    tokenFile,
		sessionID:    sessionID,
		pollInterval: defaultApprovalPollInterval,
		timeout:      defaultApprovalTimeout,
		client:       &http.Client{Timeout: 30 * time.Second},
		now:          time.Now,
		sleep:        sleepContext,
	}, nil
}

// readApprovalToken is called per request: the kubelet rotates a projected
// token in place, so a copy taken at start goes stale within the hour.
func readApprovalToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("approval gateway service account token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("approval gateway service account token %s is empty", path)
	}
	return token, nil
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
	created, err := g.create(ctx, g.externalID(requestID), approvalContext)
	if err != nil {
		return approvalOutcome{}, err
	}
	// PocketAide applies the approver's auto-response mode only at creation, so
	// a request can come back already decided and will never change after.
	if decision, settled := settledDecision(created.Status); settled {
		return approvalOutcome{Decision: decision, RequestID: created.ID}, nil
	}
	deadline := g.now().Add(g.timeout)
	for {
		decision, settled, err := g.poll(ctx, created.ID)
		if err != nil {
			return approvalOutcome{}, err
		}
		if settled {
			return approvalOutcome{Decision: decision, RequestID: created.ID}, nil
		}
		if !g.now().Before(deadline) {
			return approvalOutcome{Decision: approvalTimeout, RequestID: created.ID}, nil
		}
		if err := g.sleep(ctx, g.pollInterval); err != nil {
			return approvalOutcome{}, err
		}
	}
}

type approvalState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// settledDecision reports settled=false for PENDING and for anything it does
// not recognise, so an unfamiliar interim status keeps the caller waiting
// rather than being read as a decision.
func settledDecision(status string) (approvalDecision, bool) {
	switch approvalDecision(status) {
	case approvalApproved, approvalRejected, approvalExpired:
		return approvalDecision(status), true
	default:
		return "", false
	}
}

// create opens the request a human will decide on. The context string is what
// that human sees, and carries no credential material (AC-F6). The gateway's
// own expiry is set to our wait so neither side keeps a request the other has
// given up on.
func (g *approvalGateway) create(ctx context.Context, externalID, approvalContext string) (approvalState, error) {
	body, err := json.Marshal(map[string]any{
		"externalId":     externalID,
		"context":        approvalContext,
		"requesterName":  approvalRequesterName,
		"timeoutSeconds": int64(g.timeout / time.Second),
	})
	if err != nil {
		return approvalState{}, fmt.Errorf("encode approval request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+approvalRequestsPath, bytes.NewReader(body))
	if err != nil {
		return approvalState{}, fmt.Errorf("build approval request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	state, err := g.do(req, "create approval request")
	if err != nil {
		return approvalState{}, err
	}
	if state.ID == "" {
		return approvalState{}, errors.New("approval gateway returned a request without an id")
	}
	return state, nil
}

// poll reads the request's current status.
func (g *approvalGateway) poll(ctx context.Context, gatewayRequestID string) (approvalDecision, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		g.baseURL+approvalRequestsPath+"/"+url.PathEscape(gatewayRequestID), nil)
	if err != nil {
		return "", false, fmt.Errorf("build approval poll: %w", err)
	}
	state, err := g.do(req, "poll approval request")
	if err != nil {
		return "", false, err
	}
	decision, settled := settledDecision(state.Status)
	return decision, settled, nil
}

// do authenticates and runs one gateway call. Any non-2xx is an error about the
// gateway, never a decision: failing to ask is not the same as being told no,
// and only the second one may be reported to the model as a refusal.
func (g *approvalGateway) do(req *http.Request, what string) (approvalState, error) {
	token, err := readApprovalToken(g.tokenFile)
	if err != nil {
		return approvalState{}, fmt.Errorf("%s: %w", what, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := g.client.Do(req)
	if err != nil {
		return approvalState{}, fmt.Errorf("%s: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxApprovalResponseBytes))
	if err != nil {
		return approvalState{}, fmt.Errorf("%s: read response: %w", what, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return approvalState{}, fmt.Errorf("%s: gateway returned 401 — is this pod's service account registered as a caller in PocketAide?", what)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return approvalState{}, fmt.Errorf("%s: gateway returned %d", what, resp.StatusCode)
	}
	var state approvalState
	if err := json.Unmarshal(body, &state); err != nil {
		return approvalState{}, fmt.Errorf("%s: gateway returned a non-JSON body", what)
	}
	return state, nil
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
