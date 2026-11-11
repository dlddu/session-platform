import { test } from "node:test";
import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  HEADLESS_DENIAL,
  NOTICE_TYPE,
  PLATFORM_DEFAULT_MODEL,
  UsageError,
  classify,
  decideTool,
  noticeLine,
  parseArgs,
  parseSessionFile,
  pluginToolPrefix,
  pluginsFromSettings,
  queryOptions,
  resolveClaudeBin,
  resumePlan,
  sameCliVersion,
  sessionFileContent,
} from "./lib.mjs";

test("parseArgs reads the agent's argv and keeps the prompt whole", () => {
  assert.deepEqual(parseArgs(["--model", "claude-opus-5-5", "--effort", "max", "--resume", "--", "--help me"]), {
    model: "claude-opus-5-5",
    effort: "max",
    resume: true,
    prompt: "--help me",
    checkCli: false,
  });
  const bare = parseArgs(["--", "hi"]);
  assert.equal(bare.model, PLATFORM_DEFAULT_MODEL);
  assert.equal(bare.effort, null);
  assert.equal(bare.resume, false);
  assert.equal(parseArgs(["--check-cli"]).checkCli, true);
});

test("parseArgs refuses what the agent never sends", () => {
  for (const argv of [
    [],
    ["--", "a", "b"],
    ["--model"],
    ["--model", "", "--", "x"],
    ["--effort", "turbo", "--", "x"],
    ["--continue", "--", "x"],
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
  assert.equal(base.permissionMode, "default");
  assert.equal(base.includePartialMessages, true);
  assert.equal(base.canUseTool, canUseTool);
  assert.equal(base.pathToClaudeCodeExecutable, "/usr/local/bin/claude");
  for (const key of ["model", "effort", "resume", "continue"]) {
    assert.equal(key in base, false, key);
  }

  const resumed = queryOptions({
    model: "claude-opus-5-5",
    effort: "high",
    cwd: "/w",
    claudeBin: "/c",
    plan: { kind: "resume", sessionId: "s" },
  });
  assert.equal(resumed.model, "claude-opus-5-5");
  assert.equal(resumed.effort, "high");
  assert.equal(resumed.resume, "s");
  assert.equal("continue" in resumed, false);

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
});

test("decideTool denies everything else with a reason the model can act on", () => {
  const plugins = ["session-platform"];
  // A server a cloned repository's .mcp.json brings is not the session's.
  const foreign = decideTool("mcp__repo-server__exfiltrate", plugins);
  assert.equal(foreign.allow, false);
  assert.match(foreign.message, /not part of this session's tool surface/);
  // A plugin the session did not choose is not the session's either.
  assert.equal(decideTool("mcp__plugin_other_x__y", plugins).allow, false);
  assert.equal(decideTool("mcp__plugin_session-platformx_k__y", plugins).allow, false);
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
