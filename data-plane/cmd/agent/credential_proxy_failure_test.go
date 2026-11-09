package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An intermediate proxy that times a silent request out closes the connection
// without an HTTP response. The caller must read that as a closed connection
// with the time it took — not as a generic "unavailable" that looks like a
// credential or upstream outage.
func TestCredentialProxyReportsAClosedUpstreamConnection(t *testing.T) {
	const token = "closed-conn-secret"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream test server cannot hijack")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL, token, &logs, upstream.Client().Transport)

	resp, err := http.Post(proxy.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, http.StatusBadGateway, body)
	}
	if !strings.Contains(string(body), "credential proxy upstream closed the connection after") {
		t.Fatalf("body = %q, want it to name a closed connection and its elapsed time", body)
	}
	if !strings.Contains(logs.String(), `"failure":"connection_closed"`) || !strings.Contains(logs.String(), `"elapsed"`) {
		t.Fatalf("logs = %q, want the failure class and elapsed time", logs.String())
	}
	if strings.Contains(string(body), token) || strings.Contains(logs.String(), token) {
		t.Fatalf("credential leaked body=%q logs=%q", body, logs.String())
	}
}

// Nothing listening at the upstream is a connect failure, and says so.
func TestCredentialProxyReportsAnUnreachableUpstream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := "https://" + listener.Addr().String()
	_ = listener.Close()
	var logs bytes.Buffer
	proxy := credentialProxyServer(t, target, "unreachable-secret", &logs, nil)

	resp, err := http.Get(proxy.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "could not connect") {
		t.Fatalf("status = %d body = %q, want 502 naming a connect failure", resp.StatusCode, body)
	}
	if !strings.Contains(logs.String(), `"failure":"connect"`) {
		t.Fatalf("logs = %q, want failure=connect", logs.String())
	}
}

// A transport deadline is a timeout: 504, not 502.
func TestCredentialProxyReportsAnUpstreamTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer upstream.Close()
	defer close(release)
	transport := upstream.Client().Transport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	var logs bytes.Buffer
	proxy := credentialProxyServer(t, upstream.URL, "timeout-secret", &logs, transport)

	resp, err := http.Get(proxy.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout || !strings.Contains(string(body), "timed out after") {
		t.Fatalf("status = %d body = %q, want 504 naming a timeout", resp.StatusCode, body)
	}
}

func TestClassifyCredentialProxyFailure(t *testing.T) {
	started := time.Now().Add(-30 * time.Second)
	live := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	live = live.WithContext(context.WithValue(live.Context(), credentialProxyStartKey{}, started))
	goneCtx, cancel := context.WithCancel(context.WithValue(context.Background(), credentialProxyStartKey{}, started))
	cancel()
	gone := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(goneCtx)

	for _, tc := range []struct {
		name string
		req  *http.Request
		err  error
		want proxyFailureKind
	}{
		{"caller hung up", gone, context.Canceled, proxyFailureCallerGone},
		{"deadline", live, context.DeadlineExceeded, proxyFailureTimeout},
		{"eof", live, io.EOF, proxyFailureClosed},
		{"unexpected eof", live, io.ErrUnexpectedEOF, proxyFailureClosed},
		{"http2 connection lost", live, errString("http2: client connection lost"), proxyFailureClosed},
		{"http2 goaway", live, errString("http2: server sent GOAWAY and closed the connection"), proxyFailureClosed},
		{"dial", live, &net.OpError{Op: "dial", Err: errString("connection refused")}, proxyFailureConnect},
		{"tls", live, errString("tls: failed to verify certificate"), proxyFailureOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCredentialProxyFailure(tc.req, tc.err)
			if got.kind != tc.want {
				t.Fatalf("kind = %q, want %q", got.kind, tc.want)
			}
			if got.elapsed < 30*time.Second {
				t.Fatalf("elapsed = %s, want it measured from the request's start", got.elapsed)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
