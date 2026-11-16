#!/usr/bin/env node
// pretooluse-probe — AC-E2 「턴 중 도구 승인」의 미증명 전제 하나를 실측하는 격리 하네스다.
//
// 묻는 것: Claude Agent SDK 의 PreToolUse 훅이 `permissionDecision: "ask"` 를 돌려줄 때
// 그 호출이 실제로 `canUseTool` 로 오는가, auto 모드 분류기와의 순서는 어떤가, 그리고
// PreToolUse 의 `deny` 가 정말 `canUseTool` 을 건너뛰는가(= 명시적 deny 보존).
// 계약 문서(sdk.d.ts 의 SDKPermissionDeniedMessage 주석)는 그렇게 적고 있으나 순서 판정은
// CLI 안에 있어 정적 분석으로 증명되지 않는다 — 그래서 실행해 본다.
//
// 이 파일은 제품 코드가 아니다. 러너(`data-plane/claude-runner`)는 한 줄도 쓰지 않고,
// 제품의 권한 모드도 바꾸지 않는다. 여기서 내리는 「승인」은 격리된 가짜 작업에 대한
// 하네스 자신의 결정이며, 사람의 승인을 대신하거나 위조하지 않는다.
//
// 기록하는 것: 이벤트 종류·상대 시각(ms)·횟수뿐이다. 모델이 쓴 문장, 도구 인자, 환경
// 변수, 자격 증명은 **기록하지 않는다**.
//
// 실행: README.md 참조. 인증이 없으면 아무것도 실행하지 않고 코드 3 으로 끝난다.

import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);

// ──────────────────────────────────────────────────────────── 설정(모두 재정의 가능)
const CASE_TIMEOUT_MS = Number(process.env.PROBE_CASE_TIMEOUT_MS || 120_000);
const CLAUDE_BIN = process.env.PROBE_CLAUDE_BIN || "/usr/local/bin/claude";
const SDK_CANDIDATES = [
  process.env.PROBE_SDK_PATH,
  "/opt/claude-runner/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs",
  "@anthropic-ai/claude-agent-sdk",
].filter(Boolean);

// 무해한 하나의 도구 호출만 유도한다. 격리 cwd 안에서 파일 하나를 읽는 일이고,
// 네트워크도 운영 자원도 건드리지 않는다.
const PROBE_FILE = "probe-input.txt";
const PROBE_PROMPT =
  `Read the file ${PROBE_FILE} in the current directory with the Read tool, then reply with its first word. Use exactly one tool call.`;

// ──────────────────────────────────────────────────────────── 인증 전제
// 정식 로그인/지원되는 환경 인증만 쓴다. 토큰을 만들지도, 다른 프로세스에서 꺼내지도 않는다.
function authPresent(env) {
  if (env.CLAUDE_CODE_OAUTH_TOKEN) return "CLAUDE_CODE_OAUTH_TOKEN";
  if (env.ANTHROPIC_API_KEY) return "ANTHROPIC_API_KEY";
  if (env.ANTHROPIC_BASE_URL && env.ANTHROPIC_AUTH_TOKEN) return "ANTHROPIC_BASE_URL+ANTHROPIC_AUTH_TOKEN";
  if (env.PROBE_ASSUME_CLI_LOGIN === "true") return "cli-login(declared)";
  return null;
}

// ──────────────────────────────────────────────────────────── 기록기
// 민감 정보가 들어올 자리가 없도록, 화이트리스트한 칸만 남긴다.
export function createRecorder(now = () => Date.now()) {
  const started = now();
  const events = [];
  const counts = Object.create(null);
  const bump = (key) => {
    counts[key] = (counts[key] || 0) + 1;
  };
  return {
    events,
    counts,
    mark(kind, fields = {}) {
      const safe = {};
      for (const key of ["subtype", "tool", "decisionReasonType", "hookEvent", "decision", "behavior", "isError"]) {
        if (fields[key] !== undefined) safe[key] = fields[key];
      }
      events.push({ atMs: now() - started, kind, ...safe });
      bump(kind);
      return events[events.length - 1];
    },
    // observe 는 SDK 메시지 하나에서 기록할 만한 것만 뽑는다. 본문(text)·도구 인자는 읽지 않는다.
    observe(message) {
      const type = message?.type;
      if (type === "system") {
        const subtype = message.subtype;
        if (subtype === "permission_denied") {
          return this.mark("permission_denied", {
            tool: message.tool_name,
            decisionReasonType: message.decision_reason_type,
          });
        }
        if (subtype === "hook_started" || subtype === "hook_progress" || subtype === "hook_response") {
          return this.mark(subtype, { hookEvent: message.hook_event_name ?? message.hookEventName });
        }
        return this.mark("system", { subtype });
      }
      if (type === "assistant") {
        const blocks = Array.isArray(message.message?.content) ? message.message.content : [];
        const toolUse = blocks.find((b) => b?.type === "tool_use");
        return this.mark("assistant", toolUse ? { tool: toolUse.name } : {});
      }
      if (type === "user") {
        const blocks = Array.isArray(message.message?.content) ? message.message.content : [];
        const result = blocks.find((b) => b?.type === "tool_result");
        if (result) return this.mark("tool_result", { isError: Boolean(result.is_error) });
        return this.mark("user", {});
      }
      if (type === "result") {
        return this.mark("result", { subtype: message.subtype, isError: Boolean(message.is_error) });
      }
      return this.mark(type ? String(type) : "unknown", {});
    },
    summary() {
      return { events, counts };
    },
  };
}

