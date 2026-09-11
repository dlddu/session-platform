//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 3
//
// docs/prd/claude-code-workload.md AC-E2, driven against the in-cluster provider
// stand-in registered as `CLAUDE-PROVIDER` in docs/test/e2e.md.
//
// The scenario is about *two* writes where the second is issued before the first
// invocation has finished, and what the platform owes for that is two things:
// the runs do not overlap, and the output accumulates in the order the writes
// were sent.
//
// Both halves need the first invocation to still be running when the second
// write is accepted, and on this SUT that window does not occur by itself.
// e2e_e2_prompt_invocation_test.go's header already records that an invocation
// is not slower than a write's own round trip; this file measured how far that
// goes. Asking the stand-in for the largest body a directive could name — 200000
// runes, 600013 bytes, 64 deltas — still left the whole reply on disk before the
// *second* write returned (buffer 600148 = both replies complete). Size cannot
// buy time here.
//
// So the directive carries a delta gap instead (the optional fourth field added
// to deploy/e2e-anthropic-fake.yaml for this file), and the first prompt asks for
// 64 deltas 150ms apart — a reply that takes ~9.6s to finish against a second one
// that takes well under a second. That inversion does double duty:
//
//   - it creates the window, so cc3IssueOverlapped can prove from the buffer that
//     the second write really was accepted mid-flight rather than assuming it;
//   - it makes the ordering assertion non-vacuous. The second reply is both
//     shorter (133 bytes against 12013) and faster, so a session that ran its
//     queue concurrently, or drained it out of order, would land it first. Two
//     equal replies would satisfy the same assertion under any implementation.
//
// What this file deliberately does NOT assert, and why:
//
//   - that a write returns before its own invocation finishes. That clause
//     belongs to 시나리오 2 and stays where 시나리오 2's file left it:
//     data-plane/cmd/agent/claude_test.go's TestClaudeWriteIsNonBlockingAndSerial,
//     which holds a fake runner open. This file needs only the weaker fact that
//     the *second* write is accepted mid-flight, which it can observe directly.
//   - exact argv, one-shot lifetime and `--continue`. 시나리오 2 owns all three;
//     the process probe is reused here only to count concurrent invocations and
//     to read off which prompt each one carried, never to check the flags.
//   - the reply's meaning. The stand-in reflects a directive's label and size and
//     nothing else, so conversational ordering of *content* stays with
//     시나리오 5's exception.
//   - byte-level cursor contracts on the way out (chunk boundaries, resume,
//     reset). Those are 시나리오 4's, driven through the same stand-in.
package e2e_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The directive pair and the replies it produces. Byte counts are written out
// rather than derived from each other, so that a change on either side fails
// here instead of quietly agreeing with itself; MAX_DELTAS and MAX_DELAY_MS in
// deploy/e2e-anthropic-fake.yaml are the ceilings on the first one's duration.
const (
	// 64 deltas, 150ms apart: the reply cannot finish in less than ~9.6s, which
	// is the window the second write has to land in.
	cc3SlowPrompt  = "e2e-reply:ccthree-slow:4000:64:150"
	cc3SlowPrefix  = "ccthree-slow:"
	cc3SlowRunes   = 4000
	cc3SlowDeltas  = 64
	cc3SlowDelayMs = 150
	cc3SlowFloorMs = cc3SlowDeltas * cc3SlowDelayMs
	// len("ccthree-slow:") + 3 bytes per filler rune (U+AC00).
	cc3SlowBytes = 13 + 3*cc3SlowRunes

	// No fourth field: this one is as fast as the stand-in can answer.
	cc3FastPrompt = "e2e-reply:ccthree-fast:40:1"
	cc3FastPrefix = "ccthree-fast:"
	cc3FastRunes  = 40
	cc3FastBytes  = 13 + 3*cc3FastRunes
)

// The projector closes a message with a newline when the assistant text does not
// already end in one (data-plane/cmd/agent/claude_stream.go finishMessage), and
// two invocations run here.
const cc3Terminators = 2

