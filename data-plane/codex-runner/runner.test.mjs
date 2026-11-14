import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  AgentTracker,
  DEFAULT_BACKGROUND_WAIT_MS,
  EXIT_KILLED,
  EXIT_LOGIN_FAILED,
  EXIT_OK,
  EXIT_TURN_FAILED,
  NOTICE_TYPE,
  PLATFORM_DEFAULT_MODEL,
  TurnTranslator,
  UsageError,
  appServerArgs,
  backgroundWaitFromEnv,
  cleanLoginLine,
  execItem,
  followUpPrompt,
  forbiddenAction,
  minimalEnv,
  parseArgs,
  parseThreadFile,
  serverRequestResponse,
  threadFileContent,
  threadParams,
} from "./lib.mjs";
import { inspectPromptInput } from "./surface.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));

test("parseArgs reads the agent's argv and keeps the prompt whole", () => {
  assert.deepEqual(parseArgs(["--model", "gpt-6", "--resume", "--", "--help me"]), {
    model: "gpt-6",
    mode: "chat",
    effort: null,
    resume: true,
    prompt: "--help me",
    printCodexBin: false,
  });
  assert.equal(parseArgs(["--", "hi"]).model, PLATFORM_DEFAULT_MODEL);
  assert.equal(parseArgs(["--model", "platform-default", "--", "hi"]).resume, false);
  const task = parseArgs(["--mode", "task", "--model", "gpt-6", "--effort", "xhigh", "--", "go"]);
  assert.equal(task.mode, "task");
  assert.equal(task.effort, "xhigh");
  for (const bad of [
    [], ["hi"], ["--model"], ["--model", "", "--", "x"], ["--", "a", "b"], ["--bogus", "--", "x"],
    ["--mode", "shell", "--", "x"], ["--mode", "--", "x"], ["--effort", "max", "--", "x"], ["--effort", "", "--", "x"],
  ]) {
    assert.throws(() => parseArgs(bad), UsageError, JSON.stringify(bad));
  }
});

test("minimalEnv passes only what codex needs", () => {
  const env = minimalEnv(
    { HOME: "/h", PATH: "/bin", LANG: "C.UTF-8", TZ: "UTC", OPENAI_API_KEY: "sk", K3S_MCP_TOKEN: "t", SSL_CERT_FILE: "" },
    "/h/.codex",
  );
  assert.deepEqual(env, { HOME: "/h", PATH: "/bin", LANG: "C.UTF-8", TZ: "UTC", CODEX_HOME: "/h/.codex" });
  assert.equal(minimalEnv({ HOME: "/h", SSL_CERT_FILE: "/ca.pem" }, "/c").SSL_CERT_FILE, "/ca.pem");
});

test("threadParams pins the read-only, never-ask surface", () => {
  assert.deepEqual(threadParams(PLATFORM_DEFAULT_MODEL, "/w"), {
    cwd: "/w",
    sandbox: "read-only",
    approvalPolicy: "never",
  });
  assert.equal(threadParams("gpt-6", "/w").model, "gpt-6");
  assert.equal(threadParams("gpt-6", "/w", { mode: "task" }).sandbox, "danger-full-access");
});

test("appServerArgs keeps web search off and opens the network only for codex-task", () => {
  assert.deepEqual(appServerArgs(), [
    "app-server",
    "--config", 'web_search="disabled"',
    "--config", 'approval_policy="never"',
    "--config", "sandbox_workspace_write.network_access=false",
  ]);
  const task = appServerArgs({ mode: "task", effort: "high" });
  assert.ok(task.includes("sandbox_workspace_write.network_access=true"));
  assert.ok(task.includes('model_reasoning_effort="high"'));
});

