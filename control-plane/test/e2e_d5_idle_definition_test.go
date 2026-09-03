//go:build e2e

// 검증 AC: AC-D5
//
// Idle is measured by the absence of CLIENT shell I/O, not by the shell's
// busyness (docs/prd/shell-workload.md). `lastAccess` — the timestamp AC-B1's
// idle window counts from — is advanced by a read or a write, and by nothing
// else: a shell producing output on its own (a background job's log) does not
// reset the idle clock.
//
// GET /sessions/{id} deliberately does not count as an access, which is what
// makes this observable from the outside: the assertions below sample
// `lastAccess` via GET without perturbing it.
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

// A read with no write still counts as client I/O: the idle clock resets.
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

// The shell talking to itself is not client I/O: the idle clock keeps running.
func TestIdleDefinition_ShellSelfOutputDoesNotRefreshLastAccess(t *testing.T) {
	s := createSession(t, uniqueName(t))

	// A background job that keeps writing to the PTY for several seconds. This
	// write is the last client I/O; everything after it is the shell's own doing.
	writeShell(t, s.ID, "(for i in 1 2 3 4 5; do echo d5-bg-$i; sleep 1; done) &\n")
	before := lastAccessOf(t, s.ID)

	// Sit out several seconds of shell output without reading or writing.
	time.Sleep(4 * time.Second)
	after := lastAccessOf(t, s.ID)

	if !after.Equal(before) {
		t.Fatalf("lastAccess moved from %s to %s while only the shell was talking — idle must track client access, not shell busyness (AC-D5)", before, after)
	}

	// Premise check (after the sampling, so it cannot affect it): the shell really
	// was producing output during that window.
	out := eventuallyShellRead(t, s.ID, 0, func(p string) bool {
		return strings.Contains(p, "d5-bg-3")
	})
	if !strings.Contains(out.Payload, "d5-bg-1") {
		t.Fatalf("background job never ran; the negative assertion above was vacuous. payload=%q", out.Payload)
	}
}
