package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dlddu/session-platform/control-plane/internal/session"
)

// Session creation's doubleMemory option: the wire contract validates it,
// echoes its normalized value, and copies it to provisioning.
func TestCreateSessionDoubleMemory(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	status, s := createRaw(t, srv.URL, map[string]any{"name": "d1", "doubleMemory": true})
	if status != http.StatusCreated || !s.DoubleMemory || orch.SpecFor(s.ID).MemoryMultiplier != 2 {
		t.Fatalf("doubleMemory true: status %d, session %v, pod %v", status, s.DoubleMemory, orch.SpecFor(s.ID).MemoryMultiplier)
	}

	status, s = createRaw(t, srv.URL, map[string]any{"name": "d2", "doubleMemory": false})
	if status != http.StatusCreated || s.DoubleMemory || orch.SpecFor(s.ID).MemoryMultiplier != 1 {
		t.Fatalf("doubleMemory false: status %d, session %v, pod %v", status, s.DoubleMemory, orch.SpecFor(s.ID).MemoryMultiplier)
	}

	status, s = createRaw(t, srv.URL, map[string]any{"name": "d3"})
	if status != http.StatusCreated || s.DoubleMemory || orch.SpecFor(s.ID).MemoryMultiplier != 1 {
		t.Fatalf("omitted doubleMemory: status %d, session %v, pod %v", status, s.DoubleMemory, orch.SpecFor(s.ID).MemoryMultiplier)
	}

	before := orch.RunningCount()
	for _, body := range []map[string]any{
		{"name": "x", "doubleMemory": "yes"},
		{"name": "x", "doubleMemory": 1},
		{"name": "x", "doubleMemory": nil},
	} {
		if status, _ := createRaw(t, srv.URL, body); status != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", body, status)
		}
	}
	if after := orch.RunningCount(); after != before {
		t.Fatalf("refused creates started pods: %d -> %d", before, after)
	}
}

// switch takes an optional doubleMemory that applies when it restores a
// snapshot; on a live session the other tier is refused with 422.
func TestSwitchSessionDoubleMemory(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()
	post := func(path string, body any) (int, session.Session) {
		t.Helper()
		resp, raw := tenantCall(t, srv, http.MethodPost, path, "", body)
		var s session.Session
		_ = json.Unmarshal(raw, &s)
		return resp.StatusCode, s
	}

	_, s := createRaw(t, srv.URL, map[string]any{"name": "grow"})
	base := "/api/v1/sessions/" + s.ID
	for _, body := range []map[string]any{
		{"doubleMemory": "yes"},
		{"doubleMemory": nil},
		{"memory": true},
	} {
		if status, _ := post(base+"/switch", body); status != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", body, status)
		}
	}
	if status, _ := post(base+"/switch", map[string]any{"doubleMemory": true}); status != http.StatusUnprocessableEntity {
		t.Fatalf("tier change on a live session: status %d, want 422", status)
	}

	if status, _ := post(base+"/snapshot", nil); status != http.StatusOK {
		t.Fatalf("snapshot: status %d", status)
	}
	status, got := post(base+"/switch", map[string]any{"doubleMemory": true})
	if status != http.StatusOK || !got.DoubleMemory || orch.SpecFor(s.ID).MemoryMultiplier != 2 {
		t.Fatalf("restore with doubleMemory: status %d, session %v, pod %v", status, got.DoubleMemory, orch.SpecFor(s.ID).MemoryMultiplier)
	}
}

// memoryMultiplier picks any tier in session.MemoryMultipliers; doubleMemory
// stays an alias for tier 2 and must agree with it when both are given.
func TestCreateSessionMemoryMultiplier(t *testing.T) {
	srv, orch, _ := newServerWithOrchestrator()
	defer srv.Close()

	for _, tc := range []struct {
		body       map[string]any
		tier       int
		wantDouble bool
	}{
		{map[string]any{"memoryMultiplier": 1}, 1, false},
		{map[string]any{"memoryMultiplier": 2}, 2, true},
		{map[string]any{"memoryMultiplier": 4}, 4, false},
		{map[string]any{"memoryMultiplier": 8}, 8, false},
		{map[string]any{"memoryMultiplier": 2, "doubleMemory": true}, 2, true},
		{map[string]any{"memoryMultiplier": 1, "doubleMemory": false}, 1, false},
	} {
		tc.body["name"] = "m"
		status, s := createRaw(t, srv.URL, tc.body)
		if status != http.StatusCreated {
			t.Errorf("%v: status %d, want 201", tc.body, status)
			continue
		}
		wantStored := tc.tier
		if wantStored == 1 {
			wantStored = 0
		}
		if s.MemoryMultiplier != wantStored || s.DoubleMemory != tc.wantDouble || orch.SpecFor(s.ID).MemoryMultiplier != tc.tier {
			t.Errorf("%v: session memoryMultiplier %d doubleMemory %v, pod %d; want %d %v %d",
				tc.body, s.MemoryMultiplier, s.DoubleMemory, orch.SpecFor(s.ID).MemoryMultiplier, wantStored, tc.wantDouble, tc.tier)
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
		{"name": "x", "memoryMultiplier": 4, "doubleMemory": true},
		{"name": "x", "memoryMultiplier": 2, "doubleMemory": false},
	} {
		if status, _ := createRaw(t, srv.URL, body); status != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", body, status)
		}
	}
	if after := orch.RunningCount(); after != before {
		t.Fatalf("refused creates started pods: %d -> %d", before, after)
	}
}

// switch raises a snapshotted session straight to tier 8; a live session
// refuses any other tier with 422 and an unknown tier with 400.
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
	if status, _ := post(base+"/switch", map[string]any{"memoryMultiplier": 5}); status != http.StatusBadRequest {
		t.Fatalf("unknown tier: status %d, want 400", status)
	}
	if status, _ := post(base+"/switch", map[string]any{"memoryMultiplier": 8}); status != http.StatusUnprocessableEntity {
		t.Fatalf("tier change on a live session: status %d, want 422", status)
	}
	if status, _ := post(base+"/switch", map[string]any{"doubleMemory": true}); status != http.StatusOK {
		t.Fatalf("doubleMemory naming the live tier: status %d, want 200", status)
	}

	if status, _ := post(base+"/snapshot", nil); status != http.StatusOK {
		t.Fatalf("snapshot: status %d", status)
	}
	status, got := post(base+"/switch", map[string]any{"memoryMultiplier": 8})
	if status != http.StatusOK || got.MemoryMultiplier != 8 || got.DoubleMemory || orch.SpecFor(s.ID).MemoryMultiplier != 8 {
		t.Fatalf("restore at tier 8: status %d, session %d/%v, pod %d", status, got.MemoryMultiplier, got.DoubleMemory, orch.SpecFor(s.ID).MemoryMultiplier)
	}
}
