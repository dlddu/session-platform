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

// Tools that wait for a person. Plan mode has nobody to approve the plan, so
// it is denied. AskUserQuestion is relayed to a person through the agent when
// the turn has a questions URL (askPerson), and denied the same way when it
// has none. A denial states what happened and nothing more: the platform does
// not tell the model how to behave (docs/prd/architecture.md AC-A5).
export const HEADLESS_TOOLS = new Set(["AskUserQuestion", "EnterPlanMode", "ExitPlanMode"]);
export const QUESTION_TOOL = "AskUserQuestion";

export const HEADLESS_DENIAL =
  "This session runs headless: nobody can answer a question or approve a plan during the turn.";

export class UsageError extends Error {}

// parseArgs reads the agent's argv:
// [--model <m>] [--effort <e>] [--permission-mode <p>] [--k3s-mcp-url <u>]
// [--questions-url <u>] [--resume] -- <prompt>.
// The prompt is the one argument after "--", so nothing in it is a flag.
export function parseArgs(argv) {
  let model = PLATFORM_DEFAULT_MODEL;
  let effort = null;
  let permissionMode = DEFAULT_PERMISSION_MODE;
  let k3sMcpUrl = null;
  let questionsUrl = null;
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
      return { model, effort, permissionMode, k3sMcpUrl, questionsUrl, resume, prompt: rest[0], checkCli };
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
    } else if (arg === "--k3s-mcp-url") {
      k3sMcpUrl = loopbackHttpUrl(value(i++, "--k3s-mcp-url"), "--k3s-mcp-url");
    } else if (arg === "--questions-url") {
      questionsUrl = loopbackHttpUrl(value(i++, "--questions-url"), "--questions-url").replace(/\/+$/, "");
    } else if (arg === "--resume") {
      resume = true;
    } else if (arg === "--check-cli") {
      checkCli = true;
    } else {
      throw new UsageError(`unknown argument ${JSON.stringify(arg)}`);
    }
  }
  if (checkCli) {
    return { model, effort, permissionMode, k3sMcpUrl, questionsUrl, resume, prompt: null, checkCli };
  }
  throw new UsageError("missing -- <prompt>");
}

// loopbackHttpUrl accepts an address inside the pod only: the agent never
// points a turn's K3s MCP anywhere but this pod's own sidecar (AC-E6), nor its
// questions anywhere but itself (AC-E2).
function loopbackHttpUrl(raw, flag) {
  let parsed;
  try {
    parsed = new URL(raw);
  } catch {
    throw new UsageError(`${flag} is not a URL: ${JSON.stringify(raw)}`);
  }
  if (parsed.protocol !== "http:" || parsed.hostname !== "127.0.0.1" || parsed.username || parsed.password) {
    throw new UsageError(`${flag} must be a loopback address, got ${JSON.stringify(raw)}`);
  }
  return raw;
}

// resolveClaudeBin is the Claude Code executable the SDK drives: CLAUDE_CODE_BIN
// when it is a path, otherwise that name (default `claude`) looked up on PATH.
// The image installs the SDK without its bundled binary, so this is the one
// pinned CLI the agent's plugin install also uses.
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

// K3S_MCP_SERVER is the name the platform registers the K3s MCP under, the
// same as codex's, so its tools are mcp__k3s__<tool> in either type.
export const K3S_MCP_SERVER = "k3s";

// decideTool is AC-E2's permission policy for one tool request the settings
// rules left open. It returns { allow: true } or { allow: false, message }.
// k3s says whether this turn registered the platform's K3s MCP; the K3s MCP
// asks a human before any write itself.
export function decideTool(toolName, plugins = [], { k3s = false } = {}) {
  if (PLATFORM_TOOLS.has(toolName)) {
    return { allow: true };
  }
  if (k3s && toolName.startsWith(`mcp__${K3S_MCP_SERVER}__`)) {
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
      `${toolName} is not part of this session's tool surface.`,
  };
}

