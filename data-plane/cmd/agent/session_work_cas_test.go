package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// The exclusion the contract claims: two parents that both read a task at the
// same version cannot both write. Before the guard counters existed, a turn
// write and a bookkeeping write only *compared* the version and then touched
// updated_at — so both writers won and the second silently overwrote the first.
func TestWorkTurnWritesExcludeASecondParentAtTheSameVersion(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)

	// Both parents read the task and see the same version.
	first := mustWork(t, s, "task_get", map[string]any{"id": "task-a"})
	version := int64(first["task"].(map[string]any)["version"].(float64))
	seq := int64(first["turn_version"].(float64))

	// Parent one appends the turn that follows.
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "p1", "expected_version": version,
		"expected_turn_version": seq,
		"turn":                  map[string]any{"request": "부모 1 이 받아 적은 말"},
	})

	// Parent two holds the same numbers it read before parent one wrote. It
	// must lose: its request_id is different, so this is a second action, not a
	// retry.
	wantWorkCode(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "p2", "expected_version": version,
		"expected_turn_version": seq,
		"turn":                  map[string]any{"request": "부모 2 가 받아 적은 말"},
	}, "conflict")

	turns := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)
	if len(turns) != 1 {
		t.Fatalf("both parents wrote: %v", turns)
	}
	if request := turns[0].(map[string]any)["request"]; request != "부모 1 이 받아 적은 말" {
		t.Fatalf("the loser overwrote the winner: %v", request)
	}
}

// The same exclusion for the parent's bookkeeping: two parents that both read
// the approval scope cannot both replace it, because a full replacement by the
// loser would drop whatever the winner had just recorded.
func TestWorkBookkeepingWritesExcludeASecondParentAtTheSameVersion(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	read := mustWork(t, s, taskBookGetTool, map[string]any{"task_id": "task-a"})
	book := int64(read["book_version"].(float64))

	mustWork(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-a", "request_id": "b1", "expected_book_version": book,
		"record": map[string]any{
			"stage": "implementation", "owner": "parent", "sessions": []any{},
			"details": map[string]any{"approval": "부모 1 이 적은 범위"},
		},
	})
	wantWorkCode(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-a", "request_id": "b2", "expected_book_version": book,
		"record": map[string]any{
			"stage": "verification", "owner": "parent", "sessions": []any{},
			"details": map[string]any{"approval": "부모 2 가 적은 범위"},
		},
	}, "conflict")

	back := mustWork(t, s, taskBookGetTool, map[string]any{"task_id": "task-a"})
	if got := back["details"].(map[string]any)["approval"]; got != "부모 1 이 적은 범위" {
		t.Fatalf("the loser overwrote the winner's approval scope: %v", got)
	}
}

// turnCursor reads what a parent must hold to write a turn: the task's own
// version and the turn lane's counter.
func turnCursor(t *testing.T, s *sessionDB, taskID string) (int64, int64) {
	t.Helper()
	got := mustWork(t, s, "task_get", map[string]any{"id": taskID})
	return int64(got["task"].(map[string]any)["version"].(float64)), int64(got["turn_version"].(float64))
}

// Re-reading does not buy a second claim. A parent that reads *after* another
// has taken a turn holds a current counter, so the counter alone would admit
// it; taking a turn somebody is already doing has to be refused on its own
// terms.
func TestWorkTurnCannotBeClaimedTwiceEvenAfterReReading(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	version, seq := turnCursor(t, s, "task-a")
	appended := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "r1", "expected_version": version,
		"expected_turn_version": seq,
		"turn":                  map[string]any{"request": "사람의 말"},
	})
	turnID := int64(appended["turn"].(map[string]any)["id"].(float64))

	// Parent one takes it.
	_, seq = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "claim-1",
		"expected_version": version, "expected_turn_version": seq,
		"turn": map[string]any{"started_at": "2026-11-19T00:00:00Z", "instruction": "부모 1 의 지시"},
	})

	// Parent two re-reads, so its counter is current, and still cannot take it.
	_, fresh := turnCursor(t, s, "task-a")
	wantWorkCode(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "claim-2",
		"expected_version": version, "expected_turn_version": fresh,
		"turn": map[string]any{"started_at": "2026-11-19T00:05:00Z", "instruction": "부모 2 의 지시"},
	}, "conflict")
	turn := mustWork(t, s, "turn_list", map[string]any{"task_id": "task-a"})["turns"].([]any)[0].(map[string]any)
	if turn["instruction"] != "부모 1 의 지시" {
		t.Fatalf("the second claim overwrote the first: %v", turn["instruction"])
	}

	// Handing it back is explicit, and then it can be taken again.
	_, fresh = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "handback",
		"expected_version": version, "expected_turn_version": fresh,
		"turn": map[string]any{"started_at": nil},
	})
	_, fresh = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "claim-3",
		"expected_version": version, "expected_turn_version": fresh,
		"turn": map[string]any{"started_at": "2026-11-19T00:10:00Z"},
	})
}

