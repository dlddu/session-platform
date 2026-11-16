#!/usr/bin/env node
// recorder-selftest — probe.mjs 의 **기록·판정 로직만** 검증한다. SDK 도 CLI 도 띄우지 않고
// 인증도 쓰지 않는다. 합성 이벤트를 먹여 「무엇이 기록되고 무엇이 기록되지 않는가」와
// 판정 규칙이 의도대로 도는가를 본다.
//
// 이것은 SDK 호출 순서의 증거가 **아니다**. 순서는 probe.mjs 를 인증된 환경에서 실제로
// 돌려야만 알 수 있고, 이 파일이 초록인 것은 그 증거를 대신하지 못한다.

import assert from "node:assert/strict";
import { createRecorder, verdictFor } from "./probe.mjs";

let clock = 1000;
const now = () => clock;

// ── 기록 대상은 화이트리스트한 칸뿐이다: 모델 문장도 도구 인자도 들어오지 않는다.
{
  const rec = createRecorder(now);
  clock = 1005;
  rec.observe({
    type: "assistant",
    message: { content: [{ type: "text", text: "SECRET-TEXT" }, { type: "tool_use", name: "Read", input: { file_path: "/etc/passwd" } }] },
  });
  clock = 1010;
  rec.observe({ type: "system", subtype: "permission_denied", tool_name: "Bash", decision_reason_type: "classifier", message: "SECRET-DENY-MESSAGE" });
  clock = 1012;
  rec.observe({ type: "user", message: { content: [{ type: "tool_result", is_error: true, content: "SECRET-RESULT" }] } });
  clock = 1020;
  rec.observe({ type: "result", subtype: "success", is_error: false, result: "SECRET-ANSWER" });

  const serialized = JSON.stringify(rec.summary());
  // 이벤트 종류 이름(permission_denied)과 겹치지 않는 값으로 둔다 — 겹치면 검사가 거짓 양성이 된다.
  for (const leak of ["SECRET-TEXT", "SECRET-RESULT", "SECRET-ANSWER", "SECRET-DENY-MESSAGE", "/etc/passwd"]) {
    assert.ok(!serialized.includes(leak), `recorder leaked ${leak}`);
  }
  assert.deepEqual(
    rec.events.map((e) => [e.atMs, e.kind]),
    [[5, "assistant"], [10, "permission_denied"], [12, "tool_result"], [20, "result"]],
  );
  assert.equal(rec.events[0].tool, "Read");
  assert.equal(rec.events[1].decisionReasonType, "classifier");
  assert.equal(rec.events[2].isError, true);
  assert.equal(rec.counts.permission_denied, 1);
}

// ── 훅 수명 이벤트는 훅 이름만 남는다.
{
  const rec = createRecorder(now);
  rec.observe({ type: "system", subtype: "hook_started", hook_event_name: "PreToolUse" });
  rec.observe({ type: "system", subtype: "hook_response", hook_event_name: "PreToolUse" });
  assert.deepEqual(rec.events.map((e) => e.kind), ["hook_started", "hook_response"]);
  assert.equal(rec.events[0].hookEvent, "PreToolUse");
}

// ── baseline: 분류기 거절이 재현되지 않으면 선점 증명 불가로 표시된다.
{
  const quiet = createRecorder(now);
  quiet.mark("can_use_tool", { tool: "Read" });
  const v = verdictFor("baseline-auto", quiet.summary());
  assert.equal(v.canUseTool, 1);
  assert.equal(v.classifierDenies, 0);
  assert.equal(v.preemptionProvable, false);
  assert.match(v.note, /NOT provable/);

  const denied = createRecorder(now);
  denied.observe({ type: "system", subtype: "permission_denied", tool_name: "Bash", decision_reason_type: "classifier" });
  const dv = verdictFor("baseline-auto", denied.summary());
  assert.equal(dv.classifierDenies, 1);
  assert.equal(dv.preemptionProvable, true);
}

// ── ask: 훅이 canUseTool 보다 먼저 왔는지, ask 가 canUseTool 에 닿았는지.
{
  const rec = createRecorder(now);
  rec.mark("hook_invoked", { hookEvent: "PreToolUse", decision: "ask" });
  rec.mark("can_use_tool", { tool: "Read", decision: "allow" });
  const v = verdictFor("hook-ask-allow", rec.summary());
  assert.equal(v.askReachedCanUseTool, true);
  assert.equal(v.hookBeforeCanUseTool, true);

  const never = createRecorder(now);
  never.mark("hook_invoked", { hookEvent: "PreToolUse", decision: "ask" });
  const nv = verdictFor("hook-ask-allow", never.summary());
  assert.equal(nv.askReachedCanUseTool, false);
  assert.equal(nv.hookBeforeCanUseTool, false);
}

// ── deny 대조군: hard-deny 보존은 canUseTool 0회로만 참이 된다.
{
  const rec = createRecorder(now);
  rec.mark("hook_invoked", { hookEvent: "PreToolUse", decision: "deny" });
  assert.equal(verdictFor("hook-deny", rec.summary()).hardDenyPreserved, true);
  rec.mark("can_use_tool", { tool: "Read" });
  assert.equal(verdictFor("hook-deny", rec.summary()).hardDenyPreserved, false);
}

process.stdout.write("recorder-selftest: ok (logic only — this is NOT evidence of SDK ordering)\n");