// 판정 규칙도 데이터로 둔다 — 실행 없이도 검증할 수 있게.
export function verdictFor(caseName, summary) {
  const c = summary.counts;
  const canUseTool = c.can_use_tool || 0;
  const hookCalls = c.hook_invoked || 0;
  const classifierDenies = summary.events.filter(
    (e) => e.kind === "permission_denied" && e.decisionReasonType === "classifier",
  ).length;
  const firstIndex = (kind) => summary.events.findIndex((e) => e.kind === kind);
  switch (caseName) {
    case "baseline-auto":
      return {
        canUseTool,
        classifierDenies,
        // 무해한 호출이 분류기 거절을 재현하지 못하면 선점(preemption) 증명은 불가능하다.
        preemptionProvable: classifierDenies > 0,
        note:
          classifierDenies > 0
            ? "baseline reproduced a classifier auto-deny"
            : "BASELINE DID NOT REPRODUCE A CLASSIFIER DENY — preemption over the classifier is NOT provable from this run",
      };
    case "hook-ask-allow":
    case "hook-ask-deny":
      return {
        hookCalls,
        canUseTool,
        hookBeforeCanUseTool: hookCalls > 0 && canUseTool > 0 && firstIndex("hook_invoked") < firstIndex("can_use_tool"),
        askReachedCanUseTool: hookCalls > 0 && canUseTool > 0,
      };
    case "hook-deny":
      return {
        hookCalls,
        canUseTool,
        // 계약(sdk.d.ts): PreToolUse 의 deny 는 canUseTool 을 건너뛴다.
        hardDenyPreserved: hookCalls > 0 && canUseTool === 0,
      };
    default:
      return { canUseTool, hookCalls };
  }
}