// Dispatch is for a turn somebody holds and has not finished. Replaying the
// same request_id answers what was committed and is not a second permission to
// act; a different operation key on a turn that already recorded one is
// refused without evidence the first never landed; and a turn that has ended
// cannot dispatch anything further.
func TestWorkTurnDispatchIsOwnedBoundedAndIdempotent(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	version, seq := turnCursor(t, s, "task-a")
	turnID := int64(mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "r1", "expected_version": version,
		"expected_turn_version": seq,
		"turn":                  map[string]any{"request": "사람의 말"},
	})["turn"].(map[string]any)["id"].(float64))

	// Nobody holds it yet, so there is nobody to act for it. A well-formed
	// token that holds nothing is still refused — the check is on who holds the
	// turn, not on whether a token was typed.
	_, seq = turnCursor(t, s, "task-a")
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d0",
		"expected_version": version, "expected_turn_version": seq,
		"claim_token":   "00000000000000000000000000000000",
		"operation_key": "op-1", "operation_target": "pull/1", "dispatch_state": "intended",
	}, "conflict")

	// Take it, then dispatch.
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "claim",
		"expected_version": version, "expected_turn_version": seq,
		"turn": map[string]any{"started_at": "2026-11-19T00:00:00Z"},
	})
	_, seq = turnCursor(t, s, "task-a")
	first := mustWork(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d1",
		"expected_version": version, "expected_turn_version": seq,
		"operation_key": "op-1", "operation_target": "pull/1", "dispatch_state": "dispatched",
	})
	after := int64(first["turn_version"].(float64))

	// The same request_id replays the committed answer. It must not be a second
	// permission: the counter does not move again, so the replay cannot be used
	// to slip a further write in behind it.
	replay := mustWork(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d1",
		"expected_version": version, "expected_turn_version": seq,
		"operation_key": "op-1", "operation_target": "pull/1", "dispatch_state": "dispatched",
	})
	if int64(replay["turn_version"].(float64)) != after {
		t.Fatalf("a replay moved the counter: %v vs %v", replay["turn_version"], after)
	}
	if replay["operation_key"] != "op-1" {
		t.Fatalf("replay = %v", replay)
	}
	// A stale counter is refused even for the same key, so a retry that thinks
	// it is first cannot act twice.
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d1-again",
		"expected_version": version, "expected_turn_version": seq,
		"operation_key": "op-1", "operation_target": "pull/1", "dispatch_state": "dispatched",
	}, "conflict")

	// A different operation key needs evidence the first never landed.
	_, seq = turnCursor(t, s, "task-a")
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d2",
		"expected_version": version, "expected_turn_version": seq,
		"operation_key": "op-2", "operation_target": "pull/2", "dispatch_state": "intended",
	}, "conflict")
	mustWork(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d3",
		"expected_version": version, "expected_turn_version": seq,
		"operation_key": "op-2", "operation_target": "pull/2", "dispatch_state": "intended",
		"observed": "not_accepted",
	})

	// Finish the turn; dispatching for finished work is refused.
	_, seq = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "done",
		"expected_version": version, "expected_turn_version": seq,
		"turn": map[string]any{"response": "답", "explanation": "설명", "ended_at": "2026-11-19T01:00:00Z"},
	})
	_, seq = turnCursor(t, s, "task-a")
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "d4",
		"expected_version": version, "expected_turn_version": seq,
		"operation_key": "op-2", "operation_target": "pull/2", "dispatch_state": "dispatched",
	}, "conflict")
	// And a finished turn is not rewritten.
	wantWorkCode(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": turnID, "request_id": "rewrite",
		"expected_version": version, "expected_turn_version": seq,
		"turn": map[string]any{"explanation": "다시 쓴 설명"},
	}, "conflict")
}

