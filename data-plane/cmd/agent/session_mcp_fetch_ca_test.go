package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The three outcomes sessionMCPCACertEnv promises for the approved outbound
// call, pinned against a real TLS handshake. They are deliberately the same
// three the credential proxy makes (credential_proxy_ca_test.go): one rule,
// two trust domains.

func TestApprovedFetchRejectsUntrustedOriginCertificate(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()

	fetcher, err := newApprovedFetcher("")
	if err != nil {
		t.Fatalf("new approved fetcher: %v", err)
	}
	// Without the key, an origin issued by a CA no public store knows is
	// exactly what the gate lets through and the call still cannot complete.
	if _, err := fetcher.fetch(context.Background(), origin.URL); err == nil {
		t.Fatal("fetch succeeded against an unknown issuer; the system pool must not trust it")
	}
}

func TestApprovedFetchTrustsConfiguredCA(t *testing.T) {
	const body = "origin-reached"
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer origin.Close()

	fetcher, err := newApprovedFetcher(tlsUpstreamCAPEM(t, origin))
	if err != nil {
		t.Fatalf("new approved fetcher: %v", err)
	}
	resp, err := fetcher.fetch(context.Background(), origin.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}

	// Widening trust must not disable it. The widen-never-weaken rule itself
	// lives in trustExtraCA and is pinned there; what this asserts is that the
	// fetcher reaches the origin through that rule rather than around it.
	transport, ok := fetcher.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("fetcher transport = %T, want *http.Transport", fetcher.client.Transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("fetcher roots were not configured")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("trust anchor must not disable certificate verification")
	}
}

func TestApprovedFetcherRejectsBundleWithoutCertificate(t *testing.T) {
	if _, err := newApprovedFetcher("-----BEGIN CERTIFICATE-----\nnot base64\n-----END CERTIFICATE-----\n"); err == nil {
		t.Fatal("a non-empty bundle that parses to no certificate must fail loudly, not fall back to system trust")
	} else if !strings.Contains(err.Error(), "session MCP") {
		t.Errorf("error = %q, want the session MCP named as the subject", err)
	}
}

// The container refuses to start on that error rather than serving a gate whose
// approved calls would all fail — newSessionMCPConfig is where main.go finds out.
func TestNewSessionMCPConfigPropagatesTrustAnchorError(t *testing.T) {
	if _, err := newSessionMCPConfig(nil, "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----\n"); err == nil {
		t.Fatal("config was built from an unusable trust anchor")
	}
	config, err := newSessionMCPConfig(nil, "")
	if err != nil {
		t.Fatalf("empty trust anchor must keep the system pool: %v", err)
	}
	if config.fetch == nil {
		t.Error("config has no fetch")
	}
}