// cc3IssueOverlapped sends the slow prompt and then the fast one with nothing in
// between, and returns the buffer as it stood the moment the second write
// returned.
//
// That buffer is the scenario's precondition, not a formality. If it already
// held the whole slow reply then the first invocation had finished before the
// second write was even accepted, there was no overlap to observe, and every
// assertion downstream would be measuring a queue that was never contended. That
// is a fact about how fast this SUT is rather than about the platform, so the
// helper says so loudly instead of letting the run pass quietly.
func cc3IssueOverlapped(t *testing.T, id string) readResp {
	t.Helper()
	if before := readShellAt(t, id, 0); before.NextOffset != 0 {
		t.Fatalf("a fresh claude-code session already holds %d bytes of output; this file reads "+
			"order off absolute positions and cannot start from a dirty buffer", before.NextOffset)
	}

	writePromptOK(t, id, cc3SlowPrompt)
	writePromptOK(t, id, cc3FastPrompt)
	mid := readShellAt(t, id, 0)

	if mid.NextOffset >= cc3SlowBytes {
		t.Fatalf("the first reply (%d bytes) had already fully landed (%d bytes buffered) by the time "+
			"the second write returned — the two writes never overlapped, so nothing below would be "+
			"measuring serialisation. The stand-in was asked to spend at least %dms on it, so either "+
			"the delta gap stopped being honoured or this SUT got dramatically faster; raise "+
			"cc3SlowDelayMs (MAX_DELAY_MS in deploy/e2e-anthropic-fake.yaml is the ceiling). Note "+
			"that raising the *size* does not help — 600013 bytes was measured landing inside one "+
			"write's round trip, which is why the gap exists.",
			cc3SlowBytes, mid.NextOffset, cc3SlowFloorMs)
	}
	// The negative half of the same moment: the queue may not have started the
	// second prompt beside the first one.
	if strings.Contains(mid.Payload, cc3FastPrefix) {
		t.Fatalf("the second reply was already in the buffer when its own write returned (%d bytes "+
			"buffered) — it must queue behind the running invocation, not run beside it (AC-E2)",
			mid.NextOffset)
	}
	t.Logf("second write accepted with %d/%d bytes of the first reply buffered", mid.NextOffset, cc3SlowBytes)
	return mid
}

// cc3Settled waits for both replies to land and for the buffer to stop moving.
//
// Reaching the byte count is not enough on its own: the slow reply arrives as 64
// deltas and its terminating newline lands after the last of them, so a wait that
// stopped at the threshold could hand back a buffer that is still growing — and
// the assertions here read positions out of a buffer they assume is final.
func cc3Settled(t *testing.T, id string) readResp {
	t.Helper()
	r := eventuallyClaudeOutput(t, id, 5*time.Minute, func(p string) bool {
		return len(p) >= cc3SlowBytes+cc3FastBytes
	})
	deadline := time.Now().Add(time.Minute)
	for {
		time.Sleep(2 * time.Second)
		again := readShellAt(t, id, 0)
		if again.NextOffset == r.NextOffset {
			return again
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s output never stopped growing (%d -> %d)", id, r.NextOffset, again.NextOffset)
		}
		r = again
	}
}

// The headline: the two replies sit in the buffer in the order their writes were
// issued, even though the first is ninety times the size of the second, takes
// twenty times as long, and was still streaming when the second was accepted.
func TestClaudeSerialQueue_ReadZeroAccumulatesInWriteOrder(t *testing.T) {
	s := claudeSession(t)
	cc3IssueOverlapped(t, s.ID)

	full := cc3Settled(t, s.ID)

	slow := strings.Index(full.Payload, cc3SlowPrefix)
	fast := strings.Index(full.Payload, cc3FastPrefix)
	if slow < 0 || fast < 0 {
		t.Fatalf("read(0) is missing a reply (slow=%d fast=%d) in %d bytes", slow, fast, full.NextOffset)
	}
	if slow != 0 {
		t.Fatalf("the first reply starts at byte %d, want 0 — the buffer was empty before the writes, "+
			"so nothing may precede it", slow)
	}
	// Position rather than relative order: the short reply has to begin after the
	// long one's last byte. Two replies that interleaved would still satisfy a
	// naive `slow < fast`, and interleaving is exactly what a concurrent queue
	// would produce here.
	if fast < cc3SlowBytes {
		t.Fatalf("the second reply starts at byte %d, inside the first reply's %d bytes — a session "+
			"runs its prompts one at a time and appends in write order (AC-E2)", fast, cc3SlowBytes)
	}
	if got := strings.Count(full.Payload, cc3SlowPrefix); got != 1 {
		t.Fatalf("the first reply's label appears %d times, want exactly 1 — one accepted write is "+
			"one run (AC-E2)", got)
	}
	if got := strings.Count(full.Payload, cc3FastPrefix); got != 1 {
		t.Fatalf("the second reply's label appears %d times, want exactly 1 — one accepted write is "+
			"one run (AC-E2)", got)
	}
	// Nothing else got in, and nothing ran twice. A drain that reordered *and*
	// replayed a prompt could still pass the position checks; this one it cannot.
	if want := int64(cc3SlowBytes + cc3FastBytes); full.NextOffset < want || full.NextOffset > want+cc3Terminators {
		t.Fatalf("buffer is %d bytes, want %d plus at most %d terminating byte(s)",
			full.NextOffset, want, cc3Terminators)
	}
	t.Logf("slow reply at [0,%d), fast reply at [%d,%d), buffer settled at %d bytes",
		cc3SlowBytes, fast, fast+cc3FastBytes, full.NextOffset)
}

