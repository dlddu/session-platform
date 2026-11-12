// Pure helpers of claude-runner (docs/prd/claude-code-workload.md, AC-E2/E4),
// kept apart from runner.mjs so `node --test` can exercise them without the
// SDK, a claude binary or a provider.
import { accessSync, constants, realpathSync } from "node:fs";
import path from "node:path";

export const PLATFORM_DEFAULT_MODEL = "platform-default";
export const NOTICE_TYPE = "session_platform.notice";
// The conversation this session resumes (AC-E4). It lives in the session HOME
// next to the managed settings, so the filesystem archive carries it.
export const SESSION_FILE = path.join(".claude", "session-platform-session.json");
export const SETTINGS_FILE = path.join(".claude", "settings.json");
export const PLUGIN_MARKETPLACE = "dlddu-plugins";

// Exit codes the agent reads only as success or failure, but which keep a
// failed turn (1) and a usage error (64) apart in the logs.
export const EXIT_OK = 0;
export const EXIT_TURN_FAILED = 1;
export const EXIT_USAGE = 64;

// AC-E8's list for claude-code; the SDK's EffortLevel.
export const EFFORTS = new Set(["low", "medium", "high", "xhigh", "max"]);

// AC-E2's permission modes a write may choose for its turn. "auto" lets the
// CLI's model classifier approve or deny what the settings rules leave open;
// "acceptEdits" accepts file edits and leaves the rest to canUseTool. No mode
// that skips permission checks is offered.
export const DEFAULT_PERMISSION_MODE = "auto";
export const PERMISSION_MODES = new Set([DEFAULT_PERMISSION_MODE, "acceptEdits"]);

// AC-E2's permission policy: the built-in tools the platform lets a
// claude-code turn use. Read-only and bookkeeping tools never ask; the rest
// reach canUseTool only when neither a settings rule nor the turn's permission
// mode already decided them. Anything a turn asks for that is not here, not a
// tool of the session's own plugins and not a headless question is denied.
export const PLATFORM_TOOLS = new Set([
  "Read",
  "Write",
  "Edit",
  "MultiEdit",
  "NotebookEdit",
  "Glob",
  "Grep",
  "LS",
  "Bash",
  "BashOutput",
  "KillShell",
  "KillBash",
  "TodoWrite",
  "Task",
  "TaskOutput",
  "TaskStop",
  "Agent",
  "Skill",
  "SlashCommand",
  "ToolSearch",
  "WebFetch",
  "WebSearch",
]);

// Tools that wait for a person. A one-shot turn has nobody to answer, so they
// are denied with a message that tells the model to decide for itself.
export const HEADLESS_TOOLS = new Set(["AskUserQuestion", "EnterPlanMode", "ExitPlanMode"]);

export const HEADLESS_DENIAL =
  "This session runs headless: nobody can answer a question or approve a plan during the turn. " +
  "Decide on your own, say which assumption you made, and continue.";

export class UsageError extends Error {}

// parseArgs reads the agent's argv:
// [--model <m>] [--effort <e>] [--permission-mode <p>] [--resume] -- <prompt>.
// The prompt is the one argument after "--", so nothing in it is a flag.
export function parseArgs(argv) {
  let model = PLATFORM_DEFAULT_MODEL;
  let effort = null;
  let permissionMode = DEFAULT_PERMISSION_MODE;
  let resume = false;
  let checkCli = false;
  const value = (i, flag) => {
    if (i + 1 >= argv.length || argv[i + 1] === "") {
      throw new UsageError(`${flag} needs a value`);
    }
    return argv[i + 1];
  };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--") {
      const rest = argv.slice(i + 1);
      if (rest.length !== 1) {
        throw new UsageError("expected exactly one prompt argument after --");
      }
      return { model, effort, permissionMode, resume, prompt: rest[0], checkCli };
    }
    if (arg === "--model") {
      model = value(i++, "--model");
    } else if (arg === "--effort") {
      effort = value(i++, "--effort");
      if (!EFFORTS.has(effort)) {
        throw new UsageError(`unknown effort ${JSON.stringify(effort)}`);
      }
    } else if (arg === "--permission-mode") {
      permissionMode = value(i++, "--permission-mode");
      if (!PERMISSION_MODES.has(permissionMode)) {
        throw new UsageError(`unknown permission mode ${JSON.stringify(permissionMode)}`);
      }
    } else if (arg === "--resume") {
      resume = true;
    } else if (arg === "--check-cli") {
      checkCli = true;
    } else {
      throw new UsageError(`unknown argument ${JSON.stringify(arg)}`);
    }
  }
  if (checkCli) {
    return { model, effort, permissionMode, resume, prompt: null, checkCli };
  }
  throw new UsageError("missing -- <prompt>");
}

// resolveClaudeBin is the Claude Code executable the SDK drives: CLAUDE_CODE_BIN
// when it is a path, otherwise that name (default `claude`) looked up on PATH.
// The image installs the SDK without its bundled binary, so this is the one
// pinned CLI the plugin bootstrap also uses.
export function resolveClaudeBin(env) {
  const name = env.CLAUDE_CODE_BIN || "claude";
  if (name.includes(path.sep)) {
    return realpathSync(name);
  }
  for (const dir of (env.PATH || "/usr/local/bin:/usr/bin:/bin").split(path.delimiter)) {
    if (!dir) {
      continue;
    }
    const candidate = path.join(dir, name);
    try {
      accessSync(candidate, constants.X_OK);
      return realpathSync(candidate);
    } catch {
      // Not in this directory.
    }
  }
  throw new Error(`no ${name} executable on PATH`);
}

