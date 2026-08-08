//go:build e2e

// 검증 AC: AC-D3
//
// read = shell stdout/stderr, offset-cursored delta (docs/prd/shell-workload.md,
// docs/test/shell-workload.md scenario 3): a read at the previous read's
// nextOffset returns only output produced since, and offset=0 keeps replaying
// the full ordered history — reads are non-consuming.
//
// The state dispatch around read is AC-C2's file; that the cursor stays valid
// across a snapshot/restore is AC-B3's.
package e2e_test

import (
	"strings"
	"testing"
)

func TestReadCursor_DeltaAndFullReplay(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "echo d3-first-$((40+1))\n")
	first := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d3-first-41")
	})
	if first.NextOffset <= 0 {
		t.Fatalf("nextOffset=%d want > 0 (AC-D3 cursor)", first.NextOffset)
	}

	// Quiet shell: the cursor read must not replay pre-cursor output.
	if d := readShellAt(t, s.ID, first.NextOffset); strings.Contains(d.Payload, "d3-first-41") {
		t.Fatalf("cursor read replayed old output %q, want delta only (AC-D3)", d.Payload)
	}

	writeShell(t, s.ID, "echo d3-second-$((40+3))\n")
	delta := eventuallyShellRead(t, s.ID, first.NextOffset, func(p string) bool {
		return strings.Contains(p, "d3-second-43")
	})
	if strings.Contains(delta.Payload, "d3-first-41") {
		t.Fatalf("cursor read %q contains pre-cursor output, want only the delta (AC-D3)", delta.Payload)
	}

	// offset=0 replays everything in execution order — reads consumed nothing.
	full := readShellAt(t, s.ID, 0)
	i, j := strings.Index(full.Payload, "d3-first-41"), strings.Index(full.Payload, "d3-second-43")
	if i == -1 || j == -1 || i > j {
		t.Fatalf("full read must contain d3-first-41 then d3-second-43 in order; payload=%q", full.Payload)
	}
	if full.NextOffset < delta.NextOffset {
		t.Fatalf("full read cursor %d regressed below delta cursor %d", full.NextOffset, delta.NextOffset)
	}
}