// The point of separate lanes: a cursor advance and a turn write must not
// invalidate the choices a screen is showing. What makes a pick current is the
// turn it was offered on still being the task's latest, which neither the
// parent's bookkeeping nor a patch to that same turn changes.
func TestWorkBookkeepingAndTurnWritesKeepChoicesValid(t *testing.T) {
	s, _ := newTestSessionDB(t)
	ctx := context.Background()
	newWorkTask(t, s, "task-a", "c1", nil)
	version, seq := turnCursor(t, s, "task-a")
	offered := mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "r1", "expected_version": version,
		"expected_turn_version": seq,
		"turn": map[string]any{
			"request":  "사람의 말",
			"question": "어떻게 할까요?",
			"choices": []any{
				map[string]any{"label": "전부", "request_text": "전부 해 줘"},
			},
		},
	})["turn"].(map[string]any)
	choiceTurn := int64(offered["id"].(float64))

	// The screen is showing that turn's choices, read the way a reader outside
	// the session reads them.
	latest := mustWorkView(t, s, workArgs{})["tasks"].([]workTaskView)[0].LatestTurn
	if latest == nil || len(latest.Choices) != 1 {
		t.Fatalf("no choice to keep: %+v", latest)
	}

	// A bookkeeping write (a cursor advance) and a patch to the same turn.
	book := int64(mustWork(t, s, taskBookGetTool, map[string]any{"task_id": "task-a"})["book_version"].(float64))
	mustWork(t, s, taskBookPutTool, map[string]any{
		"task_id": "task-a", "request_id": "b1", "expected_book_version": book,
		"record": map[string]any{
			"stage": "implementation", "owner": "parent",
			"sessions": []any{map[string]any{
				"id": "w1", "ownership": "owned", "purpose": "p", "cleanup": "pending", "nextOffset": float64(4096),
			}},
			"details": map[string]any{"approval": "PR only"},
		},
	})
	_, seq = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": choiceTurn, "request_id": "p1",
		"expected_turn_version": seq,
		"turn":                  map[string]any{"instruction": "지시"},
	})

	// The task's version has not moved, and the pick a screen is holding still
	// lands on the turn it was offered on.
	if nowVersion, _ := turnCursor(t, s, "task-a"); nowVersion != version {
		t.Fatalf("a turn or bookkeeping write moved the task version: %d -> %d", version, nowVersion)
	}
	if stored, replayed, err := storeWorkRequest(t, s, "pick-1", "task-a", "전부 해 줘", &choiceTurn); err != nil {
		t.Fatalf("a pick a screen was holding was refused after unrelated writes: %v (%v %v)", err, stored, replayed)
	}

	// A later turn is what makes the old screen old: the choices on the turn
	// the person picked from are not what the task is asking any more.
	_, seq = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "r2", "expected_version": version,
		"expected_turn_version": seq,
		"turn":                  map[string]any{"request": "다음 말"},
	})
	_, _, err := storeWorkRequest(t, s, "pick-2", "task-a", "전부 해 줘", &choiceTurn)
	if err == nil {
		t.Fatal("a pick from a turn the task has moved past was accepted")
	}
	var known workError
	if !asWorkError(err, &known) || known.Code != "stale_choice" {
		t.Fatalf("refusal = %v, want stale_choice so the app can keep the words the person typed", err)
	}
	_ = ctx
}

// storeWorkRequest is the one write a reader outside the session has: a
// person's words become the next turn of a task.
func storeWorkRequest(
	t *testing.T, s *sessionDB, id, taskID, text string, choiceTurn *int64,
) (workTurnView, bool, error) {
	t.Helper()
	ctx := context.Background()
	db, err := s.handle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var view workTurnView
	var replayed bool
	var inner error
	err = s.workTx(ctx, db, func(tx *sql.Tx) error {
		view, replayed, inner = storeRequestAsTurn(ctx, tx, id, taskID, text, "user-1", choiceTurn, workClock())
		if inner != nil {
			// Roll the transaction back but report the refusal itself, not a
			// commit error standing in for it.
			return inner
		}
		return nil
	})
	if inner != nil {
		return view, replayed, inner
	}
	return view, replayed, err
}

