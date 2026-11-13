// Pure helpers of codex-runner (docs/prd/codex-workload.md, AC-J2/J3/J5/J8),
// kept apart from runner.mjs so `node --test` can exercise them without a
// codex binary or a ChatGPT account.
import { createRequire } from "node:module";
import { statSync } from "node:fs";
import path from "node:path";

export const PLATFORM_DEFAULT_MODEL = "platform-default";
export const NOTICE_TYPE = "session_platform.notice";
export const THREAD_FILE = "session-platform-thread.json";

// Exit codes the agent reads only as success or failure, but which keep a
// failed sign-in (2) and a stopped turn (3) apart in the logs.
export const EXIT_OK = 0;
export const EXIT_TURN_FAILED = 1;
export const EXIT_LOGIN_FAILED = 2;
export const EXIT_KILLED = 3;
export const EXIT_USAGE = 64;

// Item types that mean codex did something outside the AC-J5 surface. The
// managed config removes their tools from what the model sees where codex
// allows it, and the read-only sandbox stops the rest; seeing one anyway
// means the configuration did not hold, so the turn is stopped (AC-J5).
export const FORBIDDEN_ITEM_TYPES = new Map([
  ["command_execution", "run a command"],
  ["file_change", "change a file"],
  ["web_search", "search the web"],
]);

// The two session kinds the runner serves. chat is codex-chat's narrowed
// surface (AC-J5); task is codex-task's claude-code-level surface (AC-M1),
// where commands and file changes are the point and nothing is stopped.
export const MODE_CHAT = "chat";
export const MODE_TASK = "task";
export const MODES = new Set([MODE_CHAT, MODE_TASK]);

// REASONING_EFFORTS is AC-M4's list. The SDK type admits more values, but the
// platform offers only the ones its /config advertises for codex-task.
export const REASONING_EFFORTS = new Set(["minimal", "low", "medium", "high", "xhigh"]);

export class UsageError extends Error {}

// parseArgs reads the agent's argv:
// [--mode chat|task] --model <m> [--effort <e>] [--resume] -- <prompt>. The
// prompt is the one argument after "--", so nothing in it is a flag.
export function parseArgs(argv) {
  let model = PLATFORM_DEFAULT_MODEL;
  let mode = MODE_CHAT;
  let effort = null;
  let resume = false;
  let printCodexBin = false;
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
      return { model, mode, effort, resume, prompt: rest[0], printCodexBin };
    }
    if (arg === "--model") {
      model = value(i++, "--model");
    } else if (arg === "--mode") {
      mode = value(i++, "--mode");
      if (!MODES.has(mode)) {
        throw new UsageError(`unknown mode ${JSON.stringify(mode)}`);
      }
    } else if (arg === "--effort") {
      effort = value(i++, "--effort");
      if (!REASONING_EFFORTS.has(effort)) {
        throw new UsageError(`unknown reasoning effort ${JSON.stringify(effort)}`);
      }
    } else if (arg === "--resume") {
      resume = true;
    } else if (arg === "--print-codex-bin") {
      printCodexBin = true;
    } else {
      throw new UsageError(`unknown argument ${JSON.stringify(arg)}`);
    }
  }
  if (printCodexBin) {
    return { model, mode, effort, resume, prompt: null, printCodexBin };
  }
  throw new UsageError("missing -- <prompt>");
}

// minimalEnv is everything the codex child is given. The agent's own
// environment is not passed on, so no variable a future pod spec adds can
// reach codex or the tools it starts (AC-J3).
export function minimalEnv(env, codexHome) {
  const out = {};
  for (const name of ["HOME", "PATH", "LANG", "TZ", "SSL_CERT_FILE"]) {
    if (env[name]) {
      out[name] = env[name];
    }
  }
  out.CODEX_HOME = codexHome;
  if (!out.PATH) {
    out.PATH = "/usr/local/bin:/usr/bin:/bin";
  }
  return out;
}

