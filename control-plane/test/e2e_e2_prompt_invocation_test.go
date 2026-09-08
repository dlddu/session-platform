//go:build e2e

// 검증 시나리오: claude-code-workload.md#시나리오 2
//
// docs/prd/claude-code-workload.md AC-E2, driven against the in-cluster provider
// stand-in registered as `CLAUDE-PROVIDER` in docs/test/e2e.md.
//
// What this file deliberately does NOT assert, and why:
//
//   - that write returns *before* its invocation finishes. Separating that from
//     a blocking write needs an invocation that outlasts the write's own round
//     trip, and on this SUT it does not: a measured burst of four prompts took
//     3.4s to issue and all four had already answered inside that window. So
//     neither a clock nor a queue-depth count can tell the two apart here, and
//     any assertion would be a coin flip. data-plane/cmd/agent/claude_test.go's
//     TestClaudeWriteIsNonBlockingAndSerial holds a fake runner open instead,
//     which is the method AC-E2's verification text prescribes for this clause.
//   - 429 on a saturated queue: filling `maxClaudeQueuedPrompts` means racing
//     the drain rate rather than observing a contract.
//   - the 16 MiB per-invocation truncation marker: the stand-in cannot be made
//     to emit that much.
//   - `--continue` staying off after a *failed* first run: no way to fail an
//     invocation on the SUT without breaking the wiring the other assertions
//     need.
package e2e_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The prompts are distinguishable so a captured argv can be attributed to the
// write that caused it. Their text is irrelevant to the stand-in, which answers
// the same constant either way.
const (
	e2PromptFirst  = "e2-prompt-first"
	e2PromptSecond = "e2-prompt-second"
)

// What may precede this run of flags is `--continue` (only after a successful
// run) and `--model <effective model>`; what follows `-- ` is the prompt as a
// single positional argument.
const e2InvariantArgv = "--permission-mode auto -p --output-format stream-json " +
	"--verbose --include-partial-messages -- "

// Prints one line per *change*: "<count>\t<argv>". $1 is the sampling window in
// seconds; it stops early once a resume invocation has come and gone.
//
// It must not count anything but the invocation, and two things would fool a
// naive match. Its own command line contains the flags it matches on, and so
// does every subshell a command substitution forks, since those inherit the
// parent's command line — skipping $$ alone is not enough, which is why the
// lowercase sentinel below marks every process this probe owns. And a process
// that merely *mentions* the flags (any shell running a script that contains
// them) is not an invocation either, so argv[0] must be the CLI itself. Both
// guards were added because a dry run without them counted the wrong process.
//
// The sentinel is deliberately not spelled with an uppercase E2E_ prefix: that
// shape is a fidelity-allowlist seam token (scripts/check-fidelity-allowlist.py),
// and this is a probe, not a substitution.
const claudeArgvProbe = `# claude-argv-probe-self
end=$(($(date +%s) + $1))
prev=0
prevcmd=
sawcont=0
while [ "$(date +%s)" -lt "$end" ]; do
	n=0
	seen=
	for d in /proc/[0-9]*; do
		[ -r "$d/cmdline" ] || continue
		c=$(tr '\0' ' ' <"$d/cmdline" 2>/dev/null) || continue
		case "$c" in
		*claude-argv-probe-self*) continue ;;
		*--include-partial-messages*) ;;
		*) continue ;;
		esac
		case "${c%% *}" in
		*claude) ;;
		*) continue ;;
		esac
		n=$((n + 1))
		seen=$c
	done
	if [ "$n" != "$prev" ] || [ "$seen" != "$prevcmd" ]; then
		printf '%s\t%s\n' "$n" "$seen"
	fi
	case "$seen" in *--continue*) sawcont=1 ;; esac
	prev=$n
	prevcmd=$seen
	if [ "$sawcont" = 1 ] && [ "$n" -eq 0 ]; then
		break
	fi
	sleep 0.05
done
`

