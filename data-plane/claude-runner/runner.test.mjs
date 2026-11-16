import { test } from "node:test";
import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  DEFAULT_APPROVAL_WAIT_MS,
  DEFAULT_QUESTION_WAIT_MS,
  PERMISSION_MODE,
  HEADLESS_DENIAL,
  NOTICE_TYPE,
  PLATFORM_DEFAULT_MODEL,
  TurnInput,
  UsageError,
  approvalWaitFromEnv,
  askApproval,
  askPerson,
  canonicalJSON,
  classify,
  decideTool,
  noticeLine,
  parseArgs,
  parseSessionFile,
  planToolDecision,
  pluginToolPrefix,
  pluginsFromSettings,
  queryOptions,
  questionWaitFromEnv,
  resolveClaudeBin,
  resumePlan,
  sameCliVersion,
  sessionFileContent,
  toolArgsHash,
} from "./lib.mjs";

test("parseArgs reads the agent's argv and keeps the prompt whole", () => {
  assert.deepEqual(
    parseArgs([
      "--model",
      "claude-opus-5-5",
      "--effort",
      "max",
      "--k3s-mcp-url",
      "http://127.0.0.1:8093/mcp",
      "--questions-url",
      "http://127.0.0.1:8090/",
      "--approvals-url",
      "http://127.0.0.1:8090/",
      "--resume",
      "--",
      "--help me",
    ]),
    {
      model: "claude-opus-5-5",
      effort: "max",
      k3sMcpUrl: "http://127.0.0.1:8093/mcp",
      questionsUrl: "http://127.0.0.1:8090",
      approvalsUrl: "http://127.0.0.1:8090",
      resume: true,
      prompt: "--help me",
      checkCli: false,
    },
  );
  const bare = parseArgs(["--", "hi"]);
  assert.equal(bare.model, PLATFORM_DEFAULT_MODEL);
  assert.equal(bare.effort, null);
  assert.equal(bare.resume, false);
  assert.equal(bare.k3sMcpUrl, null);
  assert.equal(bare.questionsUrl, null);
  assert.equal(bare.approvalsUrl, null);
  assert.equal(parseArgs(["--check-cli"]).checkCli, true);
});

test("parseArgs refuses what the agent never sends", () => {
  for (const argv of [
    [],
    ["--", "a", "b"],
    ["--model"],
    ["--model", "", "--", "x"],
    ["--effort", "turbo", "--", "x"],
    // The permission mode is the runner's own (always auto), not a flag.
    ["--permission-mode", "auto", "--", "x"],
    ["--permission-mode", "acceptEdits", "--", "x"],
    ["--permission-mode", "bypassPermissions", "--", "x"],
    ["--continue", "--", "x"],
    ["--k3s-mcp-url", "--", "x"],
    ["--k3s-mcp-url", "not a url", "--", "x"],
    ["--k3s-mcp-url", "https://homelab-k3s-mcp.llkm.nl/mcp", "--", "x"],
    ["--k3s-mcp-url", "http://10.0.0.5:8093/mcp", "--", "x"],
    ["--questions-url", "--", "x"],
    ["--questions-url", "http://10.0.0.5:8090", "--", "x"],
    ["--questions-url", "https://127.0.0.1:8090", "--", "x"],
    ["--approvals-url", "--", "x"],
    ["--approvals-url", "http://10.0.0.5:8090", "--", "x"],
    ["--approvals-url", "https://127.0.0.1:8090", "--", "x"],
    ["--approvals-url", "http://user:pw@127.0.0.1:8090", "--", "x"],
    ["hello"],
  ]) {
    assert.throws(() => parseArgs(argv), UsageError, JSON.stringify(argv));
  }
});

test("the session record round-trips and rejects anything else", () => {
  const id = "67d68b60-5b5e-44d4-8c8c-cbad579a6136";
  assert.equal(parseSessionFile(sessionFileContent(id)), id);
  assert.equal(parseSessionFile(""), null);
  assert.equal(parseSessionFile("{"), null);
  assert.equal(parseSessionFile(JSON.stringify({ version: 2, sessionId: id })), null);
  assert.equal(parseSessionFile(JSON.stringify({ version: 1, sessionId: "../../etc" })), null);
});

