package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

func TestToolApprovalEndpoints(t *testing.T) {
	srv := newTenantServer(t)
	resp, body := tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", "beta", map[string]string{"name": "cc", "workloadType": "claude-code"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create claude-code = %d %s", resp.StatusCode, body)
	}
	var claude session.Session
	if err := json.Unmarshal(body, &claude); err != nil {
		t.Fatal(err)
	}
	resp, body = tenantCall(t, srv, http.MethodPost, "/api/v1/sessions", "alpha", map[string]string{"name": "sh"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create shell = %d %s", resp.StatusCode, body)
	}
	var shell session.Session
	if err := json.Unmarshal(body, &shell); err != nil {
		t.Fatal(err)
	}

	resp, body = tenantCall(t, srv, http.MethodGet, "/api/v1/sessions/"+claude.ID+"/tool-approvals", "beta", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
	var page session.ToolApprovalPage
	if err := json.Unmarshal(body, &page); err != nil || len(page.Approvals) != 0 {
		t.Fatalf("list body = %s (%v)", body, err)
	}

	hash := "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	decide := map[string]any{"decision": "approve", "argsHash": hash, "epoch": "ep_1111111111111111"}
	path := "/api/v1/sessions/" + claude.ID + "/tool-approvals/ap_1/decide"
	for _, tc := range []struct {
		name, method, path, tenant string
		body                       any
		want                       int
	}{
		{"list, another tenant", http.MethodGet, "/api/v1/sessions/" + claude.ID + "/tool-approvals", "alpha", nil, http.StatusNotFound},
		{"list, not claude-code", http.MethodGet, "/api/v1/sessions/" + shell.ID + "/tool-approvals", "alpha", nil, http.StatusUnprocessableEntity},
		{"decide, nothing waits", http.MethodPost, path, "beta", decide, http.StatusNotFound},
		{"decide, another tenant", http.MethodPost, path, "alpha", decide, http.StatusNotFound},
		{"decide, no body", http.MethodPost, path, "beta", nil, http.StatusBadRequest},
		{"decide, unknown field", http.MethodPost, path, "beta", map[string]any{"decision": "approve", "argsHash": hash, "epoch": "ep_1", "force": true}, http.StatusBadRequest},
		{"decide, no epoch", http.MethodPost, path, "beta", map[string]any{"decision": "approve", "argsHash": hash}, http.StatusBadRequest},
		{"decide, not a decision", http.MethodPost, path, "beta", map[string]any{"decision": "allow", "argsHash": hash, "epoch": "ep_1"}, http.StatusBadRequest},
		{"decide, who decided is not the caller's to send", http.MethodPost, path, "beta", map[string]any{"decision": "approve", "argsHash": hash, "epoch": "ep_1", "decidedBy": "someone else"}, http.StatusBadRequest},
		{"decide, not claude-code", http.MethodPost, "/api/v1/sessions/" + shell.ID + "/tool-approvals/ap_1/decide", "alpha", decide, http.StatusUnprocessableEntity},
		{"write asking for approvals on a shell session", http.MethodPost, "/api/v1/sessions/" + shell.ID + "/write", "alpha", map[string]any{"payload": "ls\n", "toolApprovals": true}, http.StatusBadRequest},
	} {
		resp, body := tenantCall(t, srv, tc.method, tc.path, tc.tenant, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s = %d %s, want %d", tc.name, resp.StatusCode, body, tc.want)
		}
	}
}