test("execItem translates app-server items into the events codex exec printed", () => {
  assert.deepEqual(execItem({ id: "a", type: "agentMessage", text: "hi", phase: null }), { id: "a", type: "agent_message", text: "hi" });
  assert.equal(execItem({ id: "r", type: "reasoning", summary: [" "], content: [] }), null);
  assert.deepEqual(execItem({ id: "r", type: "reasoning", summary: ["x", "y"], content: [] }), { id: "r", type: "reasoning", text: "x\ny" });
  assert.deepEqual(
    execItem({ id: "m", type: "mcpToolCall", server: "k3s", tool: "ping", status: "failed", arguments: {}, result: null, error: { message: "no" } }),
    { id: "m", type: "mcp_tool_call", server: "k3s", tool: "ping", status: "failed", arguments: {}, result: null, error: { message: "no" } },
  );
  assert.equal(execItem({ id: "c", type: "commandExecution", command: "ls", status: "inProgress" }).status, "in_progress");
  assert.equal(execItem({ id: "f", type: "fileChange", changes: [{ path: "a", kind: { type: "update" } }], status: "declined" }).status, "failed");
  assert.equal(execItem({ id: "w", type: "webSearch", query: "q" }).type, "web_search");
  const spawn = execItem({
    id: "s", type: "collabAgentToolCall", tool: "spawnAgent", status: "completed", senderThreadId: "t",
    receiverThreadIds: ["c"], prompt: "go", agentsStates: { c: { status: "running", message: null } },
  });
  assert.equal(spawn.tool, "spawn_agent");
  assert.deepEqual(spawn.agents_states, { c: { status: "running", message: null } });
  assert.equal(execItem({ id: "s", type: "collabAgentToolCall", tool: "listAgents", status: "completed" }), null);
  for (const other of [{ id: "u", type: "userMessage" }, { id: "p", type: "plan" }, null, "x"]) {
    assert.equal(execItem(other), null);
  }
});

test("forbiddenAction is the chat kill switch for every thread's items", () => {
  assert.equal(forbiddenAction({ type: "commandExecution", command: "ls" }).forbidden, "command_execution");
  assert.equal(forbiddenAction({ type: "fileChange", changes: [] }).forbidden, "file_change");
  assert.equal(forbiddenAction({ type: "webSearch" }).forbidden, "web_search");
  assert.equal(forbiddenAction({ type: "commandExecution" }, "task"), null);
  assert.equal(forbiddenAction({ type: "mcpToolCall", server: "k3s", tool: "x" }), null);
});

test("TurnTranslator emits exec events for its own turn and closes open items", () => {
  const t = new TurnTranslator("main");
  const late = { threadId: "main", turnId: "t0", item: { id: "x", type: "subAgentActivity", kind: "completed", agentThreadId: "c" } };
  assert.deepEqual(t.translate("item/completed", late).events, []);
  assert.equal(t.turnId, null, "an item of the previous turn does not fix the translator's turn");
  assert.deepEqual(t.translate("turn/started", { threadId: "main", turn: { id: "t1" } }).events, [{ type: "turn.started" }]);
  assert.equal(t.turnId, "t1", "the turn's own turn/started fixes it");
  assert.deepEqual(t.translate("item/started", { threadId: "other", turnId: "x", item: { id: "a", type: "agentMessage", text: "no" } }).events, []);
  assert.deepEqual(t.translate("item/started", { threadId: "main", turnId: "t1", item: { id: "a", type: "agentMessage", text: "" } }).events, []);
  const call = { id: "m", type: "mcpToolCall", server: "k3s", tool: "ping", status: "inProgress" };
  assert.equal(t.translate("item/started", { threadId: "main", turnId: "t1", item: call }).events[0].type, "item.started");
  assert.deepEqual(t.translate("error", { threadId: "main", turnId: "t1", willRetry: true, error: { message: "retrying" } }).events, [{ type: "error", message: "retrying" }]);
  assert.equal(t.errorSeen, false);
  const done = t.translate("turn/completed", { threadId: "main", turn: { id: "t1", status: "completed", items: [{ ...call, status: "completed" }] } });
  assert.deepEqual(done.events.map((e) => e.type), ["item.completed", "turn.completed"]);
  assert.equal(done.outcome, "completed");

  const failed = new TurnTranslator("main", "t2");
  failed.translate("error", { threadId: "main", turnId: "t2", willRetry: false, error: { message: "quota", additionalDetails: "plus" } });
  const end = failed.translate("turn/completed", { threadId: "main", turn: { id: "t2", status: "failed", error: { message: "quota" }, items: [] } });
  assert.equal(end.outcome, "failed");
  assert.deepEqual(end.events, [{ type: "turn.failed", error: { message: "quota" } }]);
  const interrupted = new TurnTranslator("main", "t3").translate("turn/completed", { threadId: "main", turn: { id: "t3", status: "interrupted", error: null, items: [] } });
  assert.equal(interrupted.outcome, "failed");
  assert.match(interrupted.events[0].error.message, /interrupted/);
  const errored = new TurnTranslator("main", "t4");
  errored.translate("error", { threadId: "main", turnId: "t4", willRetry: false, error: { message: "x" } });
  assert.equal(errored.translate("turn/completed", { threadId: "main", turn: { id: "t4", status: "completed", items: [] } }).outcome, "failed");
});