export function codexHomeFor(env) {
  if (env.CODEX_HOME) {
    return env.CODEX_HOME;
  }
  if (!env.HOME) {
    throw new UsageError("HOME is not set");
  }
  return path.join(env.HOME, ".codex");
}

// appServerArgs starts codex's app server with the settings the SDK used to
// pass to `codex exec` as --config: web search off, the never-ask approval
// policy, sandbox network only in task mode (AC-J2, AC-M1), and the effort.
export function appServerArgs({ mode = MODE_CHAT, effort = null } = {}) {
  const args = [
    "app-server",
    "--config", 'web_search="disabled"',
    "--config", 'approval_policy="never"',
    "--config", `sandbox_workspace_write.network_access=${mode === MODE_TASK}`,
  ];
  if (effort) {
    args.push("--config", `model_reasoning_effort="${effort}"`);
  }
  return args;
}

// threadParams fixes the sandbox per mode for thread/start and thread/resume.
// chat is read-only (AC-J2); task runs like claude-code, with the session pod
// as the execution boundary (AC-M1).
export function threadParams(model, cwd, { mode = MODE_CHAT } = {}) {
  const params = {
    cwd,
    sandbox: mode === MODE_TASK ? "danger-full-access" : "read-only",
    approvalPolicy: "never",
  };
  if (model && model !== PLATFORM_DEFAULT_MODEL) {
    params.model = model;
  }
  return params;
}

export function noticeLine(text) {
  return JSON.stringify({ type: NOTICE_TYPE, text }) + "\n";
}

// ANSI escapes and the codex warning about its helper links are terminal
// noise; neither belongs in the session output.
const ANSI = /\u001b\[[0-9;?]*[ -/]*[@-~]|\u001b\][^\u0007]*\u0007/g;
const NOISE = [/^WARNING: proceeding, even though we could not create PATH aliases/];

export function cleanLoginLine(line) {
  const text = line.replace(ANSI, "").replace(/[\u0000-\u0008\u000b-\u001f\u007f]/g, "").trimEnd();
  if (!text.trim() || NOISE.some((re) => re.test(text))) {
    return null;
  }
  return text;
}

export function parseThreadFile(content) {
  try {
    const doc = JSON.parse(content);
    if (doc && doc.version === 1 && typeof doc.threadId === "string" && doc.threadId) {
      return doc.threadId;
    }
  } catch {
    // An unreadable record is no record: the next run starts a new thread.
  }
  return null;
}

export function threadFileContent(threadId) {
  return JSON.stringify({ version: 1, threadId }) + "\n";
}

const NATIVE_TARGETS = {
  "linux-x64": ["x86_64-unknown-linux-musl", "@openai/codex-linux-x64"],
  "linux-arm64": ["aarch64-unknown-linux-musl", "@openai/codex-linux-arm64"],
  "darwin-x64": ["x86_64-apple-darwin", "@openai/codex-darwin-x64"],
  "darwin-arm64": ["aarch64-apple-darwin", "@openai/codex-darwin-arm64"],
};

// resolveCodexBin is CODEX_BIN, or the native binary of the installed
// @openai/codex package, found where the package's own launcher looks.
export function resolveCodexBin(env, fromUrl = import.meta.url) {
  if (env.CODEX_BIN) {
    return env.CODEX_BIN;
  }
  const target = NATIVE_TARGETS[`${process.platform}-${process.arch}`];
  if (!target) {
    throw new Error(`no codex binary for ${process.platform}-${process.arch}`);
  }
  const [triple, pkg] = target;
  const require = createRequire(fromUrl);
  const codexRequire = createRequire(require.resolve("@openai/codex/package.json"));
  const root = path.join(path.dirname(codexRequire.resolve(`${pkg}/package.json`)), "vendor", triple);
  for (const candidate of [path.join(root, "bin", "codex"), path.join(root, "codex", "codex")]) {
    try {
      if (statSync(candidate).isFile()) {
        return candidate;
      }
    } catch {
      // Try the other layout.
    }
  }
  throw new Error(`the installed @openai/codex has no binary for ${triple}`);
}