// sameCliVersion reports whether `claude --version` output names the version
// the SDK was built against. The SDK and the CLI speak a private protocol, so
// the image build refuses a pair that drifted apart.
export function sameCliVersion(versionOutput, sdkCliVersion) {
  const match = /(\d+\.\d+\.\d+)/.exec(String(versionOutput));
  return Boolean(match) && match[1] === sdkCliVersion;
}

export function parseSessionFile(content) {
  try {
    const doc = JSON.parse(content);
    if (doc && doc.version === 1 && typeof doc.sessionId === "string" && /^[0-9a-f-]{36}$/i.test(doc.sessionId)) {
      return doc.sessionId;
    }
  } catch {
    // An unreadable record is no record.
  }
  return null;
}

export function sessionFileContent(sessionId) {
  return JSON.stringify({ version: 1, sessionId }) + "\n";
}

// resumePlan says how a turn picks up the conversation (AC-E4). A first turn
// starts one. A later turn resumes the recorded session id; with no record —
// a session archived before the SDK runner existed — it continues the most
// recent conversation, as the CLI's --continue did. A record whose transcript
// is gone starts over, and says so, rather than guessing another conversation.
export function resumePlan({ resume, recordedId, transcriptExists }) {
  if (!resume) {
    return { kind: "new" };
  }
  if (!recordedId) {
    return { kind: "continue" };
  }
  if (!transcriptExists) {
    return { kind: "lost", sessionId: recordedId };
  }
  return { kind: "resume", sessionId: recordedId };
}

// pluginsFromSettings reads the plugins the managed settings enable (AC-E6,
// AC-L1) — the pod writes that file on every boot and restore, so it is the
// session's own selection.
export function pluginsFromSettings(content) {
  try {
    const doc = JSON.parse(content);
    const enabled = doc && doc.enabledPlugins;
    if (!enabled || typeof enabled !== "object") {
      return [];
    }
    const suffix = `@${PLUGIN_MARKETPLACE}`;
    return Object.entries(enabled)
      .filter(([key, on]) => on === true && key.endsWith(suffix))
      .map(([key]) => key.slice(0, -suffix.length))
      .filter((name) => name !== "");
  } catch {
    return [];
  }
}

// pluginToolPrefix is how Claude Code names an MCP tool a plugin brings:
// mcp__plugin_<plugin>_<server>__<tool>, with characters outside
// [A-Za-z0-9_-] replaced by "_".
export function pluginToolPrefix(plugin) {
  return `mcp__plugin_${plugin.replace(/[^A-Za-z0-9_-]/g, "_")}_`;
}

// decideTool is AC-E2's permission policy for one tool request the settings
// rules left open. It returns { allow: true } or { allow: false, message }.
export function decideTool(toolName, plugins = []) {
  if (PLATFORM_TOOLS.has(toolName)) {
    return { allow: true };
  }
  if (plugins.some((plugin) => toolName.startsWith(pluginToolPrefix(plugin)))) {
    return { allow: true };
  }
  if (HEADLESS_TOOLS.has(toolName)) {
    return { allow: false, message: HEADLESS_DENIAL };
  }
  return {
    allow: false,
    message:
      `${toolName} is not part of this session's tool surface. ` +
      "Use the built-in coding tools or the session's own plugins instead.",
  };
}

// queryOptions are the SDK options of one turn. The system prompt and the
// setting sources are spelled out because the SDK's defaults differ from the
// CLI's: without the preset it sends an empty system prompt.
export function queryOptions({
  model,
  effort,
  permissionMode = DEFAULT_PERMISSION_MODE,
  cwd,
  claudeBin,
  plan,
  canUseTool,
  stderr,
}) {
  const options = {
    cwd,
    pathToClaudeCodeExecutable: claudeBin,
    systemPrompt: { type: "preset", preset: "claude_code" },
    settingSources: ["user", "project", "local"],
    permissionMode,
    includePartialMessages: true,
    canUseTool,
    stderr,
  };
  if (model && model !== PLATFORM_DEFAULT_MODEL) {
    options.model = model;
  }
  if (effort) {
    options.effort = effort;
  }
  if (plan.kind === "resume") {
    options.resume = plan.sessionId;
  } else if (plan.kind === "continue") {
    options.continue = true;
  }
  return options;
}

export function noticeLine(text) {
  return JSON.stringify({ type: NOTICE_TYPE, text: `[session-platform: ${text}]` }) + "\n";
}

// classify says what one SDK message means for the run: the session id to
// record, or the turn's outcome.
export function classify(message) {
  if (!message || typeof message !== "object") {
    return {};
  }
  if (message.type === "system" && message.subtype === "init" && typeof message.session_id === "string" && message.session_id) {
    return { sessionId: message.session_id };
  }
  if (message.type === "result") {
    const ok = message.subtype === "success" && message.is_error !== true;
    const errors = Array.isArray(message.errors) ? message.errors.filter((e) => typeof e === "string" && e) : [];
    return { outcome: ok ? "completed" : "failed", errors };
  }
  return {};
}
