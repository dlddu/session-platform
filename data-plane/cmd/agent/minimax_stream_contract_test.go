package main

import (
	"bytes"
	"strings"
	"testing"
)

// These fixtures are real `mcode exec --output-format stream-json` captures
// (@minimax-ai/code 0.6.12, 2026-10-22) with the envelope's run/session ids and
// timestamps dropped and long tool records shortened. Field names and nesting
// are exactly what the CLI emitted, so a change in its stream shape shows up
// here rather than as silently empty session output.

// A tool call precedes the answer. Its record nests the command's own stdout
// ("TOOL_OK\n") inside toolCall.output; none of that is assistant text.
const miniMaxToolCallCapture = `{"schemaVersion":1,"sequence":1,"type":"exec.started"}
{"schemaVersion":1,"sequence":2,"type":"session.started"}
{"schemaVersion":1,"sequence":3,"type":"turn.started"}
{"schemaVersion":1,"sequence":4,"type":"item.started","item":{"id":"call_01a163fe48e7740e9ffa42ab","type":"tool_call","toolCall":{"id":"call_01a163fe48e7740e9ffa42ab","name":"bash","status":4}}}
{"schemaVersion":1,"sequence":7,"type":"item.updated","item":{"id":"call_01a163fe48e7740e9ffa42ab","type":"tool_call","toolCall":{"id":"call_01a163fe48e7740e9ffa42ab","name":"bash","status":1,"input":{"command":"echo TOOL_OK > t.txt && cat t.txt","description":"Write and read test file"}}}}
{"schemaVersion":1,"sequence":9,"type":"item.completed","item":{"id":"call_01a163fe48e7740e9ffa42ab","type":"tool_call","toolCall":{"id":"call_01a163fe48e7740e9ffa42ab","name":"bash","status":2,"input":{"command":"echo TOOL_OK > t.txt && cat t.txt"},"output":{"content":[{"type":"text","text":"TOOL_OK\n"}],"details":{"processOutput":{"stdout":"TOOL_OK\n","stderr":"","exitCode":0}}}}}}
{"schemaVersion":1,"sequence":10,"type":"item.started","item":{"id":"6a8f1082-984e-40d8-8f01-52b10308eee8:message","type":"agent_message","contentDelta":"D"}}
{"schemaVersion":1,"sequence":11,"type":"item.updated","item":{"id":"6a8f1082-984e-40d8-8f01-52b10308eee8:message","type":"agent_message","contentDelta":"ONE — the command wrote ` + "`" + `TO"}}
{"schemaVersion":1,"sequence":12,"type":"item.updated","item":{"id":"6a8f1082-984e-40d8-8f01-52b10308eee8:message","type":"agent_message","contentDelta":"OL_OK` + "`" + ` into"}}
{"schemaVersion":1,"sequence":13,"type":"item.updated","item":{"id":"6a8f1082-984e-40d8-8f01-52b10308eee8:message","type":"agent_message","contentDelta":" ` + "`" + `"}}
{"schemaVersion":1,"sequence":14,"type":"item.updated","item":{"id":"6a8f1082-984e-40d8-8f01-52b10308eee8:message","type":"agent_message","contentDelta":"t.txt` + "`" + ` and read it back."}}
{"schemaVersion":1,"sequence":15,"type":"item.completed","item":{"id":"6a8f1082-984e-40d8-8f01-52b10308eee8:message","type":"agent_message","content":"DONE — the command wrote ` + "`TOOL_OK`" + ` into ` + "`t.txt`" + ` and read it back."}}
{"schemaVersion":1,"sequence":16,"type":"turn.completed","model":{"providerId":"custom_provider:session-platform","modelId":"MiniMax-M3"},"usage":{"inputTokens":24305,"outputTokens":73}}
{"schemaVersion":1,"sequence":17,"type":"exec.completed","result":{"type":"exec.result","status":"succeeded","output":"DONE — the command wrote ` + "`TOOL_OK`" + ` into ` + "`t.txt`" + ` and read it back."}}
`

// The reasoning item completes *between* the answer's delta and the answer's
// own completion record, and the answer is one delta with embedded newlines.
const miniMaxReasoningCapture = `{"schemaVersion":1,"sequence":1,"type":"exec.started"}
{"schemaVersion":1,"sequence":4,"type":"item.started","item":{"id":"ac66c776-5dc8-4a37-ab1e-2ffef5d381af:reasoning","type":"reasoning","contentDelta":"We need answer count "}}
{"schemaVersion":1,"sequence":5,"type":"item.updated","item":{"id":"ac66c776-5dc8-4a37-ab1e-2ffef5d381af:reasoning","type":"reasoning","contentDelta":"1 to 5 one per line."}}
{"schemaVersion":1,"sequence":6,"type":"item.started","item":{"id":"ac66c776-5dc8-4a37-ab1e-2ffef5d381af:message","type":"agent_message","contentDelta":"1\n2\n3\n4\n5"}}
{"schemaVersion":1,"sequence":7,"type":"item.completed","item":{"id":"ac66c776-5dc8-4a37-ab1e-2ffef5d381af:reasoning","type":"reasoning","content":"We need answer count 1 to 5 one per line."}}
{"schemaVersion":1,"sequence":8,"type":"item.completed","item":{"id":"ac66c776-5dc8-4a37-ab1e-2ffef5d381af:message","type":"agent_message","content":"1\n2\n3\n4\n5"}}
{"schemaVersion":1,"sequence":9,"type":"turn.completed","model":{"modelId":"MiniMax-M3.1-Flash-Preview"}}
{"schemaVersion":1,"sequence":10,"type":"exec.completed","result":{"type":"exec.result","status":"succeeded","output":"1\n2\n3\n4\n5"}}
`

// A failed turn: nothing to project from stdout, because the CLI reports the
// same failure on stderr.
const miniMaxFailedTurnCapture = `{"schemaVersion":1,"sequence":1,"type":"exec.started"}
{"schemaVersion":1,"sequence":4,"type":"turn.failed","status":"failed","error":{"category":"runtime","message":"Model \"custom_provider:session-platform/Nope-Model\" is not available for the \"custom_provider\" route (preset cn-prod).","retryable":true},"durationMs":252}
{"schemaVersion":1,"sequence":5,"type":"exec.completed","result":{"type":"exec.result","status":"failed","error":{"category":"runtime","message":"Model is not available"}}}
`

func TestMiniMaxStreamContract(t *testing.T) {
	for _, tc := range []struct {
		name, stream, want string
	}{
		{"tool call then answer", miniMaxToolCallCapture, "DONE — the command wrote `TOOL_OK` into `t.txt` and read it back.\n"},
		{"reasoning interleaved", miniMaxReasoningCapture, "1\n2\n3\n4\n5\n"},
		{"failed turn", miniMaxFailedTurnCapture, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 3, 64, len(tc.stream)} {
				var out bytes.Buffer
				p := newMiniMaxStreamProjector(&out)
				for _, chunk := range splitEvery(tc.stream, size) {
					if _, err := p.Write([]byte(chunk)); err != nil {
						t.Fatalf("write: %v", err)
					}
				}
				if err := p.Close(); err != nil {
					t.Fatalf("close: %v", err)
				}
				if out.String() != tc.want {
					t.Fatalf("chunk size %d: projected %q, want %q", size, out.String(), tc.want)
				}
				if strings.Contains(out.String(), "TOOL_OK\n") && !strings.Contains(tc.want, "TOOL_OK\n") {
					t.Fatalf("tool stdout leaked into the session output: %q", out.String())
				}
			}
		})
	}
}
