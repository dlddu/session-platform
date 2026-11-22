package main

import (
	"context"
	"strings"
	"testing"
)

func pairStr(v string) *string { return &v }

func pairInt(v int64) *int64 { return &v }

// pairedWorkSurface is the work surface of a session the control plane marked
// as a paired parent, over its own temporary database.
func pairedWorkSurface(t *testing.T, role string) *sessionDB {
	t.Helper()
	db, err := newSessionDB(t.TempDir(), false, role, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Every write the slot surface has is refused for a paired parent, and the
// refusal happens before the transaction — so the call has no effect, not just
// no tool entry. turn_dispatch is in the list, which is what stops the parent
// instructing a work session directly.
func TestPairedParentCannotWriteWorkOrDispatchThroughTheSlotSurface(t *testing.T) {
	ctx := context.Background()
	db := pairedWorkSurface(t, pairRoleParent)

	// Valid arguments are not needed: the boundary sits in front of validation
	// as well as in front of the transaction, which is the point.
	for _, name := range []string{
		"task_put", "task_delete", "turn_append", "turn_patch", "turn_dispatch",
		taskBookPutTool, monitorPutTool, monitorDeleteTool,
	} {
		_, err := db.work(ctx, name, workArgs{RequestID: "r-" + name, TaskID: "t1", ID: "t1"})
		if err == nil {
			t.Fatalf("%s was allowed for a paired parent", name)
		}
		var fail workError
		if !asWorkError(err, &fail) || fail.Code != "permission_denied" {
			t.Fatalf("%s refusal = %v, want permission_denied", name, err)
		}
		if !strings.Contains(fail.ErrorText, "owned by its monitor") {
			t.Fatalf("%s refusal must say who owns the work: %s", name, fail.ErrorText)
		}
	}

	// Nothing was written: the database has no task to find.
	if _, err := db.work(ctx, "task_get", workArgs{ID: "t1"}); err == nil {
		t.Fatal("a refused write still created the task")
	}
}

// The parent keeps its reads. Those are how it shows the list, the detail and
// the history its monitor owns.
func TestPairedParentKeepsWorkReads(t *testing.T) {
	ctx := context.Background()
	db := pairedWorkSurface(t, pairRoleParent)
	for _, name := range []string{"task_list", "turn_list", monitorListTool} {
		if _, err := db.work(ctx, name, workArgs{TaskID: "t1"}); err != nil {
			var fail workError
			if asWorkError(err, &fail) && fail.Code == "permission_denied" {
				t.Fatalf("%s must stay readable for the parent: %s", name, fail.ErrorText)
			}
		}
	}
}

// An unpaired or legacy session has no role and is not affected: its writes
// still land, which is what keeps every existing session working.
func TestUnpairedSessionKeepsTheWholeWorkSurface(t *testing.T) {
	ctx := context.Background()
	db := pairedWorkSurface(t, "")
	if _, err := db.work(ctx, "task_put", workArgs{
		RequestID: "r1", ID: "t1", ExpectedVersion: pairInt(0), Task: &workTaskInput{Title: pairStr("legacy"), Description: pairStr("왜"), CompletionCriteria: pairStr("끝나면"), Status: pairStr("planned")}}); err != nil {
		t.Fatalf("an unpaired session lost task_put: %v", err)
	}
	got, err := db.work(ctx, "task_get", workArgs{ID: "t1"})
	if err != nil {
		t.Fatalf("task_get after an unpaired write: %v", err)
	}
	if got == nil {
		t.Fatal("the unpaired write did not land")
	}
}

// The monitor side is the executor: it keeps every write, including the worker
// dispatch, because it is the one the parent's instructions are forwarded to.
func TestBoundMonitorKeepsTheWriteSurface(t *testing.T) {
	ctx := context.Background()
	db := pairedWorkSurface(t, "monitor")
	if _, err := db.work(ctx, "task_put", workArgs{
		RequestID: "r1", ID: "t1", ExpectedVersion: pairInt(0), Task: &workTaskInput{Title: pairStr("monitor owns this"), Description: pairStr("왜"), CompletionCriteria: pairStr("끝나면"), Status: pairStr("planned")}}); err != nil {
		t.Fatalf("the monitor lost task_put: %v", err)
	}
}