// The same overlap watched from inside the pod: the second prompt is accepted
// while the first is running, and the CLI still never runs twice at once.
//
// 시나리오 2's file counts the same thing with the same probe, but it issues a
// burst of equal prompts and its own header records that each of them had
// answered before the next was issued — so that count may have had no window to
// find anything in. Here the window is established first, by cc3IssueOverlapped,
// which is what makes the number below mean something.
func TestClaudeSerialQueue_InvocationsNeverRunConcurrently(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; counting concurrent invocations needs the deployed SUT")
	}
	ns := sessionNamespace()
	s := claudeSession(t)

	// Sampling starts before the writes so the first invocation cannot slip in
	// between session creation and the probe attaching.
	type probeResult struct {
		out    string
		stderr string
		err    error
	}
	done := make(chan probeResult, 1)
	const probeWindow = 240
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), (probeWindow+30)*time.Second)
		defer cancel()
		out, errOut, err := execInContainer(ctx, cs, cfg, ns, s.Pod, workloadContainer,
			[]string{"/bin/sh", "-c", claudeArgvProbe, "probe", strconv.Itoa(probeWindow)})
		done <- probeResult{out: out, stderr: errOut, err: err}
	}()
	time.Sleep(3 * time.Second)

	cc3IssueOverlapped(t, s.ID)
	cc3Settled(t, s.ID)

	res := <-done
	if res.err != nil {
		t.Fatalf("argv probe in %s: %v (stderr=%q)", s.Pod, res.err, res.stderr)
	}

	// The probe prints one line per *change*, of either the count or the command
	// line, so the unit of an invocation is a distinct argv and not a rising
	// count. Counting rising edges undercounts here, and measurably so: this SUT
	// hands one invocation over to the next inside a single 50ms sample, which
	// shows up as `1 <slow argv>` followed by `1 <fast argv>` with no zero in
	// between. That sequence is a serial handoff — the count never reached 2 —
	// but an edge counter reads it as one invocation.
	var (
		maxConcurrent int
		observed      []string
		lastCount     = -1
	)
	for _, line := range strings.Split(res.out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		countField, argv, _ := strings.Cut(line, "\t")
		n, err := strconv.Atoi(strings.TrimSpace(countField))
		if err != nil {
			t.Fatalf("probe emitted an unparseable line %q (full output=%q)", line, res.out)
		}
		if n > maxConcurrent {
			maxConcurrent = n
		}
		lastCount = n
		if argv = strings.TrimSpace(argv); argv != "" {
			if len(observed) == 0 || observed[len(observed)-1] != argv {
				observed = append(observed, argv)
			}
		}
	}

	if len(observed) < 2 {
		t.Fatalf("the probe saw %d invocation(s), want 2 — both accepted writes must run (AC-E2); "+
			"probe output=%q", len(observed), res.out)
	}
	if maxConcurrent > 1 {
		t.Fatalf("%d `claude` processes ran at once — the second write was accepted while the first "+
			"invocation was still running, and a session must still run them one after the other "+
			"(AC-E2); probe output=%q", maxConcurrent, res.out)
	}
	if lastCount != 0 {
		t.Fatalf("the probe stopped while %d `claude` process(es) were still running — invocations are "+
			"one-shot (AC-E2); probe output=%q", lastCount, res.out)
	}

	// The same ordering the buffer shows, one layer down: the processes ran in
	// the order the writes were issued. The buffer could in principle be ordered
	// by something downstream of execution; this cannot.
	if !strings.Contains(observed[0], cc3SlowPrompt) {
		t.Fatalf("the first invocation ran %q, want the prompt written first (%s) — the queue runs "+
			"prompts in the order they were accepted (AC-E2)", observed[0], cc3SlowPrompt)
	}
	if !strings.Contains(observed[1], cc3FastPrompt) {
		t.Fatalf("the second invocation ran %q, want the prompt written second (%s) (AC-E2)",
			observed[1], cc3FastPrompt)
	}
	t.Logf("probe saw %d invocation(s) in write order, never more than %d running at once",
		len(observed), maxConcurrent)
}
