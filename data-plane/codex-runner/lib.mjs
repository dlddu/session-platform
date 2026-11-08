// Pure helpers of codex-runner (docs/prd/codex-workload.md, AC-J2/J3/J5),
// kept apart from runner.mjs so `node --test` can exercise them without the
// SDK, a codex binary or a ChatGPT account.
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

// threadOptions fixes the sandbox per mode. chat is read-only with no
// network (AC-J2); task runs like claude-code, with the session pod as the
// execution boundary (AC-M1). Only task searches the web, live (AC-M1).
export function threadOptions(model, cwd, { mode = MODE_CHAT, effort = null } = {}) {
  const task = mode === MODE_TASK;
  const options = {
    sandboxMode: task ? "danger-full-access" : "read-only",
    approvalPolicy: "never",
    workingDirectory: cwd,
    skipGitRepoCheck: true,
    webSearchMode: task ? "live" : "disabled",
    networkAccessEnabled: task,
  };
  if (model && model !== PLATFORM_DEFAULT_MODEL) {
    options.model = model;
  }
  if (effort) {
    options.modelReasoningEffort = effort;
  }
  return options;
}

export function noticeLine(text) {
  return JSON.stringify({ type: NOTICE_TYPE, text }) + "\n";
}

// classify says what one SDK event means for the run: a forbidden item to
// stop on, the turn's outcome, or nothing. Only chat has forbidden items; the
// kill switch is AC-J5's and codex-task does not run it (AC-M1).
export function classify(event, mode = MODE_CHAT) {
  if (!event || typeof event !== "object") {
    return {};
  }
  if (event.type === "item.started" || event.type === "item.updated" || event.type === "item.completed") {
    const kind = mode === MODE_CHAT && event.item && FORBIDDEN_ITEM_TYPES.get(event.item.type);
    if (kind) {
      return { forbidden: event.item.type, action: kind };
    }
    return {};
  }
  if (event.type === "thread.started" && typeof event.thread_id === "string" && event.thread_id) {
    return { threadId: event.thread_id };
  }
  if (event.type === "turn.completed") {
    return { outcome: "completed" };
  }
  if (event.type === "turn.failed" || event.type === "error") {
    return { outcome: "failed" };
  }
  return {};
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
// @openai/codex package, found the way the SDK finds it. The runner needs the
// path itself because it runs `codex login` outside the SDK.
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
