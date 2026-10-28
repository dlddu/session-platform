//go:build e2e

// The approver's side of the approval gate, played against the real PocketAide
// that deploy/e2e-pocket-aide.yaml stands. Everything here goes through the
// same user API the app calls; nothing reaches into PocketAide's store.
package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	pocketAideNamespace = "pocket-aide"
	pocketAidePort      = 8080
	oidcMockNamespace   = "oidc-mock"
	oidcMockPort        = 5556
	// Must match oidc-mock's -client-id and PocketAide's OIDC_REDIRECT_URI. The
	// redirect is never followed; only the code in its Location is read.
	oidcClientID    = "session-platform-e2e"
	oidcRedirectURI = "http://127.0.0.1/e2e-callback"
	// approverSubject is the PocketAide user that registers the helper pods'
	// ServiceAccount as a caller and so owns, and decides, every request.
	approverSubject = "e2e-approver"
	// The helper pod's identity — client_orchestrator.go's
	// SessionMCPServiceAccountName, written out for the reason e2e_f1 gives.
	sessionMCPServiceAccount = "session-mcp"
	// Also client_orchestrator.go's, same reason: ApprovalGatewayTokenAudience
	// and the token file the session MCP container reads.
	approvalGatewayAudience  = "pocket-aide"
	approvalGatewayTokenFile = "/var/run/secrets/pocket-aide/token"
)

// pocketAideApprover is a logged-in approver with a local route to PocketAide.
type pocketAideApprover struct {
	base   string
	token  string
	client *http.Client
}

// pocketAideRequest is one row of PocketAide's user API (the app's shape).
type pocketAideRequest struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	ExternalID    string `json:"external_id"`
	Context       string `json:"context"`
	RequesterName string `json:"requester_name"`
	KeyName       string `json:"key_name"`
}

var registerCallerOnce sync.Once

// newPocketAideApprover logs in as approverSubject and makes sure the helper
// pods' ServiceAccount is registered under that user. Registration is done once
// per run; PocketAide's store is an emptyDir, so a fresh SUT has none.
func newPocketAideApprover(t *testing.T, cs kubernetes.Interface, cfg *rest.Config) *pocketAideApprover {
	t.Helper()
	oidc := portForwardApp(t, cs, cfg, oidcMockNamespace, "oidc-mock", oidcMockPort)
	a := &pocketAideApprover{
		base:   portForwardApp(t, cs, cfg, pocketAideNamespace, "pocket-aide", pocketAidePort),
		token:  mintOIDCToken(t, oidc, approverSubject),
		client: &http.Client{Timeout: 20 * time.Second},
	}
	var regErr error
	registerCallerOnce.Do(func() { regErr = a.registerCaller(sessionNamespace() + "/" + sessionMCPServiceAccount) })
	if regErr != nil {
		t.Fatalf("register the session MCP ServiceAccount in PocketAide: %v", regErr)
	}
	return a
}

func (a *pocketAideApprover) call(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// registerCaller is what the approver does once in the app's 「승인 게이트 설정」.
// A 409 is accepted only if the account is already this user's, unrevoked.
func (a *pocketAideApprover) registerCaller(account string) error {
	status, raw, err := a.call(http.MethodPost, "/api/approval-keys", map[string]string{
		"name": "e2e " + account, "kind": "k8s_sa", "service_account": account,
	})
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		return nil
	}
	if status != http.StatusConflict {
		return fmt.Errorf("POST /api/approval-keys: %d %s", status, raw)
	}
	status, raw, err = a.call(http.MethodGet, "/api/approval-keys", nil)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("GET /api/approval-keys: %d %s %v", status, raw, err)
	}
	var keys struct {
		Keys []struct {
			ServiceAccount string `json:"service_account"`
			RevokedAt      *int64 `json:"revoked_at"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	for _, k := range keys.Keys {
		if k.ServiceAccount == account && k.RevokedAt == nil {
			return nil
		}
	}
	return fmt.Errorf("%s is registered, but not to %s", account, approverSubject)
}

// pending is the approver's open requests — what the app's 승인 tab lists.
func (a *pocketAideApprover) pending(t *testing.T) []pocketAideRequest {
	t.Helper()
	status, raw, err := a.call(http.MethodGet, "/api/approvals", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /api/approvals: status=%d body=%s err=%v", status, raw, err)
	}
	var page struct {
		Items []pocketAideRequest `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode /api/approvals: %v body=%s", err, raw)
	}
	return page.Items
}

