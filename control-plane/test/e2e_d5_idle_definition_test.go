//go:build e2e

// 검증 AC: AC-D5
//
// docs/prd/shell-workload.md.
//
// That GET is not an access is what makes this measurable at all: the assertions
// below sample `lastAccess` via GET without perturbing the thing they measure.
package e2e_test

import (
	"strings"
	"testing"
	"time"
)

func lastAccessOf(t *testing.T, id string) time.Time {
	t.Helper()
	s := getSession(t, id)
	ts, err := time.Parse(time.RFC3339Nano, s.LastAccess)
	if err != nil {
		t.Fatalf("parse lastAccess %q: %v", s.LastAccess, err)
	}
	return ts
}

func TestIdleDefinition_ReadAloneRefreshesLastAccess(t *testing.T) {
	s := createSession(t, uniqueName(t))

	before := lastAccessOf(t, s.ID)
	time.Sleep(1500 * time.Millisecond)

	readShellAt(t, s.ID, 0) // read only — no write
	after := lastAccessOf(t, s.ID)

	if !after.After(before) {
		t.Fatalf("lastAccess %s did not advance after a read (was %s) — a read is client I/O (AC-D5)", after, before)
	}
}

func TestIdleDefinition_ShellSelfOutputDoesNotRefreshLastAccess(t *testing.T) {
	s := createSession(t, uniqueName(t))

	writeShell(t, s.ID, "(for i in 1 2 3 4 5; do echo d5-bg-$i; sleep 1; done) &\n")
	before := lastAccessOf(t, s.ID)

	time.Sleep(4 * time.Second)
	after := lastAccessOf(t, s.ID)

	if !after.Equal(before) {
		t.Fatalf("lastAccess moved from %s to %s while only the shell was talking — idle must track client access, not shell busyness (AC-D5)", before, after)
	}

	// Premise check, and it must stay after the sampling — this read is itself
	// client I/O and would advance the very timestamp asserted above.
	out := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d5-bg-3")
	})
	if !strings.Contains(out.Payload, "d5-bg-1") {
		t.Fatalf("background job never ran; the negative assertion above was vacuous. payload=%q", out.Payload)
	}
}
