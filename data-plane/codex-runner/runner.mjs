// codex-runner: one Codex turn per invocation, for the codex-chat
// (docs/prd/codex-workload.md, AC-J2/J3/J5/J8) and codex-task
// (docs/prd/codex-task-workload.md, AC-M1/M2/M4) workloads. The agent runs
//
//   codex-runner [--mode chat|task] --model <model|platform-default>
//                [--effort <effort>] [--resume] -- <prompt>
//
// in the session workspace with HOME set to the session HOME, and projects the
// JSON lines written here (data-plane/cmd/agent/codex_stream.go). The runner
// drives `codex app-server` rather than `codex exec`, because exec shuts codex
// down when the main thread's turn ends and takes the sub-agents that turn
// started with it (AC-J8). The main thread's notifications are written as the
// event lines exec printed; the runner's own messages are
// {"type":"session_platform.notice","text":...} lines.
import { spawn } from "node:child_process";
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import readline from "node:readline";
import {
  AgentTracker,
  EXIT_KILLED,
  EXIT_LOGIN_FAILED,
  EXIT_OK,
  EXIT_TURN_FAILED,
  EXIT_USAGE,
  MAX_FOLLOW_UP_TURNS,
  THREAD_FILE,
  TurnTranslator,
  UsageError,
  appServerArgs,
  backgroundWaitFromEnv,
  cleanLoginLine,
  codexHomeFor,
  minimalEnv,
  noticeLine,
  parseArgs,
  parseThreadFile,
  followUpPrompt,
  forbiddenAction,
  resolveCodexBin,
  serverRequestResponse,
  threadFileContent,
  threadParams,
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

// The app server keeps the originator the SDK gave codex exec, so the ChatGPT
// backend sees the same client as before the switch.
const CLIENT_NAME = "codex_sdk_ts";
const CLIENT_VERSION = "1.0.0";
const STOP_GRACE_MS = 30 * 1000;
const EXIT_GRACE_MS = 5 * 1000;
const STDERR_TAIL_BYTES = 4096;

class KillSwitch extends Error {}

// AppServer is a JSON-RPC client of `codex app-server` over stdio: one JSON
// object per line, requests matched by id, every notification handed to
// onNotification in arrival order.
class AppServer {
  #child;
  #nextId = 1;
  #pending = new Map();
  #stderr = "";
  #exited = null;

  constructor(bin, args, env, { onNotification, onServerRequest }) {
    this.#child = spawn(bin, args, { env, stdio: ["pipe", "pipe", "pipe"] });
    this.closed = new Promise((resolve) => {
      this.#child.once("error", (err) => {
        this.#exited = err.message;
        resolve();
      });
      this.#child.once("exit", (code, signal) => {
        this.#exited = signal ? `signal ${signal}` : `code ${code}`;
        resolve();
      });
    });
    this.closed.then(() => {
      const reason = new Error(`codex app-server exited (${this.#exited})${this.#stderr ? `: ${this.#stderr.trim()}` : ""}`);
      for (const { reject } of this.#pending.values()) {
        reject(reason);
      }
      this.#pending.clear();
    });
    this.#child.stdin.on("error", () => {});
    this.#child.stderr.on("data", (chunk) => {
      this.#stderr = (this.#stderr + chunk.toString("utf8")).slice(-STDERR_TAIL_BYTES);
    });
    const rl = readline.createInterface({ input: this.#child.stdout, crlfDelay: Infinity });
    rl.on("line", (line) => {
      let message;
      try {
        message = JSON.parse(line);
      } catch {
        return;
      }
      if (!message || typeof message !== "object") {
        return;
      }
      if (message.method && message.id !== undefined) {
        const answer = onServerRequest(message.method, message.params);
        this.#send({ id: message.id, ...answer });
      } else if (message.method) {
        onNotification(message.method, message.params || {});
      } else if (this.#pending.has(message.id)) {
        const { resolve, reject } = this.#pending.get(message.id);
        this.#pending.delete(message.id);
        if (message.error) {
          reject(new Error(message.error.message || JSON.stringify(message.error)));
        } else {
          resolve(message.result);
        }
      }
    });
  }

  get exitReason() {
    return this.#exited;
  }

  #send(message) {
    if (this.#exited === null) {
      this.#child.stdin.write(JSON.stringify(message) + "\n");
    }
  }

  request(method, params) {
    if (this.#exited !== null) {
      return Promise.reject(new Error(`codex app-server exited (${this.#exited})`));
    }
    const id = this.#nextId++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      this.#send({ id, method, params });
    });
  }

  notify(method, params) {
    this.#send(params === undefined ? { method } : { method, params });
  }

  kill() {
    if (this.#exited === null) {
      this.#child.kill("SIGKILL");
    }
    return this.closed;
  }

  // close ends the connection, which ends the app server and every
  // sub-agent still in it.
  async close() {
    this.#child.stdin.end();
    const timer = setTimeout(() => this.kill(), EXIT_GRACE_MS);
    await this.closed;
    clearTimeout(timer);
  }
}

// Session drives one invocation: the main thread's turn, then, while
// sub-agents it started are still running, a wait and a follow-up turn that
// hands their results to the main thread (AC-J8).
class Session {
  #server;
  #mode;
  #threadId = null;
  #translator = null;
  #turnDone = null;
  #agents = null;
  #agentChanged = () => {};
  #killed = null;
  #queue = Promise.resolve();