// The counters are required, not optional. A writer that does not name the lane
// it read cannot be excluded from anything, so the call is refused rather than
// admitted with a guess.
func TestWorkWritesRequireTheirCASCounter(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	version, seq := turnCursor(t, s, "task-a")
	turnID := int64(mustWork(t, s, "turn_append", map[string]any{
		"task_id": "task-a", "request_id": "r1", "expected_version": version,
		"expected_turn_version": seq,
		"turn":                  map[string]any{"request": "사람의 말"},
	})["turn"].(map[string]any)["id"].(float64))

	// workCall fills a missing counter for the tests that are about something
	// else, so these go through callWork directly with the field truly absent.
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"turn_append", map[string]any{
			"task_id": "task-a", "request_id": "n1", "expected_version": version,
			"turn": map[string]any{"request": "x"},
		}},
		{"turn_patch", map[string]any{
			"task_id": "task-a", "turn_id": turnID, "request_id": "n2", "expected_version": version,
			"turn": map[string]any{"instruction": "x"},
		}},
		{"turn_dispatch", map[string]any{
			"task_id": "task-a", "turn_id": turnID, "request_id": "n3", "expected_version": version,
			"operation_key": "op", "dispatch_state": "intended",
		}},
		{taskBookPutTool, map[string]any{
			"task_id": "task-a", "request_id": "n4",
			"record": map[string]any{"stage": "s", "owner": "o", "sessions": []any{}, "details": map[string]any{}},
		}},
	} {
		raw, err := json.Marshal(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		out, rpcErr := s.callWork(context.Background(), workCaller{}, tc.name, raw)
		if rpcErr != nil {
			t.Fatalf("%s: %+v", tc.name, rpcErr)
		}
		result := out.(map[string]any)
		if result["isError"] != true {
			t.Fatalf("%s was admitted without naming its counter", tc.name)
		}
	}
}

// takeTurn claims a turn and returns its id and the claim token that holding it
// means. Dispatch needs the token, not just the timestamp.
func takeTurn(t *testing.T, s *sessionDB, taskID, requestID string, version int64) (int64, string) {
	t.Helper()
	_, seq := turnCursor(t, s, taskID)
	appended := mustWork(t, s, "turn_append", map[string]any{
		"task_id": taskID, "request_id": requestID + "-append", "expected_version": version,
		"expected_turn_version": seq, "turn": map[string]any{"request": "사람의 말"},
	})["turn"].(map[string]any)
	id := int64(appended["id"].(float64))
	_, seq = turnCursor(t, s, taskID)
	claimed := mustWork(t, s, "turn_patch", map[string]any{
		"task_id": taskID, "turn_id": id, "request_id": requestID + "-claim",
		"expected_version": version, "expected_turn_version": seq,
		"turn": map[string]any{"started_at": "2026-11-20T00:00:00Z"},
	})
	token, ok := claimed["claim_token"].(string)
	if !ok || token == "" {
		t.Fatalf("taking a turn returned no claim token: %v", claimed)
	}
	return id, token
}

// The ownership the handover claims has to be enforced, not asserted.
//
// Measured before this guard existed: a second caller that simply re-read the
// counters could record an outside action for a turn another parent was
// working — `started_at` is a timestamp, not an identity, so the counters alone
// admitted it. The claim token is what makes "only the parent that took it may
// act" true.
func TestWorkTurnDispatchNeedsTheClaimThatHoldsTheTurn(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	version, _ := turnCursor(t, s, "task-a")
	id, token := takeTurn(t, s, "task-a", "r1", version)

	// A second caller with current counters but no token is refused.
	_, seq := turnCursor(t, s, "task-a")
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": id, "request_id": "other-1",
		"expected_version": version, "expected_turn_version": seq,
		"claim_token":   "0000000000000000000000000000000f",
		"operation_key": "op-other", "operation_target": "pull/9", "dispatch_state": "dispatched",
	}, "conflict")
	// Nothing was recorded for it.
	if row := workRow(t, s, "SELECT operation_key FROM sp_work_turn_admin WHERE turn_id=?", id); row != nil {
		t.Fatalf("a caller without the claim recorded an action: %v", row)
	}

	// The holder succeeds.
	_, seq = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": id, "request_id": "d1",
		"expected_version": version, "expected_turn_version": seq,
		"claim_token":   token,
		"operation_key": "op-1", "operation_target": "pull/1", "dispatch_state": dispatchIntended,
	})

	// Handing the turn back destroys the token, so the old holder cannot keep
	// acting for work it has released.
	_, seq = turnCursor(t, s, "task-a")
	mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": id, "request_id": "handback",
		"expected_version": version, "expected_turn_version": seq,
		"turn": map[string]any{"started_at": nil},
	})
	_, seq = turnCursor(t, s, "task-a")
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": id, "request_id": "d-after-handback",
		"expected_version": version, "expected_turn_version": seq,
		"claim_token":   token,
		"operation_key": "op-1", "operation_target": "pull/1", "dispatch_state": dispatchDispatched,
	}, "conflict")

	// Re-taking it mints a different token, and the stale one stays refused.
	_, seq = turnCursor(t, s, "task-a")
	retaken := mustWork(t, s, "turn_patch", map[string]any{
		"task_id": "task-a", "turn_id": id, "request_id": "retake",
		"expected_version": version, "expected_turn_version": seq,
		"turn": map[string]any{"started_at": "2026-11-20T01:00:00Z"},
	})["claim_token"].(string)
	if retaken == token {
		t.Fatal("re-taking a turn reissued the same token")
	}
	_, seq = turnCursor(t, s, "task-a")
	wantWorkCode(t, s, "turn_dispatch", map[string]any{
		"task_id": "task-a", "turn_id": id, "request_id": "d-stale",
		"expected_version": version, "expected_turn_version": seq,
		"claim_token":   token,
		"operation_key": "op-1", "operation_target": "pull/1", "dispatch_state": dispatchDispatched,
	}, "conflict")
}