// ──────────────────────────────────────────────────────────── 한 케이스
async function runCase({ query, caseName, hookDecision, ownerDecision, cwd }) {
  const rec = createRecorder();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), CASE_TIMEOUT_MS);

  let decided = 0;
  const canUseTool = async (toolName, input) => {
    rec.mark("can_use_tool", { tool: toolName, decision: ownerDecision });
    // 결정은 한 번이다 — 두 번째 호출이 오면 그 사실을 기록하고 거절한다(중복 관측).
    if (++decided > 1) {
      rec.mark("extra_decision_refused", { tool: toolName });
      return { behavior: "deny", message: "probe harness decides once per case" };
    }
    // 이 실험이 승인하는 것은 격리 cwd 안의 그 파일 하나를 읽는 호출뿐이다.
    const target = input && typeof input === "object" ? input.file_path : undefined;
    if (toolName !== "Read" || typeof target !== "string" || path.basename(target) !== PROBE_FILE) {
      rec.mark("off_target_refused", { tool: toolName });
      return { behavior: "deny", message: "probe harness only decides the isolated probe file" };
    }
    if (ownerDecision === "deny") {
      // 격리된 가짜 작업에 대한 하네스 자신의 거절이다.
      return { behavior: "deny", message: "probe harness denied this isolated call" };
    }
    // 인자는 보존한다 — 승인은 모델이 만든 그 호출을 그대로 돌린다.
    return { behavior: "allow", updatedInput: input };
  };

  const options = {
    cwd,
    pathToClaudeCodeExecutable: CLAUDE_BIN,
    systemPrompt: { type: "preset", preset: "claude_code" },
    // 제품과 같은 모드를 쓰되 제품 설정은 읽지 않는다(격리 cwd + 설정 원천 없음).
    permissionMode: "auto",
    settingSources: [],
    // maxTurns 1 은 도구 호출 하나를 끝내지 못하고 error_max_turns 로 끝난다(2026-11-16 실측,
    // result-maxturns1.json). 3 이면 네 케이스 모두 result success 로 닫힌다.
    maxTurns: 3,
    // 이 실험이 부르는 도구는 Read 하나뿐이다. 다른 도구가 끼어들면 순서 기록이 섞인다.
    tools: ["Read"],
    includeHookEvents: true,
    canUseTool,
    abortController: controller,
    // 외부 MCP 서버는 붙이지 않는다.
    mcpServers: {},
  };
  if (hookDecision) {
    options.hooks = {
      PreToolUse: [
        {
          hooks: [
            async () => {
              rec.mark("hook_invoked", { hookEvent: "PreToolUse", decision: hookDecision });
              return {
                continue: true,
                hookSpecificOutput: {
                  hookEventName: "PreToolUse",
                  permissionDecision: hookDecision,
                  permissionDecisionReason: "pretooluse-probe",
                },
              };
            },
          ],
        },
      ],
    };
  }

  let failure = null;
  try {
    for await (const message of query({ prompt: PROBE_PROMPT, options })) {
      rec.observe(message);
    }
  } catch (err) {
    failure = err?.name === "AbortError" ? "timeout" : String(err?.message || err).slice(0, 200);
    rec.mark("aborted", {});
  } finally {
    clearTimeout(timer);
  }

  const summary = rec.summary();
  return { case: caseName, hookDecision: hookDecision || null, ownerDecision, failure, ...summary, verdict: verdictFor(caseName, summary) };
}

// ──────────────────────────────────────────────────────────── 본체
async function main() {
  const auth = authPresent(process.env);
  if (!auth) {
    process.stderr.write(
      "pretooluse-probe: no supported authentication in this environment.\n" +
        "  Run `claude setup-token` (subscription) or export ANTHROPIC_API_KEY, then retry.\n" +
        "  This probe never reads another process's environment and never writes a token file.\n",
    );
    process.exit(3);
  }

  let query;
  let sdkFrom = null;
  for (const candidate of SDK_CANDIDATES) {
    try {
      ({ query } = await import(candidate));
      sdkFrom = candidate;
      break;
    } catch {
      // 다음 후보
    }
  }
  if (!query) {
    process.stderr.write("pretooluse-probe: could not load @anthropic-ai/claude-agent-sdk (set PROBE_SDK_PATH)\n");
    process.exit(4);
  }

  const versions = {
    sdk: safeVersion("@anthropic-ai/claude-agent-sdk", sdkFrom),
    cliBin: CLAUDE_BIN,
    node: process.version,
    authSource: auth,
  };

  const root = mkdtempSync(path.join(os.tmpdir(), "pretooluse-probe-"));
  const cwd = path.join(root, "work");
  mkdirSync(cwd);
  writeFileSync(path.join(cwd, PROBE_FILE), "probe-ok nothing-here\n");

  // 한 번에 하나씩, 순차로만 돈다.
  const cases = [
    { caseName: "baseline-auto", hookDecision: null, ownerDecision: "allow" },
    { caseName: "hook-ask-allow", hookDecision: "ask", ownerDecision: "allow" },
    { caseName: "hook-ask-deny", hookDecision: "ask", ownerDecision: "deny" },
    { caseName: "hook-deny", hookDecision: "deny", ownerDecision: "allow" },
  ];
  const results = [];
  for (const spec of cases) {
    results.push(await runCase({ query, cwd, ...spec }));
  }

  rmSync(root, { recursive: true, force: true });
  process.stdout.write(JSON.stringify({ versions, results }, null, 2) + "\n");
}

function safeVersion(name, from) {
  try {
    if (from && from.endsWith(".mjs")) {
      return require(path.join(path.dirname(from), "package.json")).version;
    }
    return require(`${name}/package.json`).version;
  } catch {
    return "unknown";
  }
}

// 직접 실행할 때만 돈다 — 기록기·판정 규칙은 실행 없이 import 해 검증할 수 있다.
if (process.argv[1] && import.meta.url === `file://${process.argv[1]}`) {
  main().catch((err) => {
    process.stderr.write(`pretooluse-probe: ${String(err?.message || err).slice(0, 300)}\n`);
    process.exit(1);
  });
}