  constructor(bin, env, args) {
    this.#mode = args.mode;
    this.#server = new AppServer(bin, appServerArgs({ mode: args.mode, effort: args.effort }), env, {
      onNotification: (method, params) => {
        this.#queue = this.#queue.then(() => this.#onNotification(method, params));
      },
      onServerRequest: (method, params) => serverRequestResponse(method, params),
    });
  }

  async #onNotification(method, params) {
    if (this.#killed) {
      return;
    }
    if ((method === "item/started" || method === "item/completed") && params.item) {
      const verdict = forbiddenAction(params.item, this.#mode);
      if (verdict) {
        this.#killed = verdict;
        await notice(`Codex tried to ${verdict.action} (${verdict.forbidden}), which this session does not allow; the turn was stopped.`);
        await this.#server.kill();
        this.#turnDone?.reject(new KillSwitch());
        this.#agentChanged();
        return;
      }
    }
    if (this.#agents) {
      for (const text of this.#agents.observe(method, params)) {
        await notice(text);
      }
      this.#agentChanged();
    }
    if (this.#translator && params.threadId === this.#threadId) {
      const { events, outcome } = this.#translator.translate(method, params);
      for (const event of events) {
        await emit(JSON.stringify(event) + "\n");
      }
      if (outcome) {
        this.#agents?.setMainBusy(false);
        this.#turnDone?.resolve(outcome);
      }
    }
  }

  async open(model, resumeId, codexHome) {
    await this.#server.request("initialize", {
      clientInfo: { name: CLIENT_NAME, title: null, version: CLIENT_VERSION },
      capabilities: null,
    });
    this.#server.notify("initialized");
    const params = threadParams(model, process.cwd(), { mode: this.#mode });
    const response = resumeId
      ? await this.#server.request("thread/resume", { threadId: resumeId, ...params, excludeTurns: true })
      : await this.#server.request("thread/start", params);
    this.#threadId = response.thread.id;
    writeThreadId(codexHome, this.#threadId);
    this.#agents = new AgentTracker(this.#threadId);
    await emit(JSON.stringify({ type: "thread.started", thread_id: this.#threadId }) + "\n");
  }

  async #turn(text) {
    this.#translator = new TurnTranslator(this.#threadId);
    this.#agents.setMainBusy(true);
    const done = new Promise((resolve, reject) => {
      this.#turnDone = { resolve, reject };
    });
    const exited = this.#server.closed.then(() => {
      throw new Error(`codex app-server exited (${this.#server.exitReason})`);
    });
    const response = await Promise.race([
      this.#server.request("turn/start", {
        threadId: this.#threadId,
        input: [{ type: "text", text, text_elements: [] }],
      }),
      exited,
    ]);
    if (response && response.turn && this.#translator.turnId === null) {
      this.#translator.turnId = response.turn.id;
    }
    return Promise.race([done, exited]);
  }

  // #waitForAgents resolves true once no sub-agent is busy, false at the
  // deadline.
  #waitForAgents(ms) {
    return new Promise((resolve) => {
      const check = () => {
        if (this.#killed || this.#agents.busy().length === 0) {
          clearTimeout(timer);
          this.#agentChanged = () => {};
          resolve(true);
        }
      };
      const timer = setTimeout(() => {
        this.#agentChanged = () => {};
        resolve(false);
      }, ms);
      this.#agentChanged = check;
      check();
    });
  }

  async #stopAgents(agents) {
    for (const agent of agents) {
      if (agent.turnId) {
        await this.#server.request("turn/interrupt", { threadId: agent.id, turnId: agent.turnId }).catch(() => {});
      }
    }
    await this.#waitForAgents(STOP_GRACE_MS);
    for (const agent of this.#agents.busy()) {
      agent.busy = false;
      agent.status = "interrupted";
      agent.unreported = true;
    }
  }

  async run(prompt, waitMs) {
    let outcome = await this.#turn(prompt);
    let followUps = 0;
    while (outcome === "completed" && this.#agents.busy().length > 0) {
      if (followUps === MAX_FOLLOW_UP_TURNS) {
        const left = this.#agents.busy();
        await notice(`Stopping ${left.length} background agent(s) after ${MAX_FOLLOW_UP_TURNS} follow-up turns: ${left.map((a) => a.name).join(", ")}.`);
        await this.#stopAgents(left);
        break;
      }
      const busy = this.#agents.busy();
      await notice(`Waiting for ${busy.length} background agent(s) before this turn ends: ${busy.map((a) => a.name).join(", ")}.`);
      let stoppedAfterMs = null;
      if (!(await this.#waitForAgents(waitMs))) {
        const left = this.#agents.busy();
        await notice(`Background agent(s) still running after ${Math.round(waitMs / 60000)} minute(s), which is as long as a turn waits; stopping ${left.map((a) => a.name).join(", ")}.`);
        await this.#stopAgents(left);
        stoppedAfterMs = waitMs;
      }
      if (this.#killed) {
        break;
      }
      const ended = this.#agents.takeUnreported();
      if (ended.length === 0) {
        break;
      }
      followUps++;
      await notice("Background agents ended; the main agent continues with their results.");
      outcome = await this.#turn(followUpPrompt(ended, { stoppedAfterMs }));
    }
    return outcome;
  }

  get killed() {
    return this.#killed !== null;
  }

  close() {
    return this.#server.close();
  }
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

  const session = new Session(bin, env, args);
  try {
    await session.open(args.model, args.resume ? readThreadId(codexHome) : null, codexHome);
    const outcome = await session.run(args.prompt, backgroundWaitFromEnv(process.env));
    if (session.killed) {
      return EXIT_KILLED;
    }
    return outcome === "completed" ? EXIT_OK : EXIT_TURN_FAILED;
  } catch (err) {
    if (session.killed || err instanceof KillSwitch) {
      return EXIT_KILLED;
    }
    await emit(JSON.stringify({ type: "error", message: err && err.message ? err.message : String(err) }) + "\n");
    return EXIT_TURN_FAILED;
  } finally {
    await session.close();
  }
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
