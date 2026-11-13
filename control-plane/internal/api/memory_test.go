package api_test

import (
	"net/http"
	"testing"
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
