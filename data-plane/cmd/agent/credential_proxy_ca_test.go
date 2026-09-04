package main

import (
	"crypto/tls"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The three outcomes providerCACertEnv and trustProviderCA promise, pinned
// against a real TLS handshake.

// tlsUpstreamCAPEM re-encodes an httptest TLS server's own certificate as the
// PEM a platform Secret would carry. httptest signs with an ephemeral CA whose
// leaf is self-signed for 127.0.0.1, so trusting that certificate is exactly
// what an operator does when they paste their gateway's issuer.
func tlsUpstreamCAPEM(t *testing.T, server *httptest.Server) string {
	t.Helper()
	certs := server.TLS.Certificates
	if len(certs) == 0 || len(certs[0].Certificate) == 0 {
		t.Fatalf("test upstream has no certificate")
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certs[0].Certificate[0]}))
}

func TestCredentialProxyRejectsUntrustedProviderCertificate(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy, err := newCredentialProxy(upstream.URL, "real-secret", "", testLogger())
	if err != nil {
		t.Fatalf("new credential proxy: %v", err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	// The proxy turns an unusable upstream into 502 rather than passing a
	// handshake failure through as a success.
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d — an unknown issuer must not be trusted", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestCredentialProxyTrustsConfiguredProviderCA(t *testing.T) {
	const body = "provider-reached"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer real-secret" {
			t.Errorf("upstream Authorization = %q, want the injected platform token", got)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	proxy, err := newCredentialProxy(upstream.URL, "real-secret", tlsUpstreamCAPEM(t, upstream), testLogger())
	if err != nil {
		t.Fatalf("new credential proxy: %v", err)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(got) != body {
		t.Fatalf("status=%d body=%q, want 200 %q", resp.StatusCode, got, body)
	}
}

func TestCredentialProxyRejectsCABundleWithoutCertificate(t *testing.T) {
	for name, bundle := range map[string]string{
		"prose":            "this is not a certificate",
		"empty PEM block":  "-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n",
		"unrelated object": "-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newCredentialProxy("https://gateway.example", "real-secret", bundle, testLogger())
			if err == nil {
				t.Fatalf("bundle %q was accepted", bundle)
			}
			if strings.Contains(err.Error(), "real-secret") {
				t.Fatalf("configuration error leaked the token: %v", err)
			}
		})
	}
}

// trustProviderCA's widen-never-weaken rule, asserted on the transport.
func TestProviderCAWidensTrustWithoutWeakeningIt(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	caPEM := tlsUpstreamCAPEM(t, upstream)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := trustProviderCA(transport, caPEM); err != nil {
		t.Fatalf("trust provider CA: %v", err)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatalf("transport roots were not configured")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("provider CA trust must not disable certificate verification")
	}

	// A transport that carries no TLS configuration of its own still gets a
	// floor: this branch is what a caller outside newCredentialProxy hits.
	bare := &http.Transport{}
	if err := trustProviderCA(bare, caPEM); err != nil {
		t.Fatalf("trust provider CA on a bare transport: %v", err)
	}
	if bare.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("bare transport MinVersion = %x, want TLS 1.2", bare.TLSClientConfig.MinVersion)
	}
	if bare.TLSClientConfig.RootCAs == nil {
		t.Fatalf("bare transport roots were not configured")
	}
}

func TestEmptyProviderCALeavesTrustAtTheSystemDefault(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if err := trustProviderCA(transport, ""); err != nil {
		t.Fatalf("empty bundle: %v", err)
	}
	// nil RootCAs is how the standard library says "use the system pool".
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.RootCAs != nil {
		t.Fatalf("roots = %+v, want the system pool", transport.TLSClientConfig.RootCAs)
	}
}