test("resumePlan follows AC-E4", () => {
  const id = "67d68b60-5b5e-44d4-8c8c-cbad579a6136";
  assert.deepEqual(resumePlan({ resume: false, recordedId: id, transcriptExists: true }), { kind: "new" });
  assert.deepEqual(resumePlan({ resume: true, recordedId: id, transcriptExists: true }), { kind: "resume", sessionId: id });
  // An archive from before the SDK runner has no record: keep --continue's behaviour.
  assert.deepEqual(resumePlan({ resume: true, recordedId: null, transcriptExists: false }), { kind: "continue" });
  assert.deepEqual(resumePlan({ resume: true, recordedId: id, transcriptExists: false }), { kind: "lost", sessionId: id });
});

test("queryOptions spells out what the SDK defaults differently from the CLI", () => {
  const canUseTool = async () => ({ behavior: "allow" });
  const base = queryOptions({
    model: PLATFORM_DEFAULT_MODEL,
    effort: null,
    cwd: "/session/state/workspace",
    claudeBin: "/usr/local/bin/claude",
    plan: { kind: "new" },
    canUseTool,
  });
  assert.deepEqual(base.systemPrompt, { type: "preset", preset: "claude_code" });
  assert.deepEqual(base.settingSources, ["user", "project", "local"]);
  assert.equal(base.permissionMode, "auto");
  assert.equal(PERMISSION_MODE, "auto");
  assert.equal(base.includePartialMessages, true);
  assert.equal(base.canUseTool, canUseTool);
  assert.equal(base.pathToClaudeCodeExecutable, "/usr/local/bin/claude");
  for (const key of ["model", "effort", "resume", "continue", "mcpServers"]) {
    assert.equal(key in base, false, key);
  }

  const resumed = queryOptions({
    model: "claude-opus-5-5",
    effort: "high",
    cwd: "/w",
    claudeBin: "/c",
    plan: { kind: "resume", sessionId: "s" },
  });
  assert.equal(resumed.permissionMode, "auto");
  assert.equal(resumed.model, "claude-opus-5-5");
  assert.equal(resumed.effort, "high");
  assert.equal(resumed.resume, "s");
  assert.equal("continue" in resumed, false);

  const withK3s = queryOptions({ cwd: "/w", claudeBin: "/c", plan: { kind: "new" }, k3sMcpUrl: "http://127.0.0.1:8093/mcp" });
  assert.deepEqual(withK3s.mcpServers, { k3s: { type: "http", url: "http://127.0.0.1:8093/mcp" } });

  const continued = queryOptions({ cwd: "/w", claudeBin: "/c", plan: { kind: "continue" } });
  assert.equal(continued.continue, true);
  assert.equal("resume" in continued, false);
  const lost = queryOptions({ cwd: "/w", claudeBin: "/c", plan: { kind: "lost", sessionId: "s" } });
  assert.equal("resume" in lost || "continue" in lost, false);
});

test("pluginsFromSettings reads the managed plugin selection only", () => {
  const settings = JSON.stringify({
    permissions: { allow: ["Read"] },
    enabledPlugins: {
      "session-platform@dlddu-plugins": true,
      "mcp-servers@dlddu-plugins": true,
      "off@dlddu-plugins": false,
      "other@elsewhere": true,
    },
  });
  assert.deepEqual(pluginsFromSettings(settings), ["session-platform", "mcp-servers"]);
  assert.deepEqual(pluginsFromSettings("not json"), []);
  assert.deepEqual(pluginsFromSettings("{}"), []);
});

test("decideTool allows the platform tools and the session's own plugin tools", () => {
  for (const tool of ["Bash", "Write", "Edit", "NotebookEdit", "WebFetch", "Skill", "Task"]) {
    assert.deepEqual(decideTool(tool, []), { allow: true }, tool);
  }
  const plugins = ["session-platform", "mcp.servers"];
  assert.equal(pluginToolPrefix("mcp.servers"), "mcp__plugin_mcp_servers_");
  assert.equal(decideTool("mcp__plugin_session-platform_k3s__session_list", plugins).allow, true);
  assert.equal(decideTool("mcp__plugin_mcp_servers_db__query", plugins).allow, true);
  // The platform's K3s MCP, when this turn registered it (AC-E6).
  assert.equal(decideTool("mcp__k3s__resource_list", plugins, { k3s: true }).allow, true);
});

