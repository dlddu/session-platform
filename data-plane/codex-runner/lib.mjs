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

export class UsageError extends Error {}

// parseArgs reads the agent's argv: --model <m> [--resume] -- <prompt>. The
// prompt is the one argument after "--", so nothing in it is a flag.
export function parseArgs(argv) {
  let model = PLATFORM_DEFAULT_MODEL;
  let resume = false;
  let printCodexBin = false;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--") {
      const rest = argv.slice(i + 1);
      if (rest.length !== 1) {
        throw new UsageError("expected exactly one prompt argument after --");
      }
      return { model, resume, prompt: rest[0], printCodexBin };
    }
    if (arg === "--model") {
      if (i + 1 >= argv.length || argv[i + 1] === "") {
        throw new UsageError("--model needs a value");
      }
      model = argv[++i];
    } else if (arg === "--resume") {
      resume = true;
    } else if (arg === "--print-codex-bin") {
      printCodexBin = true;
    } else {
      throw new UsageError(`unknown argument ${JSON.stringify(arg)}`);
    }
  }
  if (printCodexBin) {
    return { model, resume, prompt: null, printCodexBin };
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

export function threadOptions(model, cwd) {
  const options = {
    sandboxMode: "read-only",
    approvalPolicy: "never",
    workingDirectory: cwd,
    skipGitRepoCheck: true,
    webSearchMode: "disabled",
    networkAccessEnabled: false,
  };
  if (model && model !== PLATFORM_DEFAULT_MODEL) {
    options.model = model;
  }
  return options;
}

export function noticeLine(text) {
  return JSON.stringify({ type: NOTICE_TYPE, text }) + "\n";
}

// classify says what one SDK event means for the run: a forbidden item to
// stop on, the turn's outcome, or nothing.
export function classify(event) {
  if (!event || typeof event !== "object") {
    return {};
  }
  if (event.type === "item.started" || event.type === "item.updated" || event.type === "item.completed") {
    const kind = event.item && FORBIDDEN_ITEM_TYPES.get(event.item.type);
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
