// codex-runner: one Codex SDK turn per invocation, for the codex workload
// (docs/prd/codex-workload.md, AC-J2/J3/J5). The agent runs
//
//   codex-runner --model <model|platform-default> [--resume] -- <prompt>
//
// in the session workspace with HOME set to the session HOME, and projects the
// JSON lines written here (data-plane/cmd/agent/codex_stream.go). Every SDK
// event is one line; the runner's own messages are
// {"type":"session_platform.notice","text":...} lines.
import { spawn } from "node:child_process";
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import readline from "node:readline";
import { Codex } from "@openai/codex-sdk";
import {
  EXIT_KILLED,
  EXIT_LOGIN_FAILED,
  EXIT_OK,
  EXIT_TURN_FAILED,
  EXIT_USAGE,
  THREAD_FILE,
  UsageError,
  classify,
  cleanLoginLine,
  codexHomeFor,
  minimalEnv,
  noticeLine,
  parseArgs,
  parseThreadFile,
  resolveCodexBin,
  threadFileContent,
  threadOptions,
} from "./lib.mjs";

// A device code is valid for about fifteen minutes; the wait outlasts it so
// codex, not this timer, reports an expired code.
const LOGIN_TIMEOUT_MS = 20 * 60 * 1000;

function emit(line) {
  return new Promise((resolve) => {
    if (!process.stdout.write(line)) {
      process.stdout.once("drain", resolve);
    } else {
      resolve();
    }
  });
}

const notice = (text) => emit(noticeLine(text));

function run(bin, args, env, { onLine, timeoutMs } = {}) {
  return new Promise((resolve) => {
    const child = spawn(bin, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    const lines = [];
    const pending = [];
    for (const stream of [child.stdout, child.stderr]) {
      const rl = readline.createInterface({ input: stream, crlfDelay: Infinity });
      pending.push(
        (async () => {
          for await (const line of rl) {
            lines.push(line);
            if (onLine) {
              await onLine(line);
            }
          }
        })(),
      );
    }
    const timer = timeoutMs ? setTimeout(() => child.kill("SIGKILL"), timeoutMs) : null;
    child.once("error", (err) => {
      if (timer) clearTimeout(timer);
      resolve({ code: null, error: err, lines });
    });
    child.once("close", async (code) => {
      if (timer) clearTimeout(timer);
      await Promise.allSettled(pending);
      resolve({ code, lines });
    });
  });
}

// ensureLogin signs the session in to ChatGPT with a device code when it is
// not signed in yet (AC-J3). The login is written to $CODEX_HOME/auth.json,
// which the session archive keeps, so this happens once per session rather
// than once per pod.
async function ensureLogin(bin, env) {
  const status = await run(bin, ["login", "status"], env);
  if (status.code === 0) {
    return true;
  }
  await notice("This Codex session is not signed in to ChatGPT yet.");
  await notice("Open the link below in a browser, sign in with your ChatGPT account and enter the one-time code. The prompt runs once sign-in completes.");
  const login = await run(bin, ["login", "--device-auth"], env, {
    timeoutMs: LOGIN_TIMEOUT_MS,
    onLine: async (line) => {
      const text = cleanLoginLine(line);
      if (text !== null) {
        await notice(text);
      }
    },
  });
  if (login.code !== 0) {
    await notice(`ChatGPT sign-in did not complete${login.error ? ` (${login.error.message})` : ""}. Send the prompt again to retry.`);
    return false;
  }
  await notice("Signed in to ChatGPT.");
  return true;
}

function readThreadId(codexHome) {
  try {
    return parseThreadFile(readFileSync(path.join(codexHome, THREAD_FILE), "utf8"));
  } catch {
    return null;
  }
}

function writeThreadId(codexHome, threadId) {
  mkdirSync(codexHome, { recursive: true, mode: 0o700 });
  const target = path.join(codexHome, THREAD_FILE);
  const tmp = `${target}.${process.pid}.tmp`;
  writeFileSync(tmp, threadFileContent(threadId), { mode: 0o600 });
  renameSync(tmp, target);
}

async function main(argv) {
  let args;
  try {
    args = parseArgs(argv);
  } catch (err) {
    if (err instanceof UsageError) {
      process.stderr.write(`codex-runner: ${err.message}\n`);
      return EXIT_USAGE;
    }
    throw err;
  }
  const bin = resolveCodexBin(process.env);
  if (args.printCodexBin) {
    await emit(bin + "\n");
    return EXIT_OK;
  }
  const codexHome = codexHomeFor(process.env);
  const env = minimalEnv(process.env, codexHome);

  if (!(await ensureLogin(bin, env))) {
    return EXIT_LOGIN_FAILED;
  }

  const codex = new Codex({ codexPathOverride: bin, env });
  const options = threadOptions(args.model, process.cwd());
  const previous = args.resume ? readThreadId(codexHome) : null;
  const thread = previous ? codex.resumeThread(previous, options) : codex.startThread(options);
  return runTurn(thread, args.prompt, codexHome);
}

// runTurn streams one turn, recording the thread id as soon as codex reports
// it and stopping at the first forbidden item (AC-J5).
async function runTurn(thread, prompt, codexHome) {
  const controller = new AbortController();
  let outcome = null;
  let killed = false;
  try {
    const { events } = await thread.runStreamed(prompt, { signal: controller.signal });
    for await (const event of events) {
      await emit(JSON.stringify(event) + "\n");
      const verdict = classify(event);
      if (verdict.threadId) {
        writeThreadId(codexHome, verdict.threadId);
      }
      if (verdict.forbidden) {
        killed = true;
        await notice(`Codex tried to ${verdict.action} (${verdict.forbidden}), which this session does not allow; the turn was stopped.`);
        controller.abort();
        break;
      }
      if (verdict.outcome === "failed" || (verdict.outcome && outcome !== "failed")) {
        outcome = verdict.outcome;
      }
    }
  } catch (err) {
    if (!killed) {
      await emit(JSON.stringify({ type: "error", message: err && err.message ? err.message : String(err) }) + "\n");
      return EXIT_TURN_FAILED;
    }
  }
  if (killed) {
    return EXIT_KILLED;
  }
  return outcome === "completed" ? EXIT_OK : EXIT_TURN_FAILED;
}

main(process.argv.slice(2)).then(
  (code) => {
    process.exitCode = code;
  },
  async (err) => {
    process.stderr.write(`codex-runner: ${err && err.stack ? err.stack : err}\n`);
    process.exitCode = EXIT_TURN_FAILED;
  },
);
