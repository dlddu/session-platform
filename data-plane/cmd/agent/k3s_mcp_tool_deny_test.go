package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestK3SMCPToolDeniedMatchesTheReconcilerLocalList(t *testing.T) {
	for _, name := range []string{
		"resource_create", "resource_update", "resource_patch", "resource_delete",
		"resource_delete_collection", "resource_exec", "resource_attach",
		"resource_port_forward", "resource_proxy", "opensearch_search",
		"opensearch_document_put", "session_create", "session_write",
		"dear_baby_reset_user", "github_commit_status_create",
	} {
		if !k3sMCPToolDenied(name) {
			t.Errorf("k3sMCPToolDenied(%q) = false, want denied", name)
		}
	}
	for _, name := range []string{
		"resource_get", "resource_list", "resource_watch", "api_resources", "ping",
		"github_app_installation_token", "grafana_token", "aws_config_get", "",
	} {
		if k3sMCPToolDenied(name) {
			t.Errorf("k3sMCPToolDenied(%q) = true, want allowed", name)
		}
	}
}

func newDenyTestProxy(t *testing.T) (http.Handler, *[]string) {
	t.Helper()
	var forwarded []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		forwarded = append(forwarded, string(data))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(upstream.Close)
	handler, err := newK3SMCPProxyWithTransport(upstream.URL+"/mcp", "k3s-token", "",
		slog.New(slog.NewTextHandler(io.Discard, nil)), upstream.Client().Transport)
	if err != nil {
		t.Fatalf("newK3SMCPProxyWithTransport: %v", err)
	}
	return handler, &forwarded
}

func postK3SMCP(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// The denial must never reach the upstream: the proxy is what holds the
// bearer, so a forwarded call would run with the platform's token.
func TestK3SMCPProxyAnswersDeniedToolCallWithoutForwarding(t *testing.T) {
	handler, forwarded := newDenyTestProxy(t)
	recorder := postK3SMCP(handler, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"resource_delete","arguments":{}}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("denied call = %d %q, want 200 with a tool error", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ID     int `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("denied call body %q: %v", recorder.Body.String(), err)
	}
	if response.ID != 7 || !response.Result.IsError || len(response.Result.Content) != 1 ||
		!strings.Contains(response.Result.Content[0].Text, "resource_delete") {
		t.Fatalf("denied call response = %+v, want an isError result naming the tool for id 7", response)
	}
	if len(*forwarded) != 0 {
		t.Fatalf("upstream received %q for a denied tool", *forwarded)
	}
}

func TestK3SMCPProxyForwardsAllowedCallsUnchanged(t *testing.T) {
	handler, forwarded := newDenyTestProxy(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"resource_get","arguments":{"kind":"Pod"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	} {
		if recorder := postK3SMCP(handler, body); recorder.Code != http.StatusOK {
			t.Fatalf("allowed request %s = %d %q", body, recorder.Code, recorder.Body.String())
		}
	}
	if len(*forwarded) != 3 || !strings.Contains((*forwarded)[0], `"kind":"Pod"`) {
		t.Fatalf("forwarded = %q, want all three bodies byte-for-byte", *forwarded)
	}
}

func TestK3SMCPProxyRefusesWhatItCannotInspect(t *testing.T) {
	handler, forwarded := newDenyTestProxy(t)
	for body, want := range map[string]int{
		`[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"resource_get"}},` +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"session_write"}}]`: http.StatusForbidden,
		`not json`: http.StatusBadRequest,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"resource_delete"}`: http.StatusBadRequest,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"},"pad":"` +
			strings.Repeat("a", maxK3SMCPRequestBody) + `"}`: http.StatusRequestEntityTooLarge,
	} {
		if recorder := postK3SMCP(handler, body); recorder.Code != want {
			t.Errorf("request %.80s… = %d, want %d", body, recorder.Code, want)
		}
	}
	if len(*forwarded) != 0 {
		t.Fatalf("upstream received %d requests the filter could not clear", len(*forwarded))
	}
}