// The model is left unset, so the session takes the `platform-default` alias and
// the pod resolves it from the credentials Secret's optional `model` key.
func claudeSession(t *testing.T) typedSession {
	t.Helper()
	status, s := createTyped(t, map[string]any{
		"name": uniqueName(t), "workloadType": "claude-code",
	})
	if status != http.StatusCreated {
		t.Fatalf("create claude-code session: status=%d", status)
	}
	if s.Pod == "" {
		t.Fatal("claude-code session has no dedicated pod")
	}
	t.Cleanup(func() {
		if resp, raw := do(t, http.MethodDelete, "/api/v1/sessions/"+s.ID, nil); resp.StatusCode >= 400 {
			t.Logf("cleanup delete %s: status=%d body=%s", s.ID, resp.StatusCode, raw)
		}
	})
	return s
}

func writePrompt(t *testing.T, id, prompt string) (int, []byte) {
	t.Helper()
	resp, body := do(t, http.MethodPost, "/api/v1/sessions/"+id+"/write", map[string]string{"payload": prompt})
	return resp.StatusCode, body
}

func writePromptOK(t *testing.T, id, prompt string) {
	t.Helper()
	if status, body := writePrompt(t, id, prompt); status != http.StatusOK {
		t.Fatalf("write prompt %q: status=%d body=%s", prompt, status, body)
	}
}

// The deadline is far longer than the shell suite's: an invocation is a cold CLI
// start (process launch, settings load, provider round trip), not a line into a
// live bash.
func eventuallyClaudeOutput(t *testing.T, id string, within time.Duration, ok func(string) bool) readResp {
	t.Helper()
	deadline := time.Now().Add(within)
	var r readResp
	for {
		r = readShellAt(t, id, 0)
		if ok(r.Payload) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s output never matched within %v; last payload=%q", id, within, r.Payload)
		}
		time.Sleep(time.Second)
	}
}

// The marker exists nowhere else in the tree, so finding it in the buffer says a
// real `claude` invocation ran and the agent projected its assistant text — the
// whole loop, not just a reachable endpoint.
func TestClaudePromptWrite_QueuesEveryPromptAndRunsEachOne(t *testing.T) {
	s := claudeSession(t)

	before := readShellAt(t, s.ID, 0)

	const burst = 4
	start := time.Now()
	for i := 0; i < burst; i++ {
		writePromptOK(t, s.ID, e2PromptFirst)
	}
	issued := time.Since(start)

	after := eventuallyClaudeOutput(t, s.ID, 5*time.Minute, func(p string) bool {
		return strings.Count(p, providerReplyMarker) >= burst
	})
	if got := strings.Count(after.Payload, providerReplyMarker); got != burst {
		t.Fatalf("%d prompts produced %d replies, want exactly %d — every accepted prompt runs once "+
			"(AC-E2); payload=%q", burst, got, burst, after.Payload)
	}
	if after.NextOffset <= before.NextOffset {
		t.Fatalf("cursor did not advance across %d invocations: before=%d after=%d",
			burst, before.NextOffset, after.NextOffset)
	}
	t.Logf("issued %d prompts in %v; all %d replies present after %v",
		burst, issued, burst, time.Since(start))
}