// What a recorded operation key is allowed to become. The key is the handle the
// outside system deduplicates on, so it has to keep naming the same action, and
// "handed over" cannot be unsaid.
func TestWorkTurnDispatchKeyAndStateAreNotRewritten(t *testing.T) {
	s, _ := newTestSessionDB(t)
	newWorkTask(t, s, "task-a", "c1", nil)
	version, _ := turnCursor(t, s, "task-a")
	id, token := takeTurn(t, s, "task-a", "r1", version)

	dispatch := func(requestID, key, target, state, observed string) string {
		t.Helper()
		_, seq := turnCursor(t, s, "task-a")
		args := map[string]any{
			"task_id": "task-a", "turn_id": id, "request_id": requestID,
			"expected_version": version, "expected_turn_version": seq,
			"claim_token":   token,
			"operation_key": key, "operation_target": target, "dispatch_state": state,
		}
		if observed != "" {
			args["observed"] = observed
		}
		_, code := workCall(t, s, "turn_dispatch", args)
		return code
	}

	if code := dispatch("d1", "op-1", "pull/1", dispatchIntended, ""); code != "" {
		t.Fatalf("first intent refused: %s", code)
	}
	// intended -> dispatched is the forward step and is allowed.
	if code := dispatch("d2", "op-1", "pull/1", dispatchDispatched, ""); code != "" {
		t.Fatalf("handing over refused: %s", code)
	}
	// Re-recording the same key and target under a new request_id is allowed:
	// that is what an idempotency key is for, and the outside system dedupes on
	// it. The platform records intent; it cannot make an outside call happen
	// exactly once.
	if code := dispatch("d3", "op-1", "pull/1", dispatchDispatched, ""); code != "" {
		t.Fatalf("re-recording the same key and target refused: %s", code)
	}
	// dispatched -> intended is refused: it would erase that something left.
	if code := dispatch("d4", "op-1", "pull/1", dispatchIntended, ""); code != "conflict" {
		t.Fatalf("rewinding dispatched to intended = %q, want conflict", code)
	}
	// The same key cannot be pointed at a different target.
	if code := dispatch("d5", "op-1", "pull/999", dispatchDispatched, ""); code != "conflict" {
		t.Fatalf("moving a key to another target = %q, want conflict", code)
	}
	// And what was recorded is unchanged by the refusals.
	row := workRow(t, s,
		"SELECT operation_key,operation_target,dispatch_state FROM sp_work_turn_admin WHERE turn_id=?", id)
	if row["operation_key"] != "op-1" || row["operation_target"] != "pull/1" ||
		row["dispatch_state"] != dispatchDispatched {
		t.Fatalf("a refused dispatch changed the record: %v", row)
	}
	// A different key still needs evidence the first never landed, and then it
	// may name its own target.
	if code := dispatch("d6", "op-2", "pull/2", dispatchIntended, ""); code != "conflict" {
		t.Fatalf("a second key without evidence = %q, want conflict", code)
	}
	if code := dispatch("d7", "op-2", "pull/2", dispatchIntended, "not_accepted"); code != "" {
		t.Fatalf("a second key with observed=not_accepted refused: %s", code)
	}
}
