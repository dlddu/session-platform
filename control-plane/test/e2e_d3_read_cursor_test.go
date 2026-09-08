//go:build e2e

// 검증 시나리오: shell-workload.md#시나리오 3
//
// docs/prd/shell-workload.md, docs/test/shell-workload.md scenario 3.
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

	full := readShellAt(t, s.ID, 0)
	i, j := strings.Index(full.Payload, "d3-first-41"), strings.Index(full.Payload, "d3-second-43")
	if i == -1 || j == -1 || i > j {
		t.Fatalf("full read must contain d3-first-41 then d3-second-43 in order; payload=%q", full.Payload)
	}
	if full.NextOffset < delta.NextOffset {
		t.Fatalf("full read cursor %d regressed below delta cursor %d", full.NextOffset, delta.NextOffset)
	}
}