// Argv, one-shot lifetime and serial queueing are all properties of the
// *process*, so one probe of the container's process table covers all three.
func TestClaudePromptInvocation_ExactArgvOneShotAndSerialised(t *testing.T) {
	cs, cfg, ok := kubeClient(t)
	if !ok {
		t.Skip("no cluster access; the process-level assertions need the deployed SUT")
	}
	ns := sessionNamespace()
	s := claudeSession(t)

	// Start sampling before the writes, so the first invocation cannot slip in
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

	writePromptOK(t, s.ID, e2PromptFirst)
	writePromptOK(t, s.ID, e2PromptSecond)

	eventuallyClaudeOutput(t, s.ID, 5*time.Minute, func(p string) bool {
		return strings.Count(p, providerReplyMarker) >= 2
	})

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
		lastCount = n
		if n > maxConcurrent {
			maxConcurrent = n
		}
		if argv = strings.TrimSpace(argv); argv != "" {
			if len(observed) == 0 || observed[len(observed)-1] != argv {
				observed = append(observed, argv)
			}
		}
	}

	if len(observed) < 2 {
		t.Fatalf("the probe saw %d invocation(s), want 2 — the CLI must run once per accepted write (AC-E2); "+
			"probe output=%q", len(observed), res.out)
	}
	if maxConcurrent > 1 {
		t.Fatalf("%d `claude` processes ran at once — a session serialises its invocations (AC-E2); "+
			"probe output=%q", maxConcurrent, res.out)
	}
	if lastCount != 0 {
		t.Fatalf("the probe stopped while %d `claude` process(es) were still running — invocations are "+
			"one-shot and leave no resident CLI (AC-E2); probe output=%q", lastCount, res.out)
	}

	first, second := observed[0], observed[1]
	for _, argv := range []string{first, second} {
		// argv[0] is the configured binary — `claude` unless CLAUDE_CODE_BIN
		// names a path, which no manifest in this repo does.
		if arg0, _, _ := strings.Cut(argv, " "); !strings.HasSuffix(arg0, "claude") {
			t.Fatalf("invocation argv[0] = %q, want the `claude` binary (AC-E2); full argv=%q", arg0, argv)
		}
		if strings.Contains(argv, "--dangerously-skip-permissions") {
			t.Fatalf("invocation used --dangerously-skip-permissions: %q — platform policy is "+
				"--permission-mode auto (AC-E2 구현 결정)", argv)
		}
	}
	if want := e2InvariantArgv + e2PromptFirst; !strings.Contains(first, want) {
		t.Fatalf("first invocation argv = %q, want it to contain %q (AC-E2 exact argv)", first, want)
	}
	if want := e2InvariantArgv + e2PromptSecond; !strings.Contains(second, want) {
		t.Fatalf("second invocation argv = %q, want it to contain %q (AC-E2 exact argv)", second, want)
	}

	if strings.Contains(first, "--continue") {
		t.Fatalf("the first invocation carried --continue: %q — a session's first prompt starts a new "+
			"conversation (AC-E2)", first)
	}
	if !strings.Contains(second, "--continue") {
		t.Fatalf("the second invocation did not carry --continue: %q — writes after a successful run "+
			"resume the conversation (AC-E2)", second)
	}
}

// The limit is checked before the session is even activated, so the output
// cursor cannot move.
func TestClaudePromptWrite_RejectsOverSizedPromptWithoutRunningIt(t *testing.T) {
	s := claudeSession(t)

	before := readShellAt(t, s.ID, 0)

	oversized := strings.Repeat("x", (1<<20)+1)
	status, body := writePrompt(t, s.ID, oversized)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("write of a %d byte prompt: status=%d body=%s, want 413 (AC-E2 1 MiB limit)",
			len(oversized), status, body)
	}
	if after := readShellAt(t, s.ID, 0); after.NextOffset != before.NextOffset {
		t.Fatalf("output cursor moved %d -> %d on a rejected prompt — the limit is checked before the "+
			"session is activated, so nothing may run (AC-E2); payload=%q",
			before.NextOffset, after.NextOffset, after.Payload)
	}

	// A second, accepted prompt is what proves the rejection never entered the
	// queue: prompts run in order, so had the oversized one been admitted its
	// reply would sit in the buffer ahead of this one's. Exactly one marker
	// after this invocation settles means exactly one invocation ever ran — and
	// it also shows a 413 is per-write rather than terminal for the session.
	writePromptOK(t, s.ID, e2PromptFirst)
	eventuallyClaudeOutput(t, s.ID, 3*time.Minute, func(p string) bool {
		return strings.Contains(p, providerReplyMarker)
	})
	time.Sleep(10 * time.Second)
	if got := readShellAt(t, s.ID, 0); strings.Count(got.Payload, providerReplyMarker) != 1 {
		t.Fatalf("session output carries %d provider replies, want exactly 1 — the rejected prompt must "+
			"never have run (AC-E2); payload=%q",
			strings.Count(got.Payload, providerReplyMarker), got.Payload)
	}
}