test("decideTool denies everything else with a reason the model can act on", () => {
  const plugins = ["session-platform"];
  // A server a cloned repository's .mcp.json brings is not the session's.
  const foreign = decideTool("mcp__repo-server__exfiltrate", plugins);
  assert.equal(foreign.allow, false);
  assert.match(foreign.message, /not part of this session's tool surface/);
  // AC-A5: a denial reports what happened; it does not steer the model.
  for (const message of [foreign.message, HEADLESS_DENIAL]) {
    assert.doesNotMatch(message, /\b(Use|Decide|decide|instead|continue)\b/, message);
  }
  // A plugin the session did not choose is not the session's either.
  assert.equal(decideTool("mcp__plugin_other_x__y", plugins).allow, false);
  assert.equal(decideTool("mcp__plugin_session-platformx_k__y", plugins).allow, false);
  // Without a sidecar token the turn registered no K3s MCP to allow.
  assert.equal(decideTool("mcp__k3s__resource_list", plugins).allow, false);
  assert.equal(decideTool("mcp__k3sx__resource_list", plugins, { k3s: true }).allow, false);
  for (const tool of ["AskUserQuestion", "ExitPlanMode", "EnterPlanMode"]) {
    assert.deepEqual(decideTool(tool, plugins), { allow: false, message: HEADLESS_DENIAL }, tool);
  }
});

test("classify finds the session id and the outcome", () => {
  assert.deepEqual(classify({ type: "system", subtype: "init", session_id: "abc" }), { sessionId: "abc" });
  assert.deepEqual(classify({ type: "system", subtype: "status", session_id: "abc" }), {});
  assert.deepEqual(classify({ type: "result", subtype: "success", is_error: false }), { outcome: "completed", errors: [] });
  assert.deepEqual(classify({ type: "result", subtype: "success", is_error: true }), { outcome: "failed", errors: [] });
  assert.deepEqual(classify({ type: "result", subtype: "error_max_turns", errors: ["too many turns", 3] }), {
    outcome: "failed",
    errors: ["too many turns"],
  });
  assert.deepEqual(classify(null), {});
});

test("noticeLine is one JSON line the agent projects as text", () => {
  const line = noticeLine("denied X: no");
  assert.ok(line.endsWith("\n"));
  assert.deepEqual(JSON.parse(line), { type: NOTICE_TYPE, text: "[session-platform: denied X: no]" });
});

test("sameCliVersion compares against the SDK's CLI release", () => {
  assert.equal(sameCliVersion("2.1.220 (Claude Code)\n", "2.1.220"), true);
  assert.equal(sameCliVersion("2.1.221 (Claude Code)\n", "2.1.220"), false);
  assert.equal(sameCliVersion("", "2.1.220"), false);
});

test("resolveClaudeBin finds the CLI on PATH or takes a path", () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "claude-runner-"));
  try {
    const bin = path.join(dir, "bin");
    mkdirSync(bin);
    const claude = path.join(bin, "claude");
    writeFileSync(claude, "#!/bin/sh\n");
    chmodSync(claude, 0o755);
    assert.equal(resolveClaudeBin({ PATH: `/nonexistent${path.delimiter}${bin}` }), claude);
    assert.equal(resolveClaudeBin({ CLAUDE_CODE_BIN: claude }), claude);
    assert.throws(() => resolveClaudeBin({ PATH: "/nonexistent" }), /no claude executable/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// A clock the test advances by hand, so TurnInput's waits run instantly.
function fakeClock() {
  let now = 0;
  let next = 1;
  const timers = new Map();
  return {
    setTimer(fn, ms) {
      const id = next++;
      timers.set(id, { at: now + ms, fn });
      return id;
    },
    clearTimer(id) {
      timers.delete(id);
    },
    advance(ms) {
      now += ms;
      for (const [id, timer] of [...timers].sort((a, b) => a[1].at - b[1].at)) {
        if (timers.has(id) && timer.at <= now) {
          timers.delete(id);
          timer.fn();
        }
      }
    },
    pending() {
      return timers.size;
    },
  };
}

const result = { type: "result", subtype: "success" };
const tasks = (...ids) => ({ type: "system", subtype: "background_tasks_changed", tasks: ids.map((task_id) => ({ task_id })) });

async function firstAndRest(input) {
  const it = input.messages();
  const first = await it.next();
  return { first: first.value, rest: it.next() };
}

test("a turn without background work closes its input at the result", async () => {
  const clock = fakeClock();
  const input = new TurnInput({ prompt: "hi", ...clock });
  const { first, rest } = await firstAndRest(input);
  assert.deepEqual(first, { type: "user", message: { role: "user", content: "hi" }, parent_tool_use_id: null });
  input.observe({ type: "assistant" });
  input.observe(result);
  assert.equal(input.closed, true);
  assert.equal((await rest).done, true);
  assert.equal(clock.pending(), 0);
});

test("background work keeps the input open until its follow-up result", () => {
  const clock = fakeClock();
  const input = new TurnInput({ prompt: "p", ...clock });
  input.observe(tasks("t1"));
  input.observe(result);
  assert.equal(input.closed, false, "a running task holds the turn open");
  input.observe(tasks());
  input.observe({ type: "system", subtype: "task_updated" });
  input.observe({ type: "system", subtype: "task_notification", status: "completed" });
  clock.advance(60_000);
  assert.equal(input.closed, false, "the notification starts a follow-up, so nothing closes yet");
  input.observe({ type: "system", subtype: "init" });
  input.observe({ type: "assistant" });
  input.observe(result);
  assert.equal(input.closed, false, "a quiet spell follows a turn that ran background work");
  clock.advance(2_000);
  assert.equal(input.closed, true);
});

test("activity during the quiet spell keeps the input open", () => {
  const clock = fakeClock();
  const input = new TurnInput({ prompt: "p", ...clock });
  input.observe(tasks("t1"));
  input.observe(result);
  input.observe(tasks());
  clock.advance(1_000);
  input.observe({ type: "system", subtype: "task_notification", status: "completed" });
  clock.advance(5_000);
  assert.equal(input.closed, false);
});

test("background work with no end keeps the input open, and the platform writes nothing after the prompt", async () => {
  const clock = fakeClock();
  const input = new TurnInput({ prompt: "p", ...clock });
  const { rest } = await firstAndRest(input);
  input.observe(tasks("t1", "t2"));
  input.observe(result);
  clock.advance(24 * 60 * 60 * 1000);
  assert.equal(input.closed, false, "only the agent's turn timeout ends a turn whose work keeps running");
  assert.equal(clock.pending(), 0, "the runner sets no wait of its own");
  input.close();
  assert.deepEqual(await rest, { value: undefined, done: true }, "no message follows the prompt");
});

test("closing early clears every timer and ends the input", async () => {
  const clock = fakeClock();
  const input = new TurnInput({ prompt: "p", ...clock });
  const { rest } = await firstAndRest(input);
  input.observe(tasks("t1"));
  input.observe(result);
  input.observe(tasks());
  assert.equal(clock.pending(), 1, "the quiet spell is the one timer");
  input.close();
  input.close();
  assert.equal(clock.pending(), 0);
  assert.equal((await rest).done, true);
  input.observe(result);
  assert.equal(clock.pending(), 0, "nothing reopens a closed input");
});

// askPerson's agent, in memory: POST /questions/ask opens, GET polls, DELETE
// gives up. answerAfter is how many polls go by before the answer appears.
function fakeQuestionAgent({ answerAfter = 0, answers = null, cancelReason = null, askStatus = 201 } = {}) {
  const calls = [];
  let polls = 0;
  const fetchImpl = async (url, init = {}) => {
    const method = init.method || "GET";
    calls.push({ method, url, body: init.body ? JSON.parse(init.body) : undefined });
    const reply = (status, body) => ({
      status,
      ok: status >= 200 && status < 300,
      json: async () => body,
      text: async () => (typeof body === "string" ? body : JSON.stringify(body)),
    });
    if (method === "POST") {
      return askStatus === 201 ? reply(201, { id: "q_1" }) : reply(askStatus, "no turn is running");
    }
    if (method === "DELETE") {
      return reply(204, "");
    }
    polls++;
    if (cancelReason) {
      return reply(200, { state: "cancelled", reason: cancelReason });
    }
    if (answers && polls > answerAfter) {
      return reply(200, { state: "answered", answers });
    }
    return reply(200, { state: "pending" });
  };
  return { fetchImpl, calls };
}

const askInput = {
  questions: [
    {
      question: "Which database?",
      header: "DB",
      multiSelect: false,
      options: [
        { label: "SQLite", description: "file", preview: "x" },
        { label: "Postgres", description: "server" },
      ],
    },
  ],
};

test("askPerson relays the questions and returns the person's answers as the tool input", async () => {
  const agent = fakeQuestionAgent({ answerAfter: 2, answers: { "Which database?": "Postgres" } });
  const result = await askPerson({
    url: "http://127.0.0.1:8090",
    input: askInput,
    fetchImpl: agent.fetchImpl,
    sleep: async () => {},
  });
  assert.deepEqual(result, { behavior: "allow", updatedInput: { ...askInput, answers: { "Which database?": "Postgres" } } });
  const ask = agent.calls[0];
  assert.equal(ask.url, "http://127.0.0.1:8090/questions/ask");
  // Only the shape the agent takes goes over; previews stay with the model.
  assert.deepEqual(ask.body.questions[0].options[0], { label: "SQLite", description: "file" });
  assert.equal(agent.calls.filter((c) => c.method === "GET").length, 3);
  assert.ok(agent.calls.every((c) => c.method !== "DELETE"));
});

test("askPerson gives up after the wait and tells the model to decide", async () => {
  const agent = fakeQuestionAgent();
  let clock = 0;
  const result = await askPerson({
    url: "http://127.0.0.1:8090",
    input: askInput,
    waitMs: 5000,
    pollMs: 1000,
    fetchImpl: agent.fetchImpl,
    sleep: async (ms) => {
      clock += ms;
    },
    now: () => clock,
  });
  assert.equal(result.behavior, "deny");
  assert.match(result.message, /Nobody answered within 0\.1 minute/);
  assert.doesNotMatch(result.message, /Decide|decide|should|must/, "a denial states what happened, nothing more");
  assert.equal(agent.calls.at(-1).method, "DELETE");
  assert.equal(agent.calls.at(-1).url, "http://127.0.0.1:8090/questions/ask/q_1");
});

test("askPerson reports a question the agent closed or could not take", async () => {
  const closed = await askPerson({
    url: "http://127.0.0.1:8090",
    input: askInput,
    fetchImpl: fakeQuestionAgent({ cancelReason: "the session is being frozen" }).fetchImpl,
    sleep: async () => {},
  });
  assert.equal(closed.behavior, "deny");
  assert.match(closed.message, /the session is being frozen/);

  const refused = await askPerson({
    url: "http://127.0.0.1:8090",
    input: askInput,
    fetchImpl: fakeQuestionAgent({ askStatus: 409 }).fetchImpl,
    sleep: async () => {},
  });
  assert.equal(refused.behavior, "deny");
  assert.match(refused.message, /could not be shown to a person \(409/);

  const unreachable = await askPerson({
    url: "http://127.0.0.1:8090",
    input: askInput,
    fetchImpl: async () => {
      throw new Error("connect ECONNREFUSED");
    },
    sleep: async () => {},
  });
  assert.match(unreachable.message, /ECONNREFUSED/);

  const empty = await askPerson({ url: "http://127.0.0.1:8090", input: {}, fetchImpl: async () => assert.fail("no call") });
  assert.equal(empty.behavior, "deny");
});

test("askPerson stops waiting when the turn is interrupted", async () => {
  const agent = fakeQuestionAgent();
  const controller = new AbortController();
  const result = await askPerson({
    url: "http://127.0.0.1:8090",
    input: askInput,
    signal: controller.signal,
    fetchImpl: agent.fetchImpl,
    sleep: async () => controller.abort(),
  });
  assert.equal(result.behavior, "deny");
  assert.match(result.message, /interrupted/);
  assert.equal(agent.calls.at(-1).method, "DELETE");
});

test("the question wait comes from the environment only as a positive integer", () => {
  assert.equal(questionWaitFromEnv({}), DEFAULT_QUESTION_WAIT_MS);
  assert.equal(questionWaitFromEnv({ CLAUDE_RUNNER_QUESTION_WAIT_MS: "60000" }), 60000);
  for (const bad of ["0", "-5", "1.5", "abc", ""]) {
    assert.equal(questionWaitFromEnv({ CLAUDE_RUNNER_QUESTION_WAIT_MS: bad }), DEFAULT_QUESTION_WAIT_MS, bad);
  }
});

test("planToolDecision puts the owner after the policy, never over it", () => {
  const plugins = ["session-platform"];
  // A policy denial stays a denial even in a turn that can ask the owner.
  for (const tool of ["mcp__repo__anything", "EnterPlanMode", "ExitPlanMode"]) {
    const plan = planToolDecision(tool, plugins, { approvals: true, questions: true });
    assert.equal(plan.kind, "deny", tool);
    assert.ok(plan.message);
  }
  // What the policy allows is what the owner decides.
  assert.equal(planToolDecision("Bash", plugins, { approvals: true }).kind, "approval");
  assert.equal(planToolDecision("mcp__k3s__resource_get", plugins, { k3s: true, approvals: true }).kind, "approval");
  assert.equal(planToolDecision(`${pluginToolPrefix("session-platform")}mcp__x`, plugins, { approvals: true }).kind, "approval");
  // A turn that did not ask for approvals behaves exactly as before.
  assert.equal(planToolDecision("Bash", plugins, {}).kind, "allow");
  assert.equal(planToolDecision("mcp__k3s__resource_get", plugins, { k3s: true }).kind, "allow");
  assert.equal(planToolDecision("Bash", plugins, { questions: true }).kind, "allow");
  // A question is a question in either turn, and a denial without a desk.
  assert.equal(planToolDecision("AskUserQuestion", plugins, { questions: true, approvals: true }).kind, "question");
  assert.equal(planToolDecision("AskUserQuestion", plugins, { approvals: true }).kind, "deny");
});

test("the approval wait comes from the environment only as a positive integer", () => {
  assert.equal(approvalWaitFromEnv({}), DEFAULT_APPROVAL_WAIT_MS);
  assert.equal(approvalWaitFromEnv({ CLAUDE_RUNNER_APPROVAL_WAIT_MS: "60000" }), 60000);
  for (const bad of ["0", "-5", "1.5", "abc", ""]) {
    assert.equal(approvalWaitFromEnv({ CLAUDE_RUNNER_APPROVAL_WAIT_MS: bad }), DEFAULT_APPROVAL_WAIT_MS, bad);
  }
});

test("toolArgsHash names one exact call and ignores only key order", () => {
  const a = toolArgsHash("Bash", { command: "ls /tmp", timeout: 1000 });
  assert.equal(a, toolArgsHash("Bash", { timeout: 1000, command: "ls /tmp" }));
  assert.match(a, /^sha256:[0-9a-f]{64}$/);
  // Anything else about the call is another call.
  assert.notEqual(a, toolArgsHash("Bash", { command: "ls /tmp", timeout: 1001 }));
  assert.notEqual(a, toolArgsHash("Bash", { command: "ls /tmp " }));
  assert.notEqual(a, toolArgsHash("Write", { command: "ls /tmp", timeout: 1000 }));
  assert.notEqual(toolArgsHash("Bash", { a: [1, 2] }), toolArgsHash("Bash", { a: [2, 1] }));
  // Nested keys are ordered too, and a missing input is not an error.
  assert.equal(toolArgsHash("X", { a: { b: 1, c: 2 } }), toolArgsHash("X", { a: { c: 2, b: 1 } }));
  assert.match(toolArgsHash("X", undefined), /^sha256:/);
  assert.equal(canonicalJSON({ b: 1, a: [true, null, "s"] }), '{"a":[true,null,"s"],"b":1}');
});

// askApproval's agent, in memory: POST /approvals/ask opens (or joins, for a
// redelivered requestId), GET polls, DELETE withdraws. decideAfter is how many
// polls go by before the decision appears.
function fakeApprovalAgent({
  decideAfter = 0,
  decision = null,
  reason = null,
  argsHash = null,
  tool = null,
  askStatus = 201,
  pollStatus = 200,
} = {}) {
  const calls = [];
  let polls = 0;
  let asked = null;
  const fetchImpl = async (url, init = {}) => {
    const method = init.method || "GET";
    const body = init.body ? JSON.parse(init.body) : undefined;
    calls.push({ method, url, body });
    const reply = (status, payload) => ({
      status,
      ok: status >= 200 && status < 300,
      json: async () => payload,
      text: async () => (typeof payload === "string" ? payload : JSON.stringify(payload)),
    });
    if (method === "POST") {
      if (askStatus !== 201) {
        return reply(askStatus, "the arguments of this call changed");
      }
      // The same control request joins the request already waiting.
      const rejoin = asked !== null && asked === body.requestId;
      asked = body.requestId;
      return reply(rejoin ? 200 : 201, { id: "ap_1", expiresAt: "2026-11-15T13:00:00Z" });
    }
    if (method === "DELETE") {
      return reply(204, "");
    }
    polls++;
    if (pollStatus !== 200) {
      return reply(pollStatus, "no such approval request");
    }
    if (decision && polls > decideAfter) {
      return reply(200, {
        state: decision,
        reason,
        tool: tool ?? calls[0].body.tool,
        argsHash: argsHash ?? calls[0].body.argsHash,
      });
    }
    return reply(200, { state: "pending" });
  };
  return { fetchImpl, calls };
}

const approvalCall = {
  url: "http://127.0.0.1:8090",
  toolName: "Bash",
  input: { command: "rm -rf /tmp/scratch" },
  toolUseId: "toolu_1",
  requestId: "req_1",
};

test("askApproval allows a call the owner approved, with the arguments untouched", async () => {
  const agent = fakeApprovalAgent({ decideAfter: 1, decision: "approved" });
  const result = await askApproval({ ...approvalCall, fetchImpl: agent.fetchImpl, sleep: async () => {} });
  assert.deepEqual(result, { behavior: "allow", updatedInput: approvalCall.input });
  const ask = agent.calls[0];
  assert.equal(ask.url, "http://127.0.0.1:8090/approvals/ask");
  assert.equal(ask.body.tool, "Bash");
  assert.equal(ask.body.toolUseId, "toolu_1");
  assert.equal(ask.body.requestId, "req_1");
  assert.equal(ask.body.argsHash, toolArgsHash("Bash", approvalCall.input));
  assert.deepEqual(ask.body.input, approvalCall.input);
  assert.ok(agent.calls.every((c) => c.method !== "DELETE"));
});

test("askApproval denies a call the owner refused, and says the reason", async () => {
  const agent = fakeApprovalAgent({ decision: "denied", reason: "not on production" });
  const result = await askApproval({ ...approvalCall, fetchImpl: agent.fetchImpl, sleep: async () => {} });
  assert.equal(result.behavior, "deny");
  assert.match(result.message, /owner denied this call \(not on production\)/);
  assert.doesNotMatch(result.message, /should|must|instead/, "a denial states what happened, nothing more");
});

test("askApproval denies a decision taken on other arguments", async () => {
  const agent = fakeApprovalAgent({ decision: "approved", argsHash: toolArgsHash("Bash", { command: "ls" }) });
  const result = await askApproval({ ...approvalCall, fetchImpl: agent.fetchImpl, sleep: async () => {} });
  assert.equal(result.behavior, "deny");
  assert.match(result.message, /for another call/);
  assert.equal(agent.calls.at(-1).method, "DELETE", "an unusable request is withdrawn, not left to be approved later");

  const otherTool = fakeApprovalAgent({ decision: "approved", tool: "Write" });
  const mixed = await askApproval({ ...approvalCall, fetchImpl: otherTool.fetchImpl, sleep: async () => {} });
  assert.equal(mixed.behavior, "deny");
});

test("askApproval gives up after the wait and withdraws the request", async () => {
  const agent = fakeApprovalAgent();
  let clock = 0;
  const result = await askApproval({
    ...approvalCall,
    waitMs: 5000,
    pollMs: 1000,
    fetchImpl: agent.fetchImpl,
    sleep: async (ms) => {
      clock += ms;
    },
    now: () => clock,
  });
  assert.equal(result.behavior, "deny");
  assert.match(result.message, /Nobody decided within 0\.1 minute/);
  assert.equal(agent.calls.at(-1).method, "DELETE");
  assert.equal(agent.calls.at(-1).url, "http://127.0.0.1:8090/approvals/ask/ap_1");
});

test("askApproval denies when the request is closed, gone or unreachable", async () => {
  const closed = await askApproval({
    ...approvalCall,
    fetchImpl: fakeApprovalAgent({ decision: "cancelled", reason: "the session is being frozen" }).fetchImpl,
    sleep: async () => {},
  });
  assert.equal(closed.behavior, "deny");
  assert.match(closed.message, /the session is being frozen/);

  const gone = await askApproval({
    ...approvalCall,
    fetchImpl: fakeApprovalAgent({ pollStatus: 404 }).fetchImpl,
    sleep: async () => {},
  });
  assert.equal(gone.behavior, "deny");
  assert.match(gone.message, /no longer open/);

  const refused = await askApproval({
    ...approvalCall,
    fetchImpl: fakeApprovalAgent({ askStatus: 409 }).fetchImpl,
    sleep: async () => {},
  });
  assert.equal(refused.behavior, "deny");
  assert.match(refused.message, /could not be put to the session owner \(409/);

  const unreachable = await askApproval({
    ...approvalCall,
    fetchImpl: async () => {
      throw new Error("connect ECONNREFUSED");
    },
    sleep: async () => {},
  });
  assert.equal(unreachable.behavior, "deny");
  assert.match(unreachable.message, /ECONNREFUSED/);
});

test("askApproval keeps waiting through a missed poll, and never allows without a decision", async () => {
  let polls = 0;
  const calls = [];
  const fetchImpl = async (url, init = {}) => {
    const method = init.method || "GET";
    calls.push(method);
    if (method === "POST") {
      return { status: 201, ok: true, json: async () => ({ id: "ap_1" }), text: async () => "" };
    }
    if (method === "DELETE") {
      return { status: 204, ok: true, json: async () => ({}), text: async () => "" };
    }
    polls++;
    if (polls === 1) {
      throw new Error("read ECONNRESET");
    }
    return { status: 200, ok: true, json: async () => ({ state: "pending" }), text: async () => "" };
  };
  let clock = 0;
  const result = await askApproval({
    ...approvalCall,
    waitMs: 3000,
    pollMs: 1000,
    fetchImpl,
    sleep: async (ms) => {
      clock += ms;
    },
    now: () => clock,
  });
  assert.equal(result.behavior, "deny");
  assert.ok(polls > 1, "a missed poll is retried");
  assert.equal(calls.at(-1), "DELETE");
});

test("askApproval stops waiting when the turn is interrupted", async () => {
  const agent = fakeApprovalAgent();
  const controller = new AbortController();
  const result = await askApproval({
    ...approvalCall,
    signal: controller.signal,
    fetchImpl: agent.fetchImpl,
    sleep: async () => controller.abort(),
  });
  assert.equal(result.behavior, "deny");
  assert.match(result.message, /interrupted/);
  assert.equal(agent.calls.at(-1).method, "DELETE");
});

test("a redelivered permission request joins the one approval already waiting", async () => {
  const agent = fakeApprovalAgent({ decision: "approved" });
  const first = await askApproval({ ...approvalCall, fetchImpl: agent.fetchImpl, sleep: async () => {} });
  const again = await askApproval({ ...approvalCall, fetchImpl: agent.fetchImpl, sleep: async () => {} });
  assert.equal(first.behavior, "allow");
  assert.equal(again.behavior, "allow");
  const asks = agent.calls.filter((c) => c.method === "POST");
  assert.equal(asks.length, 2);
  assert.ok(asks.every((c) => c.body.requestId === "req_1"));
  // Both asks are answered with the same request id, so one decision serves
  // the redelivery too — the agent is what refuses to open a second request.
  assert.ok(agent.calls.filter((c) => c.method === "GET").every((c) => c.url.endsWith("/approvals/ask/ap_1")));
});
