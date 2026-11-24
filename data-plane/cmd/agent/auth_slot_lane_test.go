package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The slot a manager's two lanes share (AC-N11). The login is one file, and the
// lanes take turns at the same time, so the slot has to keep two HOMEs equal to
// it without either lane's turn deciding anything about the other's.

func writeHomeLogin(t *testing.T, slot *authSlot, home, content string) {
	t.Helper()
	path := slot.homePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAuthSlotKeepsTurnStatePerHome: one `placed` flag for two HOMEs would mean
// one lane's pull decided whether the other lane's missing file counted as the
// CLI logging out — which would empty the slot's login on a turn that never
// touched it.
func TestAuthSlotKeepsTurnStatePerHome(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	parentHome, monitorHome := t.TempDir(), t.TempDir()
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"token-1"}`)); err != nil {
		t.Fatal(err)
	}

	// Only the parent lane pulls. The monitor lane has not looked yet, so its
	// empty HOME is "has not started a turn", not "logged out".
	if err := slot.pull(parentHome); err != nil {
		t.Fatal(err)
	}
	if got, ok := readOptional(t, slot.homePath(parentHome)); !ok || got != `{"t":"token-1"}` {
		t.Fatalf("parent HOME = %q, %v; want the slot's login", got, ok)
	}
	if _, ok := readOptional(t, slot.homePath(monitorHome)); ok {
		t.Fatal("the monitor lane's HOME must be untouched by the parent's pull")
	}
	if err := slot.push(monitorHome); err != nil {
		t.Fatalf("the monitor lane's end-of-turn copy: %v", err)
	}
	if got, ok := readOptional(t, slot.slotPath()); !ok || got != `{"t":"token-1"}` {
		t.Fatalf("slot = %q, %v; a lane that never pulled must not empty the login", got, ok)
	}

	// Now the monitor lane pulls, then its CLI logs out. That one is a real
	// logout, and the slot follows it.
	if err := slot.pull(monitorHome); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(slot.homePath(monitorHome)); err != nil {
		t.Fatal(err)
	}
	if err := slot.push(monitorHome); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOptional(t, slot.slotPath()); ok {
		t.Fatal("a logout after this HOME's own pull must empty the slot")
	}
}

// TestAuthSlotRefusesTheSlowerLanesStaleCopy is the compare-and-swap between
// the lanes: a lane may only replace the login it started its turn from, so the
// slower lane cannot undo the refresh the faster one just made. The lane that
// is behind re-adopts the newer file on its next pull.
func TestAuthSlotRefusesTheSlowerLanesStaleCopy(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	parentHome, monitorHome := t.TempDir(), t.TempDir()
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"token-1"}`)); err != nil {
		t.Fatal(err)
	}

	// Both lanes start a turn from token-1.
	if err := slot.pull(parentHome); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(monitorHome); err != nil {
		t.Fatal(err)
	}

	// The monitor lane's CLI refreshes the login and its copy lands first.
	writeHomeLogin(t, slot, monitorHome, `{"t":"token-2"}`)
	if err := slot.push(monitorHome); err != nil {
		t.Fatal(err)
	}
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"token-2"}` {
		t.Fatalf("slot = %q, want the refreshed login", got)
	}

	// The parent lane's turn ends holding the login it started from. Copying it
	// back would undo the refresh, so it is refused — and refused as "the slot
	// moved on", not as a failure to retry, because retrying is exactly what
	// must not happen.
	if err := slot.push(parentHome); !errors.Is(err, errLoginOverwritten) {
		t.Fatalf("the slower lane's copy back = %v, want errLoginOverwritten", err)
	}
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"token-2"}` {
		t.Fatalf("slot = %q; the slower lane overwrote the newer login", got)
	}
	// Refused that way, it must not be queued for a retry either.
	if state := slot.homes[parentHome]; state == nil || state.unpushed {
		t.Fatalf("parent lane state = %+v, want no pending retry of the older copy", state)
	}

	// The lane that is behind adopts the newer file on its next pull, and from
	// then on its own copies land again.
	if err := slot.pull(parentHome); err != nil {
		t.Fatal(err)
	}
	if got, _ := readOptional(t, slot.homePath(parentHome)); got != `{"t":"token-2"}` {
		t.Fatalf("parent HOME after re-pull = %q, want the newer login", got)
	}
	writeHomeLogin(t, slot, parentHome, `{"t":"token-3"}`)
	if err := slot.push(parentHome); err != nil {
		t.Fatalf("after re-adopting, the parent lane's copy must land: %v", err)
	}
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"token-3"}` {
		t.Fatalf("slot = %q, want token-3", got)
	}
}

// TestAuthSlotUnchangedCopyIsNotAConflict: a lane whose CLI never touched the
// login copies back a file equal to what it started from. That is the common
// case for the lane that did not refresh, and it must stay silent rather than
// reading as the other lane having moved the slot.
func TestAuthSlotUnchangedCopyIsNotAConflict(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	home := t.TempDir()
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"token-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(home); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := slot.push(home); err != nil {
			t.Fatalf("an unchanged copy back must succeed: %v", err)
		}
	}
	if got, _ := readOptional(t, slot.slotPath()); got != `{"t":"token-1"}` {
		t.Fatalf("slot = %q, want it unchanged", got)
	}
}

// TestAuthSlotLanesShareOneObject is why the pair is one pod: both lanes go
// through one process's lock, so their copies are serialised rather than two
// pods racing renames on a network filesystem.
func TestAuthSlotLanesShareOneObject(t *testing.T) {
	slot := newTestAuthSlot(t, codexAuthFile, codexAuthHomeRel)
	parentLane := managerLane{role: relayLaneParent, incarnation: "p1", slot: slot}
	monitorLane := managerLane{role: relayLaneMonitor, incarnation: "m1", slot: parentLane.slot}
	if monitorLane.slot != parentLane.slot {
		t.Fatal("the two lanes must hold the same slot object, not two views of one file")
	}
	// And the two lanes are two HOMEs on it, each with its own turn state.
	parentHome, monitorHome := t.TempDir(), t.TempDir()
	if err := writeLoginFile(slot.slotPath(), []byte(`{"t":"token-1"}`)); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(parentHome); err != nil {
		t.Fatal(err)
	}
	if err := slot.pull(monitorHome); err != nil {
		t.Fatal(err)
	}
	if len(slot.homes) != 2 {
		t.Fatalf("slot tracks %d HOME(s), want one per lane", len(slot.homes))
	}
	if slot.homes[parentHome] == slot.homes[monitorHome] {
		t.Fatal("the lanes must not share one turn state")
	}
}