// The app server speaks camelCase items; the agent's projection
// (codex_stream.go) reads the snake_case events `codex exec`
// printed, so the runner translates the main thread's notifications into
// those, the way codex's own exec event processor does.
const STATUS = { inProgress: "in_progress", completed: "completed", failed: "failed", declined: "declined", interrupted: "interrupted" };
const COLLAB_TOOL = { spawnAgent: "spawn_agent", sendInput: "send_input", resumeAgent: "wait", wait: "wait", closeAgent: "close_agent" };

function snakeStatus(status) {
  return STATUS[status] || status;
}

// execItem is one app-server item as an exec item, or null for kinds exec
// never printed (user messages, plans, sub-agent activity, ...).
export function execItem(item) {
  if (!item || typeof item !== "object") {
    return null;
  }
  const id = item.id;
  switch (item.type) {
    case "agentMessage":
      return { id, type: "agent_message", text: item.text ?? "" };
    case "reasoning": {
      const text = Array.isArray(item.summary) ? item.summary.join("\n") : "";
      return text.trim() ? { id, type: "reasoning", text } : null;
    }
    case "commandExecution":
      return {
        id,
        type: "command_execution",
        command: item.command,
        aggregated_output: item.aggregatedOutput ?? "",
        exit_code: item.exitCode ?? null,
        status: snakeStatus(item.status),
      };
    case "fileChange":
      return {
        id,
        type: "file_change",
        changes: (item.changes || []).map((c) => ({ path: c.path, kind: c.kind && typeof c.kind === "object" ? c.kind.type : c.kind })),
        status: snakeStatus(item.status === "declined" ? "failed" : item.status),
      };
    case "mcpToolCall":
      return {
        id,
        type: "mcp_tool_call",
        server: item.server,
        tool: item.tool,
        status: snakeStatus(item.status),
        arguments: item.arguments ?? null,
        result: item.result ? { content: item.result.content, structured_content: item.result.structuredContent ?? null } : null,
        error: item.error ? { message: item.error.message } : null,
      };
    case "webSearch":
      return { id, type: "web_search", query: item.query ?? "" };
    case "collabAgentToolCall": {
      const tool = COLLAB_TOOL[item.tool];
      if (!tool || item.status === "interrupted") {
        return null;
      }
      const states = {};
      for (const [threadId, state] of Object.entries(item.agentsStates || {})) {
        states[threadId] = { status: state && state.status, message: (state && state.message) ?? null };
      }
      return {
        id,
        type: "collab_tool_call",
        tool,
        sender_thread_id: item.senderThreadId,
        receiver_thread_ids: item.receiverThreadIds || [],
        prompt: item.prompt ?? null,
        agents_states: states,
        status: snakeStatus(item.status),
      };
    }
    default:
      return null;
  }
}

// forbiddenAction is the kill switch's verdict on one item of any thread,
// sub-agents included: `codex exec` only showed the main thread, so a
// sub-agent's command never reached the old kill switch (AC-J5).
export function forbiddenAction(item, mode = MODE_CHAT) {
  const mapped = execItem(item);
  if (mode !== MODE_CHAT || !mapped) {
    return null;
  }
  const action = FORBIDDEN_ITEM_TYPES.get(mapped.type);
  return action ? { forbidden: mapped.type, action } : null;
}

function turnErrorMessage(error, fallback) {
  if (!error || typeof error !== "object" || !error.message) {
    return fallback;
  }
  return error.additionalDetails ? `${error.message} (${error.additionalDetails})` : error.message;
}

// TurnTranslator turns the main thread's notifications for one turn into exec
// events. A started item the turn ends without completing is completed from
// the turn's own item list, as exec did, so a tool call is never left open.
export class TurnTranslator {
  #open = new Set();
  #errorSeen = false;