test("AgentTracker learns agents from spawns, activity items and unknown threads", () => {
  const agents = new AgentTracker("main");
  assert.deepEqual(agents.observe("thread/started", { thread: { id: "main" } }), []);
  const spawn = { id: "s", type: "collabAgentToolCall", tool: "spawnAgent", status: "completed", senderThreadId: "main", receiverThreadIds: ["agent-0000aaaa"] };
  assert.deepEqual(agents.observe("item/completed", { threadId: "main", turnId: "t1", item: spawn }), ["Background agent 0000aaaa started."]);
  assert.deepEqual(agents.observe("item/completed", { threadId: "main", turnId: "t1", item: spawn }), [], "a second sighting is not a new agent");
  assert.deepEqual(
    agents.observe("item/completed", { threadId: "main", turnId: "t1", item: { id: "x", type: "subAgentActivity", kind: "started", agentThreadId: "agent-0000aaaa", agentPath: "/root/ada" } }),
    [],
  );
  assert.deepEqual(agents.observe("item/completed", { threadId: "main", item: { ...spawn, status: "failed", receiverThreadIds: ["never"] } }), []);
  assert.deepEqual(agents.busy().map((a) => [a.id, a.name]), [["agent-0000aaaa", "ada"]], "busy from its spawn, before its first turn");

  agents.observe("turn/started", { threadId: "agent-0000aaaa", turn: { id: "ta" } });
  assert.deepEqual(agents.observe("item/started", { threadId: "agent-0000aaaa", turnId: "ta", item: { type: "mcpToolCall", server: "k3s", tool: "ping" } }), ["· ada: k3s.ping"]);
  agents.observe("item/completed", { threadId: "agent-0000aaaa", turnId: "ta", item: { type: "agentMessage", text: "found it" } });
  assert.deepEqual(agents.observe("turn/completed", { threadId: "agent-0000aaaa", turn: { id: "ta", status: "completed" } }), ["Background agent ada finished."]);
  assert.deepEqual(agents.takeUnreported(), [], "the main turn was still running and saw the result itself");

  agents.setMainBusy(false);
  assert.deepEqual(agents.observe("turn/started", { threadId: "grandchild-1234bbbb", turn: { id: "tb" } }), ["Background agent 1234bbbb started."], "an agent's own spawns are found from their events");
  assert.match(agents.observe("turn/completed", { threadId: "grandchild-1234bbbb", turn: { id: "tb", status: "failed", error: { message: "boom" } } })[0], /failed: boom/);
  const ended = agents.takeUnreported();
  assert.deepEqual(ended.map((a) => [a.id, a.status, a.error]), [["grandchild-1234bbbb", "failed", "boom"]]);
  assert.deepEqual(agents.takeUnreported(), []);
  assert.deepEqual(agents.busy(), []);
  assert.deepEqual(agents.observe("configWarning", { summary: "x" }), []);
});

test("followUpPrompt hands each ended agent's result to the main thread", () => {
  const text = followUpPrompt([
    { id: "a", name: "Ada", status: "completed", message: "found it" },
    { id: "b", name: "b", status: "interrupted", message: null },
    { id: "c", name: "Cy", status: "failed", error: "boom" },
  ], { stoppedAfterMs: 600000 });
  assert.match(text, /^\[session-platform\]/);
  assert.match(text, /Ada \(a\): completed\. Its final message:\nfound it/);
  assert.match(text, /- b: stopped by the platform after waiting 10 minute\(s\)\./);
  assert.match(text, /Cy \(c\): failed: boom/);
  const long = followUpPrompt([{ id: "a", name: "a", status: "completed", message: `${"x".repeat(9000)}\nTHE-END` }]);
  assert.match(long, /\[… 816 characters cut …\]/);
  assert.match(long, /THE-END\n/, "the end of a long message survives");
});