// queryOptions are the SDK options of one turn. The system prompt and the
// setting sources are spelled out because the SDK's defaults differ from the
// CLI's: without the preset it sends an empty system prompt.
export function queryOptions({
  model,
  effort,
  permissionMode = DEFAULT_PERMISSION_MODE,
  k3sMcpUrl = null,
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
  if (k3sMcpUrl) {
    // The sidecar injects the token; the CLI sends none of its own.
    options.mcpServers = { [K3S_MCP_SERVER]: { type: "http", url: k3sMcpUrl } };
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

// The quiet spell that lets a finished background task's notification start
// the model's follow-up before the input closes (AC-E2).
export const BACKGROUND_QUIET_MS = 2 * 1000;

// TurnInput is the input a turn hands the SDK (AC-E2): the prompt, and nothing
// the platform writes after it. Closing it ends the CLI process, and a
// background task still running then is killed — its work is lost, and the
// next resumed turn opens with a "stopped, no completion record" notice in the
// same user message as its prompt, which the model reads as an injected
// instruction. So the input stays open after a result while tasks the turn
// started are running: they finish inside the turn, the CLI hands their
// completion to the model, and the input closes after the follow-up result.
// There is no wait of its own (2026-11-14): the agent's turn timeout is the
// only bound, and a turn it cuts short ends as an error.
export class TurnInput {
  #prompt;
  #quietMs;
  #setTimer;
  #clearTimer;
  #resolveClosed;
  #closed;
  #isClosed = false;
  #settled = false;
  #sawBackground = false;
  #running = new Set();
  #quietTimer = null;

  constructor({ prompt, quietMs = BACKGROUND_QUIET_MS, setTimer = setTimeout, clearTimer = clearTimeout }) {
    this.#prompt = prompt;
    this.#quietMs = quietMs;
    this.#setTimer = setTimer;
    this.#clearTimer = clearTimer;
    this.#closed = new Promise((resolve) => {
      this.#resolveClosed = resolve;
    });
  }

  get closed() {
    return this.#isClosed;
  }

  async *messages() {
    yield userMessage(this.#prompt);
    await this.#closed;
  }

  // observe feeds every SDK message of the turn, in order.
  observe(message) {
    if (this.#isClosed || !message || typeof message !== "object") {
      return;
    }
    if (message.type === "system" && message.subtype === "background_tasks_changed") {
      this.#running = new Set((message.tasks || []).map((task) => task.task_id));
      if (this.#running.size > 0) {
        this.#sawBackground = true;
      }
    } else if (message.type === "result") {
      this.#settled = true;
    } else if (message.type !== "stream_event" && !(message.type === "system" && message.subtype === "task_updated")) {
      // Anything else after a result — a task notification, the follow-up
      // turn's init or assistant text — means the conversation moved on.
      this.#settled = false;
    }
    this.#reconsider();
  }

  close() {
    if (this.#isClosed) {
      return;
    }
    this.#isClosed = true;
    if (this.#quietTimer !== null) {
      this.#clearTimer(this.#quietTimer);
    }
    this.#resolveClosed();
  }

  #reconsider() {
    if (this.#quietTimer !== null) {
      this.#clearTimer(this.#quietTimer);
      this.#quietTimer = null;
    }
    if (!this.#settled || this.#running.size > 0) {
      return;
    }
    if (!this.#sawBackground) {
      this.close();
      return;
    }
    this.#quietTimer = this.#setTimer(() => this.close(), this.#quietMs);
  }
}

function userMessage(text) {
  return { type: "user", message: { role: "user", content: text }, parent_tool_use_id: null };
}

// How long a turn waits for a person to answer an AskUserQuestion (AC-E2),
// and how often it asks the agent. The wait stays under the agent's default
// 30-minute turn limit so an unanswered question still leaves the model time
// to go on.
export const DEFAULT_QUESTION_WAIT_MS = 20 * 60 * 1000;
export const QUESTION_POLL_MS = 1000;

// questionWaitFromEnv reads CLAUDE_RUNNER_QUESTION_WAIT_MS, falling back to the
// default for anything that is not a positive integer.
export function questionWaitFromEnv(env) {
  const raw = env.CLAUDE_RUNNER_QUESTION_WAIT_MS;
  if (raw && /^[1-9][0-9]*$/.test(raw)) {
    return Number(raw);
  }
  return DEFAULT_QUESTION_WAIT_MS;
}

function abortableSleep(ms, signal) {
  return new Promise((resolve) => {
    if (signal && signal.aborted) {
      resolve();
      return;
    }
    const timer = setTimeout(done, ms);
    function done() {
      clearTimeout(timer);
      if (signal) signal.removeEventListener("abort", done);
      resolve();
    }
    if (signal) signal.addEventListener("abort", done, { once: true });
  });
}

// askPerson relays one AskUserQuestion call to a person through the agent
// (AC-E2): it posts the questions, polls until the agent reports an answer,
// and returns the permission result the SDK expects — allow with the answers
// added to the tool input, so the tool reports them to the model. A question
// nobody answers in waitMs, one the agent closes (the session is being
// frozen), or one the agent cannot take is denied with a message that says
// what happened — and, like every denial, tells the model nothing more (AC-A5).
export async function askPerson({
  url,
  input,
  signal = null,
  waitMs = DEFAULT_QUESTION_WAIT_MS,
  pollMs = QUESTION_POLL_MS,
  fetchImpl = fetch,
  sleep = abortableSleep,
  now = Date.now,
}) {
  const deny = (message) => ({ behavior: "deny", message });
  const questions = input && Array.isArray(input.questions) ? input.questions : null;
  if (!questions) {
    return deny("The question had no questions to show.");
  }
  let id;
  try {
    const resp = await fetchImpl(`${url}/questions/ask`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        questions: questions.map((q) => ({
          question: q.question,
          header: q.header || "",
          multiSelect: Boolean(q.multiSelect),
          options: (q.options || []).map((o) => ({ label: o.label, description: o.description || "" })),
        })),
      }),
      signal,
    });
    if (resp.status !== 201) {
      const detail = (await resp.text()).trim();
      return deny(`The question could not be shown to a person (${resp.status}: ${detail}).`);
    }
    ({ id } = await resp.json());
  } catch (err) {
    return deny(`The question could not be shown to a person (${err && err.message ? err.message : err}).`);
  }

  const deadline = now() + waitMs;
  const giveUp = async () => {
    try {
      await fetchImpl(`${url}/questions/ask/${encodeURIComponent(id)}`, { method: "DELETE" });
    } catch {
      // The turn ends soon and the agent drops the question with it.
    }
  };
  while (!(signal && signal.aborted)) {
    try {
      const resp = await fetchImpl(`${url}/questions/ask/${encodeURIComponent(id)}`, { signal });
      if (resp.status === 404) {
        return deny("The question is no longer open.");
      }
      if (resp.ok) {
        const status = await resp.json();
        if (status.state === "answered" && status.answers && typeof status.answers === "object") {
          return { behavior: "allow", updatedInput: { ...input, answers: status.answers } };
        }
        if (status.state === "cancelled") {
          return deny(`The question was closed before anyone answered (${status.reason || "no reason given"}).`);
        }
      }
    } catch {
      // A missed poll is retried; the deadline still bounds the wait.
    }
    if (now() >= deadline) {
      await giveUp();
      const minutes = Math.round((waitMs / 60000) * 10) / 10;
      return deny(`Nobody answered within ${minutes} minute(s).`);
    }
    await sleep(Math.min(pollMs, Math.max(0, deadline - now())), signal);
  }
  await giveUp();
  return deny("The turn was interrupted while waiting for an answer.");
}