  // turnId may be null until turn/start answers; the first notification of
  // the thread that names a turn fixes it, since notifications can overtake
  // the response.
  constructor(threadId, turnId = null) {
    this.threadId = threadId;
    this.turnId = turnId;
  }

  #isTurn(turnId) {
    if (this.turnId === null && typeof turnId === "string" && turnId) {
      this.turnId = turnId;
    }
    return turnId === this.turnId;
  }

  get errorSeen() {
    return this.#errorSeen;
  }

  translate(method, params) {
    const events = [];
    if (!params || params.threadId !== this.threadId) {
      return { events };
    }
    const sameTurn = method !== "turn/started" && method !== "turn/completed" && this.#isTurn(params.turnId);
    switch (method) {
      case "turn/started":
        if (params.turn && this.#isTurn(params.turn.id)) {
          events.push({ type: "turn.started" });
        }
        break;
      case "item/started": {
        if (!sameTurn || !params.item || params.item.type === "agentMessage" || params.item.type === "reasoning") {
          break;
        }
        const item = execItem(params.item);
        if (item) {
          this.#open.add(params.item.id);
          events.push({ type: "item.started", item });
        }
        break;
      }
      case "item/completed": {
        if (!sameTurn) {
          break;
        }
        const item = execItem(params.item);
        if (item) {
          this.#open.delete(params.item.id);
          events.push({ type: "item.completed", item });
        }
        break;
      }
      case "error":
        if (sameTurn && params.error) {
          if (!params.willRetry) {
            this.#errorSeen = true;
          }
          events.push({ type: "error", message: turnErrorMessage(params.error, "codex reported an error") });
        }
        break;
      case "turn/completed": {
        const turn = params.turn;
        if (!turn || !this.#isTurn(turn.id)) {
          break;
        }
        for (const raw of turn.items || []) {
          if (this.#open.has(raw.id)) {
            this.#open.delete(raw.id);
            const item = execItem(raw);
            if (item) {
              events.push({ type: "item.completed", item });
            }
          }
        }
        if (turn.status === "completed") {
          events.push({ type: "turn.completed" });
          return { events, outcome: this.#errorSeen ? "failed" : "completed" };
        }
        const fallback = turn.status === "interrupted" ? "the turn was interrupted" : "turn failed";
        events.push({ type: "turn.failed", error: { message: turnErrorMessage(turn.error, fallback) } });
        return { events, outcome: "failed" };
      }
      default:
        break;
    }
    return { events };
  }
}

// How long a turn waits, after the main thread's answer, for sub-agents it
// left running before it stops them — claude-runner's background wait
// (AC-J8, AC-E2).
export const DEFAULT_BACKGROUND_WAIT_MS = 10 * 60 * 1000;
export const MAX_FOLLOW_UP_TURNS = 5;
export const MAX_AGENT_MESSAGE_CHARS = 8 * 1024;

export function backgroundWaitFromEnv(env) {
  const raw = env.CODEX_RUNNER_BACKGROUND_WAIT_MS;
  const value = raw === undefined || raw === "" ? NaN : Number(raw);
  if (Number.isInteger(value) && value >= 0) {
    return value;
  }
  return DEFAULT_BACKGROUND_WAIT_MS;
}

// AgentTracker follows the sub-agent threads the app server attaches while
// the runner is connected. An agent is busy from its thread's start (before
// its first turn) until its turn ends; an agent whose turn ends while the
// main thread has no turn running is unreported until a follow-up turn tells
// the main thread about it — a turn in progress learns of its agents itself.
export class AgentTracker {
  #agents = new Map();
  #mainBusy = true;

  constructor(mainThreadId) {
    this.mainThreadId = mainThreadId;
  }

  setMainBusy(busy) {
    this.#mainBusy = busy;
  }

