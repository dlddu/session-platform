//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 3
//
// 이 파일이 사는 것과 시나리오 2·4 에 남기는 경계, 그리고 겹침 창을 크기가 아니라
// 델타 간격으로 만든 경위는 docs/test/e2e.md 의 매핑 행이 갖는다. 지시자의 문법과
// 그 상한(MAX_DELTAS·MAX_DELAY_MS)은 deploy/e2e-anthropic-fake.yaml 의 헤더가 갖는다.
package e2e_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Byte counts are written out rather than derived from each other, so that a
// change on either side fails here instead of quietly agreeing with itself.
const (
	cc3SlowPrompt  = "e2e-reply:ccthree-slow:4000:64:150"
	cc3SlowOpenLen = agentTurnInputMarkerLen + 34 + 1 + agentTurnOutputMarkerLen
	cc3SlowPrefix  = "ccthree-slow:가"
	cc3SlowRunes   = 4000
	cc3SlowDeltas  = 64
	cc3SlowDelayMs = 150
	cc3SlowFloorMs = cc3SlowDeltas * cc3SlowDelayMs
	cc3SlowBytes   = 13 + 3*cc3SlowRunes

	cc3FastPrompt  = "e2e-reply:ccthree-fast:40:1"
	cc3FastOpenLen = agentTurnInputMarkerLen + 27 + 1 + agentTurnOutputMarkerLen
	cc3FastPrefix  = "ccthree-fast:가"
	cc3FastRunes   = 40
	cc3FastBytes   = 13 + 3*cc3FastRunes
)

// One per invocation; data-plane/cmd/agent/claude_stream.go finishMessage owns the rule.
const cc3Terminator = 1

const (
	agentTurnLead            = "\x1esession-platform "
	agentTurnInputMarkerLen  = 18 + 15 + 20 + 1
	agentTurnOutputMarkerLen = 18 + 12 + 1
	agentTurnEndOKMarkerLen  = 18 + 23 + 20 + 1
)

func agentTurnInput(seq int) string { return agentTurnLead + "input seq=" + strconv.Itoa(seq) + " at=" }

func agentTurnEndOK(seq int) string {
	return agentTurnLead + "end seq=" + strconv.Itoa(seq) + " status=ok at="
}

func cc3IssueOverlapped(t *testing.T, id string) readResp {
	t.Helper()
	if before := readShellAt(t, id, 0); before.NextOffset != 0 {
		t.Fatalf("a fresh claude-code session already holds %d bytes of output; this file reads "+
			"order off absolute positions and cannot start from a dirty buffer", before.NextOffset)
	}

	writePromptOK(t, id, cc3SlowPrompt)
	writePromptOK(t, id, cc3FastPrompt)
	mid := readShellAt(t, id, 0)

	if mid.NextOffset >= cc3SlowOpenLen+cc3SlowBytes {
		t.Fatalf("the first reply (%d bytes) had already fully landed (%d bytes buffered) by the time "+
			"the second write returned — the two writes never overlapped, so nothing below would be "+
			"measuring serialisation. The stand-in was asked to spend at least %dms on it, so either "+
			"the delta gap stopped being honoured or this SUT got dramatically faster; raise "+
			"cc3SlowDelayMs (MAX_DELAY_MS in deploy/e2e-anthropic-fake.yaml is the ceiling). Note "+
			"that raising the *size* does not help — 600013 bytes was measured landing inside one "+
			"write's round trip, which is why the gap exists.",
			cc3SlowBytes, mid.NextOffset-cc3SlowOpenLen, cc3SlowFloorMs)
	}
	if strings.Contains(mid.Payload, cc3FastPrefix) {
		t.Fatalf("the second reply was already in the buffer when its own write returned (%d bytes "+
			"buffered) — it must queue behind the running invocation, not run beside it (AC-E2)",
			mid.NextOffset)
	}
	t.Logf("second write accepted with %d/%d bytes of the first reply buffered", mid.NextOffset, cc3SlowBytes)
	return mid
}

