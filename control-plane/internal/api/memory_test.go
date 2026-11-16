package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Session creation's memoryMultiplier: the wire contract validates it, echoes
// it (tier 1 as absent), and copies it to provisioning.
func TestCreateSessionMemoryMultiplier(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	for _, tc := range []struct {
		body         map[string]any
		tier, stored int
	}{
		{map[string]any{}, 1, 0},
		{map[string]any{"memoryMultiplier": 1}, 1, 0},
		{map[string]any{"memoryMultiplier": 2}, 2, 2},
		{map[string]any{"memoryMultiplier": 4}, 4, 4},
		{map[string]any{"memoryMultiplier": 8}, 8, 8},
	} {
		tc.body["name"] = "m"
		status, s := createRaw(t, srv.URL, tc.body)
		if status != http.StatusCreated {
			t.Errorf("%v: status %d, want 201", tc.body, status)
			continue
		}
		if s.MemoryMultiplier != tc.stored || orch.SpecFor(s.ID).MemoryMultiplier != tc.tier {
			t.Errorf("%v: session memoryMultiplier %d, pod %d; want %d, %d",
				tc.body, s.MemoryMultiplier, orch.SpecFor(s.ID).MemoryMultiplier, tc.stored, tc.tier)
		}
	}

	before := orch.RunningCount()
	for _, body := range []map[string]any{
		{"name": "x", "memoryMultiplier": 0},
		{"name": "x", "memoryMultiplier": 3},
		{"name": "x", "memoryMultiplier": 16},
		{"name": "x", "memoryMultiplier": 2.5},
		{"name": "x", "memoryMultiplier": "4"},
		{"name": "x", "memoryMultiplier": nil},
		{"name": "x", "doubleMemory": true},
	} {
		if status, _ := createRaw(t, srv.URL, body); status != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", body, status)
		}
	}
	if after := orch.RunningCount(); after != before {
		t.Fatalf("refused creates started pods: %d -> %d", before, after)
	}
}

// switch takes an optional memoryMultiplier that applies when it restores a
// snapshot; on a live session another tier is refused with 422.
func TestSwitchSessionMemoryMultiplier(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()
	post := func(path string, body any) (int, session.Session) {
		t.Helper()
		resp, raw := tenantCall(t, srv, http.MethodPost, path, "", body)
		var s session.Session
		_ = json.Unmarshal(raw, &s)
		return resp.StatusCode, s
	}

	_, s := createRaw(t, srv.URL, map[string]any{"name": "grow", "memoryMultiplier": 2})
	base := "/api/v1/sessions/" + s.ID
	for _, body := range []map[string]any{
		{"memoryMultiplier": 5},
		{"memoryMultiplier": nil},
		{"doubleMemory": true},
	} {
		if status, _ := post(base+"/switch", body); status != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", body, status)
		}
	}
	if status, _ := post(base+"/switch", map[string]any{"memoryMultiplier": 8}); status != http.StatusUnprocessableEntity {
		t.Fatalf("tier change on a live session: status %d, want 422", status)
	}
	if status, _ := post(base+"/switch", map[string]any{"memoryMultiplier": 2}); status != http.StatusOK {
		t.Fatalf("the tier it already has: status %d, want 200", status)
	}

	if status, _ := post(base+"/snapshot", nil); status != http.StatusOK {
		t.Fatalf("snapshot: status %d", status)
	}
	status, got := post(base+"/switch", map[string]any{"memoryMultiplier": 8})
	if status != http.StatusOK || got.MemoryMultiplier != 8 || orch.SpecFor(s.ID).MemoryMultiplier != 8 {
		t.Fatalf("restore at tier 8: status %d, session %d, pod %d", status, got.MemoryMultiplier, orch.SpecFor(s.ID).MemoryMultiplier)
	}
}