test("serverRequestResponse cancels elicitations and refuses the rest, as exec did", () => {
  assert.deepEqual(serverRequestResponse("mcpServer/elicitation/request", { mode: "form", _meta: { codex_approval_kind: "mcp_tool_call" } }), {
    result: { action: "cancel", content: null, _meta: { codex_approval_cancellation_reason: "non_interactive" } },
  });
  assert.equal(serverRequestResponse("mcpServer/elicitation/request", { mode: "url", _meta: null }).result._meta, null);
  const refused = serverRequestResponse("item/commandExecution/requestApproval", {});
  assert.equal(refused.error.code, -32000);
});

test("backgroundWaitFromEnv reads a non-negative millisecond count", () => {
  assert.equal(backgroundWaitFromEnv({}), DEFAULT_BACKGROUND_WAIT_MS);
  assert.equal(backgroundWaitFromEnv({ CODEX_RUNNER_BACKGROUND_WAIT_MS: "250" }), 250);
  for (const bad of ["", "-1", "1.5", "soon"]) {
    assert.equal(backgroundWaitFromEnv({ CODEX_RUNNER_BACKGROUND_WAIT_MS: bad }), DEFAULT_BACKGROUND_WAIT_MS, bad);
  }
});

test("cleanLoginLine strips terminal noise", () => {
  assert.equal(cleanLoginLine("\u001b[94mhttps://auth.openai.com/codex/device\u001b[0m  "), "https://auth.openai.com/codex/device");
  assert.equal(cleanLoginLine("   "), null);
  assert.equal(cleanLoginLine("WARNING: proceeding, even though we could not create PATH aliases: x"), null);
});

test("thread file round-trips and tolerates garbage", () => {
  assert.equal(parseThreadFile(threadFileContent("abc")), "abc");
  for (const bad of ["", "{", "null", '{"version":2,"threadId":"a"}', '{"version":1,"threadId":""}']) {
    assert.equal(parseThreadFile(bad), null, bad);
  }
});

test("inspectPromptInput flags shell tools and bundled skills", () => {
  const exec = { type: "function", name: "exec", description: "### `apply_patch`\n### `exec_command`\n" };
  const items = [
    { type: "additional_tools", tools: [{ type: "namespace", name: "functions", tools: [exec, { name: "view_image" }] }] },
    {
      type: "message",
      role: "developer",
      content: [
        { type: "input_text", text: "<skills_instructions>\n### Skill roots\n- `r0` = `/h/.codex/skills/.system`\n### Available skills\n- imagegen: Images. (file: r0/imagegen/SKILL.md)\n</skills_instructions>" },
      ],
    },
  ];
  const report = inspectPromptInput(items);
  assert.ok(report.tools.includes("functions.exec>exec_command"));
  assert.deepEqual(report.skills, ["imagegen"]);
  assert.equal(report.problems.length, 3, report.problems.join("\n"));

  const clean = [
    { type: "additional_tools", tools: [{ type: "namespace", name: "functions", tools: [{ ...exec, description: "### `apply_patch`\n" }] }] },
    {
      type: "message",
      role: "developer",
      content: [
        { type: "input_text", text: "<skills_instructions>\n- `r0` = `/h/.agents/skills`\n- p--s: Demo. (file: r0/p--s/SKILL.md)\n</skills_instructions>" },
      ],
    },
  ];
  assert.deepEqual(inspectPromptInput(clean).problems, []);
  assert.deepEqual(inspectPromptInput(clean).skills, ["p--s"]);
  // The chat config fails the task check: it has no shell.
  assert.equal(inspectPromptInput(clean, "task").problems.length, 1, inspectPromptInput(clean, "task").problems.join("\n"));
});

test("inspectPromptInput requires a shell in task mode", () => {
  const exec = { type: "function", name: "exec", description: "### `apply_patch`\n### `exec_command`\n" };
  const task = [
    { type: "additional_tools", tools: [{ type: "namespace", name: "functions", tools: [exec] }] },
  ];
  assert.deepEqual(inspectPromptInput(task, "task").problems, []);
  const leaky = structuredClone(task);
  leaky[0].tools[0].tools.push({ name: "view_image" }, { name: "web_search" });
  assert.equal(inspectPromptInput(leaky, "task").problems.length, 2);
  // A task config must never pass the chat check: its shell is model-visible.
  assert.ok(inspectPromptInput(task).problems.length >= 1);
});

