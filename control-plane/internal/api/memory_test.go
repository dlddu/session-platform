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
	if status != http.StatusCreated || !s.DoubleMemory || !orch.SpecFor(s.ID).DoubleMemory {
		t.Fatalf("doubleMemory true: status %d, session %v, pod %v", status, s.DoubleMemory, orch.SpecFor(s.ID).DoubleMemory)
	}

	status, s = createRaw(t, srv.URL, map[string]any{"name": "d2", "doubleMemory": false})
	if status != http.StatusCreated || s.DoubleMemory || orch.SpecFor(s.ID).DoubleMemory {
		t.Fatalf("doubleMemory false: status %d, session %v, pod %v", status, s.DoubleMemory, orch.SpecFor(s.ID).DoubleMemory)
	}

	status, s = createRaw(t, srv.URL, map[string]any{"name": "d3"})
	if status != http.StatusCreated || s.DoubleMemory || orch.SpecFor(s.ID).DoubleMemory {
		t.Fatalf("omitted doubleMemory: status %d, session %v, pod %v", status, s.DoubleMemory, orch.SpecFor(s.ID).DoubleMemory)
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
	if status != http.StatusOK || !got.DoubleMemory || !orch.SpecFor(s.ID).DoubleMemory {
		t.Fatalf("restore with doubleMemory: status %d, session %v, pod %v", status, got.DoubleMemory, orch.SpecFor(s.ID).DoubleMemory)
	}
}