// pendingByExternalID returns ok=false while the request has not been created
// yet — until the CLI reaches the tool there is legitimately no record.
func (a *pocketAideApprover) pendingByExternalID(t *testing.T, externalID string) (pocketAideRequest, bool) {
	t.Helper()
	for _, r := range a.pending(t) {
		if r.ExternalID == externalID {
			return r, true
		}
	}
	return pocketAideRequest{}, false
}

func (a *pocketAideApprover) get(t *testing.T, id string) pocketAideRequest {
	t.Helper()
	status, raw, err := a.call(http.MethodGet, "/api/approvals/"+url.PathEscape(id), nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /api/approvals/%s: status=%d body=%s err=%v", id, status, raw, err)
	}
	var r pocketAideRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode /api/approvals/%s: %v body=%s", id, err, raw)
	}
	return r
}

// approve is the approver pressing 승인.
func (a *pocketAideApprover) approve(t *testing.T, id string) {
	t.Helper()
	status, raw, err := a.call(http.MethodPost, "/api/approvals/"+url.PathEscape(id)+"/approve", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("approve %s: status=%d body=%s err=%v", id, status, raw, err)
	}
}

// mintOIDCToken runs the PKCE authorization-code flow oidc-mock serves, with
// login_hint choosing the subject in place of a human login.
func mintOIDCToken(t *testing.T, oidc, subject string) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	noRedirect := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {oidcClientID},
		"redirect_uri":          {oidcRedirectURI},
		"scope":                 {"openid"},
		"state":                 {subject},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"login_hint":            {subject},
	}
	resp, err := noRedirect.Get(oidc + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("oidc-mock /authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("oidc-mock /authorize: status=%d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("oidc-mock /authorize redirected to %q, with no code", resp.Header.Get("Location"))
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {loc.Query().Get("code")},
		"code_verifier": {verifier},
		"client_id":     {oidcClientID},
		"redirect_uri":  {oidcRedirectURI},
	}
	tokenResp, err := noRedirect.PostForm(oidc+"/token", form)
	if err != nil {
		t.Fatalf("oidc-mock /token: %v", err)
	}
	defer tokenResp.Body.Close()
	raw, _ := io.ReadAll(tokenResp.Body)
	if tokenResp.StatusCode != http.StatusOK {
		t.Fatalf("oidc-mock /token: status=%d body=%s", tokenResp.StatusCode, raw)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("oidc-mock /token returned no access token: %s", raw)
	}
	return tok.AccessToken
}

// portForwardApp forwards a random local port to a Ready pod labelled app=<app>
// for the rest of the test. The fixtures are ClusterIP-only on purpose; this is
// the runner's route in, the way an approver's phone has its own.
func portForwardApp(t *testing.T, cs kubernetes.Interface, cfg *rest.Config, ns, app string, port int) string {
	t.Helper()
	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		t.Fatalf("list %s pods in %s: %v", app, ns, err)
	}
	pod := ""
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil && podReady(p) {
			pod = p.Name
			break
		}
	}
	if pod == "" {
		t.Fatalf("no Ready %s pod in %s", app, ns)
	}
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		t.Fatalf("port-forward transport: %v", err)
	}
	target := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, target)
	stop, ready := make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("port-forward %s/%s: %v", ns, pod, err)
	}
	errs := make(chan error, 1)
	go func() { errs <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-errs:
		t.Fatalf("port-forward %s/%s: %v", ns, pod, err)
	case <-time.After(30 * time.Second):
		close(stop)
		t.Fatalf("port-forward %s/%s did not become ready", ns, pod)
	}
	t.Cleanup(func() { close(stop) })
	ports, err := fw.GetPorts()
	if err != nil || len(ports) != 1 {
		t.Fatalf("port-forward %s/%s ports: %v %v", ns, pod, ports, err)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// jwtClaims reads a token's payload without verifying it; the verification is
// PocketAide's TokenReview, which the approval round trip exercises for real.
func jwtClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		t.Fatalf("token is not a JWT (%d parts)", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parse JWT payload: %v", err)
	}
	return claims
}