// The end-to-end cases drive runner.mjs against a fake codex: a node script
// that answers `login status` and `login --device-auth`, and as `app-server`
// plays the JSON-RPC scenario in $CODEX_HOME/scenario.json — one list of
// steps per turn/start, plus the steps that follow a turn/interrupt — while
// recording argv, its environment and every message the runner sends.
function fakeCodex(dir) {
  const bin = path.join(dir, "codex");
  writeFileSync(
    bin,
    `#!/usr/bin/env node
const fs = require("node:fs");
const path = require("node:path");
const home = process.env.CODEX_HOME;
const argv = process.argv.slice(2);
fs.appendFileSync(path.join(home, "argv.log"), argv.join(" ") + "\\n");
fs.writeFileSync(path.join(home, "env." + argv[0] + ".log"), Object.keys(process.env).sort().map((k) => k + "=" + process.env[k]).join("\\n"));
if (argv[0] === "login" && argv[1] === "status") {
  if (fs.existsSync(path.join(home, "auth.json"))) process.exit(0);
  console.error("Not logged in");
  process.exit(1);
}
if (argv[0] === "login" && argv[1] === "--device-auth") {
  if (fs.existsSync(path.join(home, "login-fails"))) { console.error("Error logging in"); process.exit(1); }
  console.log("\\u001b[94mhttps://auth.openai.com/codex/device\\u001b[0m");
  console.log("Enter this one-time code: ABCD-1234");
  fs.writeFileSync(path.join(home, "auth.json"), "{}");
  process.exit(0);
}
const scenario = JSON.parse(fs.readFileSync(path.join(home, "scenario.json"), "utf8"));
const log = (m) => fs.appendFileSync(path.join(home, "requests.jsonl"), JSON.stringify(m) + "\\n");
const send = (m) => process.stdout.write(JSON.stringify(m) + "\\n");
let turns = 0;
let nextServerRequest = 1000;
const play = async (steps) => {
  for (const step of steps || []) {
    if (step.delay) await new Promise((r) => setTimeout(r, step.delay));
    else if (step.request) send({ id: nextServerRequest++, method: step.request.method, params: step.request.params });
    else send(step);
  }
};
const thread = (id) => ({ id, parentThreadId: null, agentNickname: null, turns: [] });
require("node:readline").createInterface({ input: process.stdin }).on("line", (line) => {
  const m = JSON.parse(line);
  log(m);
  if (m.method === "initialize") send({ id: m.id, result: { userAgent: "fake" } });
  else if (m.method === "thread/start") send({ id: m.id, result: { thread: thread(scenario.threadId) } });
  else if (m.method === "thread/resume") send({ id: m.id, result: { thread: thread(m.params.threadId) } });
  else if (m.method === "turn/start") {
    const turn = scenario.turns[turns++] || { id: "extra", steps: [] };
    send({ id: m.id, result: { turn: { id: turn.id, status: "inProgress", items: [] } } });
    play(turn.steps);
  } else if (m.method === "turn/interrupt") {
    send({ id: m.id, result: {} });
    play((scenario.interrupts || {})[m.params.threadId]);
  }
});
`,
  );
  chmodSync(bin, 0o755);
  return bin;
}

function runRunner(home, args, scenario, extraEnv = {}) {
  const codexHome = path.join(home, ".codex");
  mkdirSync(codexHome, { recursive: true });
  writeFileSync(path.join(codexHome, "scenario.json"), JSON.stringify(scenario));
  rmSync(path.join(codexHome, "requests.jsonl"), { force: true });
  const result = spawnSync(process.execPath, [path.join(here, "runner.mjs"), ...args], {
    cwd: home,
    env: { HOME: home, PATH: process.env.PATH, CODEX_BIN: fakeCodex(home), OPENAI_API_KEY: "sk-leak", ...extraEnv },
    encoding: "utf8",
    timeout: 20000,
  });
  const lines = result.stdout.split("\n").filter(Boolean).map((l) => JSON.parse(l));
  const requestsFile = path.join(codexHome, "requests.jsonl");
  const requests = existsSync(requestsFile)
    ? readFileSync(requestsFile, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l))
    : [];
  return { status: result.status, lines, stderr: result.stderr, codexHome, requests };
}

