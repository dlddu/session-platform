package session

import (
	"encoding/json"
	"testing"
	"time"
)

// A record written before memoryMultiplier existed carries only doubleMemory;
// it must still read as tier 2, through the ConfigMap record and the manifest.
func TestLegacyDoubleMemoryRecordReadsAsTierTwo(t *testing.T) {
	var sess Session
	if err := json.Unmarshal([]byte(`{"id":"x","name":"n","state":"snapshot","doubleMemory":true}`), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.MemoryTier() != 2 {
		t.Fatalf("legacy record tier = %d, want 2", sess.MemoryTier())
	}

	man := ManifestFor(&sess, &Checkpoint{Ref: "r"})
	if man.MemoryMultiplier != 2 || !man.DoubleMemory {
		t.Fatalf("manifest from legacy record: %d %v, want 2 true", man.MemoryMultiplier, man.DoubleMemory)
	}
	wire := []byte(`{"version":1,"id":"0123456789abcdef0123456789abcdef","name":"n","workloadType":"shell","doubleMemory":true,"createdAt":"2026-11-16T00:00:00Z","checkpoint":{"ref":"r"}}`)
	old, err := DecodeManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := old.SnapshotSession(time.Now()).MemoryTier(); got != 2 {
		t.Fatalf("legacy manifest tier = %d, want 2", got)
	}
}

// SetMemoryTier keeps doubleMemory true exactly at tier 2 and stores tier 1
// as the omitted default.
func TestSetMemoryTier(t *testing.T) {
	for _, tc := range []struct {
		tier, stored int
		double       bool
	}{{1, 0, false}, {2, 2, true}, {4, 4, false}, {8, 8, false}} {
		s := Session{DoubleMemory: true, MemoryMultiplier: 4}
		s.SetMemoryTier(tc.tier)
		if s.MemoryMultiplier != tc.stored || s.DoubleMemory != tc.double || s.MemoryTier() != tc.tier {
			t.Errorf("tier %d: stored %d double %v reads %d", tc.tier, s.MemoryMultiplier, s.DoubleMemory, s.MemoryTier())
		}
	}
}
