package session

import (
	"testing"
	"time"
)

// A manifest written while the tier was the boolean doubleMemory recovers at
// tier 2.
func TestLegacyDoubleMemoryManifestReadsAsTierTwo(t *testing.T) {
	wire := []byte(`{"version":1,"id":"0123456789abcdef0123456789abcdef","name":"n","workloadType":"shell","doubleMemory":true,"createdAt":"2026-11-16T00:00:00Z","checkpoint":{"ref":"r"}}`)
	m, err := DecodeManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.SnapshotSession(time.Now()).MemoryTier(); got != 2 {
		t.Fatalf("legacy manifest tier = %d, want 2", got)
	}
	explicit := []byte(`{"version":1,"id":"0123456789abcdef0123456789abcdef","name":"n","workloadType":"shell","memoryMultiplier":8,"doubleMemory":true,"createdAt":"2026-11-16T00:00:00Z","checkpoint":{"ref":"r"}}`)
	if m, err = DecodeManifest(explicit); err != nil || m.MemoryMultiplier != 8 {
		t.Fatalf("memoryMultiplier beside the legacy flag: %v %v, want 8", m, err)
	}
}

// SetMemoryTier stores tier 1 as the omitted default.
func TestSetMemoryTier(t *testing.T) {
	for _, tc := range []struct{ tier, stored int }{{1, 0}, {2, 2}, {4, 4}, {8, 8}} {
		s := Session{MemoryMultiplier: 4}
		s.SetMemoryTier(tc.tier)
		if s.MemoryMultiplier != tc.stored || s.MemoryTier() != tc.tier {
			t.Errorf("tier %d: stored %d reads %d", tc.tier, s.MemoryMultiplier, s.MemoryTier())
		}
	}
}