function signedIn(home) {
  mkdirSync(path.join(home, ".codex"), { recursive: true });
  writeFileSync(path.join(home, ".codex", "auth.json"), "{}");
}

function withHome(fn) {
  const home = mkdtempSync(path.join(os.tmpdir(), "codex-runner-"));
  try {
    fn(home);
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
}

const n = (method, params) => ({ method, params });
const started = (threadId, turnId) => n("turn/started", { threadId, turn: { id: turnId, status: "inProgress", items: [] } });
const item = (threadId, turnId, it) => n("item/completed", { threadId, turnId, item: it, completedAtMs: 0 });
const itemStarted = (threadId, turnId, it) => n("item/started", { threadId, turnId, item: it, startedAtMs: 0 });
const done = (threadId, turnId, status = "completed", extra = {}) =>
  n("turn/completed", { threadId, turn: { id: turnId, status, items: [], error: null, ...extra } });
const message = (id, text) => ({ id, type: "agentMessage", text, phase: null });
// The app server announces a spawned agent only through the spawning
// thread's spawnAgent item and the agent's own events — no thread/started.
const spawned = (id) => item("th-1", "t1", {
  id: `spawn-${id}`, type: "collabAgentToolCall", tool: "spawnAgent", status: "completed",
  senderThreadId: "th-1", receiverThreadIds: [id], prompt: "go", agentsStates: { [id]: { status: "pendingInit", message: null } },
});
const activity = (id, agentPath) => item("th-1", "t1", { id: `act-${id}`, type: "subAgentActivity", kind: "started", agentThreadId: id, agentPath });
const notices = (lines) => lines.filter((l) => l.type === NOTICE_TYPE).map((l) => l.text);
const turnStarts = (requests) => requests.filter((r) => r.method === "turn/start");

test("runner signs in by device code, runs the turn and keeps the thread", () => {
  withHome((home) => {
    const first = runRunner(home, ["--model", "platform-default", "--", "hello"], {
      threadId: "th-1",
      turns: [{ id: "t1", steps: [started("th-1", "t1"), item("th-1", "t1", message("1", "hi")), done("th-1", "t1")] }],
    });
    assert.equal(first.status, EXIT_OK, first.stderr);
    assert.ok(notices(first.lines).includes("https://auth.openai.com/codex/device"), notices(first.lines).join("\n"));
    assert.ok(notices(first.lines).includes("Enter this one-time code: ABCD-1234"));
    assert.deepEqual(first.lines.filter((l) => l.type !== NOTICE_TYPE).map((l) => l.type), [
      "thread.started", "turn.started", "item.completed", "turn.completed",
    ]);
    assert.ok(first.lines.some((l) => l.type === "item.completed" && l.item.type === "agent_message" && l.item.text === "hi"));
    assert.equal(parseThreadFile(readFileSync(path.join(first.codexHome, "session-platform-thread.json"), "utf8")), "th-1");
    const env = readFileSync(path.join(first.codexHome, "env.app-server.log"), "utf8");
    assert.ok(!env.includes("sk-leak"), "the codex child must not inherit credentials");
    const init = first.requests.find((r) => r.method === "initialize");
    assert.equal(init.params.clientInfo.name, "codex_sdk_ts");
    assert.ok(first.requests.some((r) => r.method === "initialized"));
    const start = first.requests.find((r) => r.method === "thread/start");
    assert.deepEqual(start.params, { cwd: home, sandbox: "read-only", approvalPolicy: "never" });
    assert.equal(turnStarts(first.requests)[0].params.input[0].text, "hello");
    const argv = readFileSync(path.join(first.codexHome, "argv.log"), "utf8").trim().split("\n").pop();
    assert.match(argv, /^app-server /);
    assert.match(argv, /web_search="disabled"/);
    assert.match(argv, /sandbox_workspace_write\.network_access=false/);

    const second = runRunner(home, ["--model", "gpt-6", "--resume", "--", "again"], {
      threadId: "unused",
      turns: [{ id: "t2", steps: [done("th-1", "t2")] }],
    });
    assert.equal(second.status, EXIT_OK, second.stderr);
    assert.deepEqual(notices(second.lines), [], "a signed-in session is not asked again");
    const resume = second.requests.find((r) => r.method === "thread/resume");
    assert.equal(resume.params.threadId, "th-1");
    assert.equal(resume.params.model, "gpt-6");
    assert.equal(resume.params.sandbox, "read-only");
  });
});

test("runner stops the turn on a forbidden item", () => {
  withHome((home) => {
    signedIn(home);
    const result = runRunner(home, ["--", "rm"], {
      threadId: "th-2",
      turns: [{ id: "t1", steps: [
        itemStarted("th-2", "t1", { id: "c", type: "commandExecution", command: "rm -rf /", status: "inProgress" }),
        item("th-2", "t1", message("2", "after")),
        done("th-2", "t1"),
      ] }],
    });
    assert.equal(result.status, EXIT_KILLED, result.stderr);
    assert.ok(notices(result.lines).some((t) => /run a command/.test(t)));
    assert.ok(!result.lines.some((l) => l.item && l.item.text === "after"), "nothing after the kill switch is forwarded");
  });
});

test("runner stops the turn when a sub-agent does something forbidden", () => {
  withHome((home) => {
    signedIn(home);
    const result = runRunner(home, ["--", "go"], {
      threadId: "th-1",
      turns: [{ id: "t1", steps: [
        spawned("child"),
        started("child", "tc"),
        itemStarted("child", "tc", { id: "f", type: "fileChange", changes: [], status: "inProgress" }),
        done("th-1", "t1"),
      ] }],
    });
    assert.equal(result.status, EXIT_KILLED, result.stderr);
    assert.ok(notices(result.lines).some((t) => /change a file/.test(t)));
  });
});

test("runner in task mode runs commands, passes the effort and opens the sandbox", () => {
  withHome((home) => {
    signedIn(home);
    const result = runRunner(home, ["--mode", "task", "--effort", "high", "--", "ls"], {
      threadId: "th-3",
      turns: [{ id: "t1", steps: [
        item("th-3", "t1", { id: "c", type: "commandExecution", command: "ls", status: "completed", aggregatedOutput: "a", exitCode: 0 }),
        item("th-3", "t1", message("2", "after")),
        done("th-3", "t1"),
      ] }],
    });
    assert.equal(result.status, EXIT_OK, result.stderr);
    assert.ok(result.lines.some((l) => l.item && l.item.text === "after"));
    assert.equal(result.requests.find((r) => r.method === "thread/start").params.sandbox, "danger-full-access");
    const argv = readFileSync(path.join(result.codexHome, "argv.log"), "utf8").trim().split("\n").pop();
    assert.match(argv, /sandbox_workspace_write\.network_access=true/);
    assert.match(argv, /model_reasoning_effort="high"/);
  });
});

test("runner reports a failed turn and a failed sign-in", () => {
  withHome((home) => {
    signedIn(home);
    const failed = runRunner(home, ["--", "x"], {
      threadId: "t",
      turns: [{ id: "t1", steps: [done("t", "t1", "failed", { error: { message: "quota", additionalDetails: null } })] }],
    });
    assert.equal(failed.status, EXIT_TURN_FAILED);
    assert.ok(failed.lines.some((l) => l.type === "turn.failed" && l.error.message === "quota"));

    rmSync(path.join(home, ".codex", "auth.json"));
    writeFileSync(path.join(home, ".codex", "login-fails"), "");
    const login = runRunner(home, ["--", "x"], { threadId: "t", turns: [] });
    assert.equal(login.status, EXIT_LOGIN_FAILED);
    assert.ok(notices(login.lines).some((t) => /did not complete/.test(t)));
  });
});

test("runner waits for sub-agents the turn left running and hands their results to a follow-up turn", () => {
  withHome((home) => {
    signedIn(home);
    const result = runRunner(home, ["--", "research"], {
      threadId: "th-1",
      turns: [
        { id: "t1", steps: [
          started("th-1", "t1"),
          spawned("child"),
          activity("child", "/root/Ada"),
          started("child", "tc"),
          item("th-1", "t1", message("m1", "Started Ada; I'll report back.")),
          done("th-1", "t1"),
          { delay: 300 },
          itemStarted("child", "tc", { id: "k", type: "mcpToolCall", server: "k3s", tool: "ping", status: "inProgress" }),
          item("child", "tc", message("cm", "Ada's findings")),
          done("child", "tc"),
          item("th-1", "t1", { id: "act-done", type: "subAgentActivity", kind: "completed", agentThreadId: "child", agentPath: "/root/Ada" }),
        ] },
        { id: "t2", steps: [started("th-1", "t2"), item("th-1", "t2", message("m2", "Ada found: findings")), done("th-1", "t2")] },
      ],
    });
    assert.equal(result.status, EXIT_OK, result.stderr);
    const texts = result.lines.map((l) => (l.type === NOTICE_TYPE ? `notice:${l.text}` : l.item ? `${l.type}:${l.item.text}` : l.type));
    const order = [
      "item.completed:Started Ada; I'll report back.",
      "notice:Waiting for 1 background agent(s) before this turn ends: Ada.",
      "notice:· Ada: k3s.ping",
      "notice:Background agent Ada finished.",
      "notice:Background agents ended; the main agent continues with their results.",
      "item.completed:Ada found: findings",
    ];
    let at = -1;
    for (const want of order) {
      const next = texts.indexOf(want, at + 1);
      assert.ok(next > at, `${want} after position ${at} in:\n${texts.join("\n")}`);
      at = next;
    }
    assert.ok(!texts.includes("item.completed:Ada's findings"), "a sub-agent's messages are not the main thread's");
    const followUp = turnStarts(result.requests)[1].params;
    assert.equal(followUp.threadId, "th-1");
    assert.match(followUp.input[0].text, /Ada \(child\): completed\. Its final message:\nAda's findings/);
  });
});

test("runner stops sub-agents that outlast the wait and still lets the main thread answer", () => {
  withHome((home) => {
    signedIn(home);
    const result = runRunner(home, ["--", "research"], {
      threadId: "th-1",
      turns: [
        { id: "t1", steps: [spawned("slow-agent"), started("slow-agent", "ts"), done("th-1", "t1")] },
        { id: "t2", steps: [item("th-1", "t2", message("m2", "slow was stopped")), done("th-1", "t2")] },
      ],
      interrupts: { "slow-agent": [done("slow-agent", "ts", "interrupted")] },
    }, { CODEX_RUNNER_BACKGROUND_WAIT_MS: "200" });
    assert.equal(result.status, EXIT_OK, result.stderr);
    assert.ok(result.requests.some((r) => r.method === "turn/interrupt" && r.params.threadId === "slow-agent" && r.params.turnId === "ts"));
    assert.ok(notices(result.lines).some((t) => /still running after .* stopping ow-agent/.test(t)), notices(result.lines).join("\n"));
    assert.match(turnStarts(result.requests)[1].params.input[0].text, /- ow-agent \(slow-agent\): stopped by the platform after waiting 0 minute\(s\)\./);
    assert.ok(result.lines.some((l) => l.item && l.item.text === "slow was stopped"));
  });
});

test("runner does not wait for agents the turn already finished with", () => {
  withHome((home) => {
    signedIn(home);
    const result = runRunner(home, ["--", "quick"], {
      threadId: "th-1",
      turns: [{ id: "t1", steps: [
        spawned("child"), started("child", "tc"), done("child", "tc"),
        item("th-1", "t1", message("m", "Ada said hi")), done("th-1", "t1"),
      ] }],
    });
    assert.equal(result.status, EXIT_OK, result.stderr);
    assert.equal(turnStarts(result.requests).length, 1);
    assert.ok(!notices(result.lines).some((t) => /Waiting/.test(t)));
  });
});

test("runner cancels MCP elicitations as non-interactive", () => {
  withHome((home) => {
    signedIn(home);
    const result = runRunner(home, ["--", "write"], {
      threadId: "th-1",
      turns: [{ id: "t1", steps: [
        { request: { method: "mcpServer/elicitation/request", params: { threadId: "th-1", turnId: "t1", serverName: "k3s", mode: "form", _meta: { codex_approval_kind: "mcp_tool_call" }, message: "ok?", requestedSchema: {} } } },
        { delay: 100 },
        done("th-1", "t1"),
      ] }],
    });
    assert.equal(result.status, EXIT_OK, result.stderr);
    const answer = result.requests.find((r) => r.id === 1000);
    assert.deepEqual(answer.result, { action: "cancel", content: null, _meta: { codex_approval_cancellation_reason: "non_interactive" } });
  });
});
