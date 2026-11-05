import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  EXIT_KILLED,
  EXIT_LOGIN_FAILED,
  EXIT_OK,
  EXIT_TURN_FAILED,
  NOTICE_TYPE,
  PLATFORM_DEFAULT_MODEL,
  UsageError,
  classify,
  cleanLoginLine,
  minimalEnv,
  parseArgs,
  parseThreadFile,
  threadFileContent,
  threadOptions,
} from "./lib.mjs";
import { inspectPromptInput } from "./surface.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));

test("parseArgs reads the agent's argv and keeps the prompt whole", () => {
  assert.deepEqual(parseArgs(["--model", "gpt-6", "--resume", "--", "--help me"]), {
    model: "gpt-6",
    resume: true,
    prompt: "--help me",
    printCodexBin: false,
  });
  assert.equal(parseArgs(["--", "hi"]).model, PLATFORM_DEFAULT_MODEL);
  assert.equal(parseArgs(["--model", "platform-default", "--", "hi"]).resume, false);
  for (const bad of [[], ["hi"], ["--model"], ["--model", "", "--", "x"], ["--", "a", "b"], ["--bogus", "--", "x"]]) {
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

test("threadOptions pins the read-only, never-ask, offline surface", () => {
  assert.deepEqual(threadOptions(PLATFORM_DEFAULT_MODEL, "/w"), {
    sandboxMode: "read-only",
    approvalPolicy: "never",
    workingDirectory: "/w",
    skipGitRepoCheck: true,
    webSearchMode: "disabled",
    networkAccessEnabled: false,
  });
  assert.equal(threadOptions("gpt-6", "/w").model, "gpt-6");
});

test("classify finds forbidden items, the thread id and the outcome", () => {
  for (const type of ["command_execution", "file_change", "web_search"]) {
    for (const event of ["item.started", "item.updated", "item.completed"]) {
      assert.equal(classify({ type: event, item: { type } }).forbidden, type);
    }
  }
  assert.deepEqual(classify({ type: "item.completed", item: { type: "agent_message", text: "x" } }), {});
  assert.deepEqual(classify({ type: "item.started", item: { type: "mcp_tool_call" } }), {});
  assert.deepEqual(classify({ type: "thread.started", thread_id: "t-1" }), { threadId: "t-1" });
  assert.deepEqual(classify({ type: "turn.completed", usage: {} }), { outcome: "completed" });
  assert.deepEqual(classify({ type: "turn.failed", error: { message: "x" } }), { outcome: "failed" });
  assert.deepEqual(classify({ type: "error", message: "x" }), { outcome: "failed" });
  assert.deepEqual(classify(null), {});
  assert.deepEqual(classify("x"), {});
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

test("inspectPromptInput flags shell tools, bundled skills and a missing config", () => {
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
  assert.equal(report.problems.length, 4, report.problems.join("\n"));

  const clean = [
    { type: "additional_tools", tools: [{ type: "namespace", name: "functions", tools: [{ ...exec, description: "### `apply_patch`\n" }] }] },
    {
      type: "message",
      role: "developer",
      content: [
        { type: "input_text", text: "You are a session-platform codex session." },
        { type: "input_text", text: "<skills_instructions>\n- `r0` = `/h/.agents/skills`\n- p--s: Demo. (file: r0/p--s/SKILL.md)\n</skills_instructions>" },
      ],
    },
  ];
  assert.deepEqual(inspectPromptInput(clean).problems, []);
  assert.deepEqual(inspectPromptInput(clean).skills, ["p--s"]);
});

// The end-to-end cases drive runner.mjs, with the real SDK, against a fake
// codex: a shell script that answers `login status`, `login --device-auth`
// and `exec --experimental-json` from FAKE_* variables, and records argv.
const sdkInstalled = existsSync(path.join(here, "node_modules", "@openai", "codex-sdk"));

function fakeCodex(dir) {
  const bin = path.join(dir, "codex");
  writeFileSync(
    bin,
    `#!/bin/sh
printf '%s\\n' "$*" >> "$CODEX_HOME/argv.log"
env | sort > "$CODEX_HOME/env.$1.log"
case "$1 $2" in
  "login status") [ -f "$CODEX_HOME/auth.json" ] && exit 0; echo "Not logged in" >&2; exit 1 ;;
  "login --device-auth")
    [ -f "$CODEX_HOME/login-fails" ] && { echo "Error logging in" >&2; exit 1; }
    printf '\\033[94mhttps://auth.openai.com/codex/device\\033[0m\\n'
    echo "Enter this one-time code: ABCD-1234"
    echo '{}' > "$CODEX_HOME/auth.json"; exit 0 ;;
esac
cat > /dev/null
cat "$CODEX_HOME/events.jsonl"
`,
  );
  chmodSync(bin, 0o755);
  return bin;
}

function runRunner(home, args, events, extraEnv = {}) {
  const codexHome = path.join(home, ".codex");
  mkdirSync(codexHome, { recursive: true });
  writeFileSync(path.join(codexHome, "events.jsonl"), events.map((e) => JSON.stringify(e)).join("\n") + "\n");
  const result = spawnSync(process.execPath, [path.join(here, "runner.mjs"), ...args], {
    cwd: home,
    env: { HOME: home, PATH: process.env.PATH, CODEX_BIN: fakeCodex(home), OPENAI_API_KEY: "sk-leak", ...extraEnv },
    encoding: "utf8",
  });
  const lines = result.stdout.split("\n").filter(Boolean).map((l) => JSON.parse(l));
  return { status: result.status, lines, stderr: result.stderr, codexHome };
}

const turn = (threadId, ...items) => [
  { type: "thread.started", thread_id: threadId },
  { type: "turn.started" },
  ...items,
  { type: "turn.completed", usage: { input_tokens: 1, cached_input_tokens: 0, output_tokens: 1 } },
];

test("runner signs in by device code, runs the turn and keeps the thread", { skip: !sdkInstalled }, () => {
  const home = mkdtempSync(path.join(os.tmpdir(), "codex-runner-"));
  try {
    const first = runRunner(home, ["--model", "platform-default", "--", "hello"], turn("th-1", {
      type: "item.completed",
      item: { id: "1", type: "agent_message", text: "hi" },
    }));
    assert.equal(first.status, EXIT_OK, first.stderr);
    const notices = first.lines.filter((l) => l.type === NOTICE_TYPE).map((l) => l.text);
    assert.ok(notices.includes("https://auth.openai.com/codex/device"), notices.join("\n"));
    assert.ok(notices.includes("Enter this one-time code: ABCD-1234"));
    assert.ok(first.lines.some((l) => l.type === "item.completed" && l.item.text === "hi"));
    assert.equal(parseThreadFile(readFileSync(path.join(first.codexHome, "session-platform-thread.json"), "utf8")), "th-1");
    const env = readFileSync(path.join(first.codexHome, "env.exec.log"), "utf8");
    assert.ok(!env.includes("sk-leak"), "the codex child must not inherit credentials");

    const second = runRunner(home, ["--model", "gpt-6", "--resume", "--", "again"], turn("th-1"));
    assert.equal(second.status, EXIT_OK, second.stderr);
    assert.ok(!second.lines.some((l) => l.type === NOTICE_TYPE), "a signed-in session is not asked again");
    const argv = readFileSync(path.join(second.codexHome, "argv.log"), "utf8").trim().split("\n").pop();
    assert.match(argv, /--model gpt-6/);
    assert.match(argv, /--sandbox read-only/);
    assert.match(argv, /resume th-1$/);
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});

test("runner stops the turn on a forbidden item", { skip: !sdkInstalled }, () => {
  const home = mkdtempSync(path.join(os.tmpdir(), "codex-runner-"));
  try {
    mkdirSync(path.join(home, ".codex"), { recursive: true });
    writeFileSync(path.join(home, ".codex", "auth.json"), "{}");
    const result = runRunner(home, ["--", "rm"], turn("th-2", {
      type: "item.started",
      item: { id: "c", type: "command_execution", command: "rm -rf /", status: "in_progress" },
    }, { type: "item.completed", item: { id: "2", type: "agent_message", text: "after" } }));
    assert.equal(result.status, EXIT_KILLED, result.stderr);
    assert.ok(result.lines.some((l) => l.type === NOTICE_TYPE && /run a command/.test(l.text)));
    assert.ok(!result.lines.some((l) => l.item && l.item.text === "after"), "nothing after the kill switch is forwarded");
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});

test("runner reports a failed turn and a failed sign-in", { skip: !sdkInstalled }, () => {
  const home = mkdtempSync(path.join(os.tmpdir(), "codex-runner-"));
  try {
    mkdirSync(path.join(home, ".codex"), { recursive: true });
    writeFileSync(path.join(home, ".codex", "auth.json"), "{}");
    const failed = runRunner(home, ["--", "x"], [{ type: "thread.started", thread_id: "t" }, { type: "turn.failed", error: { message: "quota" } }]);
    assert.equal(failed.status, EXIT_TURN_FAILED);

    rmSync(path.join(home, ".codex", "auth.json"));
    writeFileSync(path.join(home, ".codex", "login-fails"), "");
    const login = runRunner(home, ["--", "x"], []);
    assert.equal(login.status, EXIT_LOGIN_FAILED);
    assert.ok(login.lines.some((l) => l.type === NOTICE_TYPE && /did not complete/.test(l.text)));
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});
