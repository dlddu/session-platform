//go:build e2e

// 검증 시나리오: 없음 (스모크·인프라)
//
// Registered as a non-AC matching unit in docs/test/e2e.md §"비-AC 파일 등재"
// (1:1 rule 3).
package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSmoke_Healthz(t *testing.T) {
	resp, body := do(t, http.MethodGet, "/api/v1/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status=%d body=%s", resp.StatusCode, body)
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode healthz: %v body=%s", err, body)
	}
	if m["status"] != "ok" {
		t.Fatalf("healthz status=%q want ok", m["status"])
	}
}

func TestSmoke_SessionSurfaceRoundTrips(t *testing.T) {
	s := createSession(t, uniqueName(t))

	resp, body := do(t, http.MethodGet, "/api/v1/sessions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	var lr struct {
		Sessions []session `json:"sessions"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode list: %v body=%s", err, body)
	}
	found := false
	for _, x := range lr.Sessions {
		if x.ID == s.ID {
			if x.Name != s.Name {
				t.Fatalf("listed name=%q want %q", x.Name, s.Name)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created session %s (%s) not found in list of %d", s.ID, s.Name, len(lr.Sessions))
	}

	got := getSession(t, s.ID)
	if got.ID != s.ID || got.Name != s.Name || got.Pod != s.Pod || got.State != "active" {
		t.Fatalf("get mismatch: got %+v want id/name/pod of %+v", got, s)
	}
}

func TestSmoke_UnknownSessionIsNotFound(t *testing.T) {
	resp, _ := do(t, http.MethodGet, "/api/v1/sessions/does-not-exist", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get unknown status=%d want 404", resp.StatusCode)
	}
}