  observe(method, params) {
    if (!params) {
      return null;
    }
    if (method === "thread/started") {
      const thread = params.thread;
      if (!thread || !thread.id || thread.id === this.mainThreadId || this.#agents.has(thread.id)) {
        return null;
      }
      const name = thread.agentNickname || thread.id;
      this.#agents.set(thread.id, { id: thread.id, name, busy: true, turnId: null, status: null, message: null, error: null, unreported: false });
      return `Background agent ${name} started.`;
    }
    const agent = this.#agents.get(params.threadId);
    if (!agent) {
      return null;
    }
    switch (method) {
      case "turn/started":
        agent.busy = true;
        agent.turnId = params.turn ? params.turn.id : null;
        agent.status = null;
        return null;
      case "item/completed":
        if (params.item && params.item.type === "agentMessage" && params.item.text) {
          agent.message = params.item.text;
        }
        return null;
      case "item/started":
        if (params.item && params.item.type === "mcpToolCall") {
          return `· ${agent.name}: ${params.item.server}.${params.item.tool}`;
        }
        return null;
      case "turn/completed": {
        const turn = params.turn || {};
        agent.busy = false;
        agent.turnId = null;
        agent.status = turn.status || "completed";
        agent.error = turn.error ? turnErrorMessage(turn.error, null) : null;
        if (!this.#mainBusy) {
          agent.unreported = true;
        }
        if (agent.status === "completed") {
          return `Background agent ${agent.name} finished.`;
        }
        if (agent.status === "interrupted") {
          return `Background agent ${agent.name} was stopped.`;
        }
        return `Background agent ${agent.name} failed${agent.error ? `: ${agent.error}` : "."}`;
      }
      case "thread/closed":
        agent.busy = false;
        return null;
      default:
        return null;
    }
  }

  busy() {
    return [...this.#agents.values()].filter((a) => a.busy);
  }

  takeUnreported() {
    const out = [...this.#agents.values()].filter((a) => a.unreported);
    for (const agent of out) {
      agent.unreported = false;
    }
    return out;
  }
}

function clip(text, max) {
  return text.length <= max ? text : `${text.slice(0, max)}\n[… cut at ${max} characters]`;
}

export function followUpPrompt(agents, { stoppedAfterMs = null } = {}) {
  const lines = ["[session-platform] Background agents you started have ended since your last answer:"];
  for (const agent of agents) {
    const label = agent.name === agent.id ? agent.id : `${agent.name} (${agent.id})`;
    if (agent.status === "completed") {
      lines.push(`- ${label}: completed.${agent.message ? ` Its final message:\n${clip(agent.message, MAX_AGENT_MESSAGE_CHARS)}` : ""}`);
    } else if (agent.status === "interrupted") {
      const minutes = stoppedAfterMs === null ? null : Math.round(stoppedAfterMs / 60000);
      lines.push(`- ${label}: stopped by the platform${minutes === null ? "" : ` after waiting ${minutes} minute(s)`}.${agent.message ? ` Its last message:\n${clip(agent.message, MAX_AGENT_MESSAGE_CHARS)}` : ""}`);
    } else {
      lines.push(`- ${label}: failed${agent.error ? `: ${agent.error}` : "."}`);
    }
  }
  lines.push("Tell the user what they found. Do not wait for these agents again.");
  return lines.join("\n");
}

// serverRequestResponse is the runner's answer to a request the app server
// sends it, mirroring `codex exec`: MCP elicitations are cancelled as
// non-interactive and everything that would need a person is refused.
export function serverRequestResponse(method, params) {
  if (method === "mcpServer/elicitation/request") {
    const meta = params && params._meta;
    const toolApproval = params && params.mode === "form" && meta && typeof meta === "object" && meta.codex_approval_kind === "mcp_tool_call";
    return {
      result: {
        action: "cancel",
        content: null,
        _meta: toolApproval ? { codex_approval_cancellation_reason: "non_interactive" } : null,
      },
    };
  }
  return { error: { code: -32000, message: `${method} is not supported by codex-runner` } };
}