// The settle loop is not belt and braces: the message-terminating newline lands
// after the last delta, so a wait that stops at the byte threshold can hand back a
// buffer that is still growing, and the assertions below read positions out of it.
func cc3Settled(t *testing.T, id string) readResp {
	t.Helper()
	r := eventuallyClaudeOutput(t, id, 5*time.Minute, func(p string) bool {
		return len(p) >= cc3SlowOpenLen+cc3SlowBytes+cc3FastOpenLen+cc3FastBytes
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

func TestClaudeSerialQueue_ReadZeroAccumulatesInWriteOrder(t *testing.T) {
	s := claudeSession(t)
	cc3IssueOverlapped(t, s.ID)

	full := cc3Settled(t, s.ID)

	slow := strings.Index(full.Payload, cc3SlowPrefix)
	fast := strings.Index(full.Payload, cc3FastPrefix)
	if slow < 0 || fast < 0 {
		t.Fatalf("read(0) is missing a reply (slow=%d fast=%d) in %d bytes", slow, fast, full.NextOffset)
	}
	if !strings.HasPrefix(full.Payload, agentTurnInput(1)) || slow != cc3SlowOpenLen ||
		full.Payload[agentTurnInputMarkerLen:agentTurnInputMarkerLen+len(cc3SlowPrompt)+1] != cc3SlowPrompt+"\n" {
		t.Fatalf("the first reply starts at byte %d, want %d right after turn 1's input block carrying %q — "+
			"the buffer was empty before the writes, so nothing else may precede it (AC-E3)",
			slow, cc3SlowOpenLen, cc3SlowPrompt)
	}
	// Position rather than relative order: interleaved replies would still satisfy
	// a naive `slow < fast`, and interleaving is what a concurrent queue produces.
	end1 := strings.Index(full.Payload, agentTurnEndOK(1))
	open2 := strings.Index(full.Payload, agentTurnInput(2))
	if end1 != cc3SlowOpenLen+cc3SlowBytes+cc3Terminator || open2 != end1+agentTurnEndOKMarkerLen || fast != open2+cc3FastOpenLen {
		t.Fatalf("turn 1 ends at byte %d, turn 2 opens at %d and its reply starts at %d, want %d, %d and %d — "+
			"a session runs its prompts one at a time, closes each turn before the next opens, and appends "+
			"in write order (AC-E2/E3)", end1, open2, fast, cc3SlowOpenLen+cc3SlowBytes+cc3Terminator,
			cc3SlowOpenLen+cc3SlowBytes+cc3Terminator+agentTurnEndOKMarkerLen,
			cc3SlowOpenLen+cc3SlowBytes+cc3Terminator+agentTurnEndOKMarkerLen+cc3FastOpenLen)
	}
	// A drain that reordered *and* replayed a prompt could still pass the position
	// checks above; the counts and the total length are what rule that out.
	if got := strings.Count(full.Payload, cc3SlowPrefix); got != 1 {
		t.Fatalf("the first reply's label appears %d times, want exactly 1 — one accepted write is "+
			"one run (AC-E2)", got)
	}
	if got := strings.Count(full.Payload, cc3FastPrefix); got != 1 {
		t.Fatalf("the second reply's label appears %d times, want exactly 1 — one accepted write is "+
			"one run (AC-E2)", got)
	}
	if want := int64(cc3SlowOpenLen + cc3SlowBytes + cc3FastOpenLen + cc3FastBytes + 2*(cc3Terminator+agentTurnEndOKMarkerLen)); full.NextOffset != want ||
		strings.LastIndex(full.Payload, agentTurnEndOK(2)) != len(full.Payload)-agentTurnEndOKMarkerLen {
		t.Fatalf("buffer is %d bytes, want exactly %d ending with turn 2's ok end marker",
			full.NextOffset, want)
	}
	t.Logf("slow reply at [%d,%d), fast reply at [%d,%d), buffer settled at %d bytes",
		slow, slow+cc3SlowBytes, fast, fast+cc3FastBytes, full.NextOffset)
}

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
