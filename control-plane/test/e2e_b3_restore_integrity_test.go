//go:build e2e

// 검증 시나리오: lifecycle.md#시나리오 3
//
// docs/prd/lifecycle.md, docs/test/lifecycle.md scenario 3; the observable cursor
// contract is the "offset과 복원" design note in docs/prd/shell-workload.md.
package e2e_test

import (
	"strings"
	"testing"
)

func TestRestoreIntegrity_HistoryAndCursorSurviveTheFreeze(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "echo b3-pre-$((40+1))\n")
	before := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "b3-pre-41")
	})
	if before.NextOffset <= 0 {
		t.Fatalf("pre-freeze nextOffset=%d want > 0", before.NextOffset)
	}

	if _, ok := snapshotSession(t, s.ID); !ok {
		t.Skip("SUT predates the product snapshot endpoint — the CRIU round trip is not exercisable here; see docs/criu-verification.md")
	}

	writeShell(t, s.ID, "echo b3-post-$((40+2))\n")

	delta := eventuallyShellRead(t, s.ID, before.NextOffset, func(p string) bool {
		return strings.Contains(p, "b3-post-42")
	})
	if strings.Contains(delta.Payload, "b3-pre-41") {
		t.Fatalf("delta from the pre-freeze cursor replayed pre-freeze output %q; the cursor must stay valid across the freeze (AC-B3)", delta.Payload)
	}

	full := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "b3-pre-41") && strings.Contains(p, "b3-post-42")
	})
	pre, post := strings.Index(full.Payload, "b3-pre-41"), strings.Index(full.Payload, "b3-post-42")
	if pre == -1 || post == -1 || pre > post {
		t.Fatalf("history order broken across the freeze: pre=%d post=%d in %q (AC-B3)", pre, post, full.Payload)
	}
	if full.NextOffset < delta.NextOffset {
		t.Fatalf("full-read cursor %d regressed below the delta cursor %d after restore", full.NextOffset, delta.NextOffset)
	}
}
