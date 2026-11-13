// claude-runner: one Claude Agent SDK turn per invocation, for the claude-code
// workload (docs/prd/claude-code-workload.md, AC-E2/E4). The agent runs
//
//   claude-runner [--model <model|platform-default>] [--effort <effort>]
//                 [--permission-mode <auto|acceptEdits>] [--k3s-mcp-url <url>]
//                 [--resume] -- <prompt>
//
// in the session workspace with HOME set to the session HOME, and projects the
// JSON lines written here (data-plane/cmd/agent/claude_stream.go). Every SDK
// message is one line — the same shapes the CLI's stream-json printed — and
// the runner's own messages are {"type":"session_platform.notice","text":...}
// lines. The turn's input stays open while background tasks it started are
// running (TurnInput), so their work finishes inside the turn;
// CLAUDE_RUNNER_BACKGROUND_WAIT_MS bounds that wait.
import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { getSessionInfo, query } from "@anthropic-ai/claude-agent-sdk";
import {
  EXIT_OK,
  EXIT_TURN_FAILED,
  EXIT_USAGE,
  SESSION_FILE,
  SETTINGS_FILE,
  TurnInput,
  UsageError,
  backgroundWaitFromEnv,
  classify,
  decideTool,
  noticeLine,
  parseArgs,
  parseSessionFile,
  pluginsFromSettings,
  queryOptions,
  resolveClaudeBin,
  resumePlan,
  sameCliVersion,
  sessionFileContent,
} from "./lib.mjs";

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

function readSessionId(file) {
  try {
    return parseSessionFile(readFileSync(file, "utf8"));
  } catch {
    return null;
  }
}

function writeSessionId(file, sessionId) {
  mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const tmp = `${file}.${process.pid}.tmp`;
  writeFileSync(tmp, sessionFileContent(sessionId), { mode: 0o600 });
  renameSync(tmp, file);
}

function readPlugins(home) {
  try {
    return pluginsFromSettings(readFileSync(path.join(home, SETTINGS_FILE), "utf8"));
  } catch {
    return [];
  }
}

// transcriptExists asks the SDK whether the recorded conversation is still on
// disk. A lookup that fails says nothing either way, so the turn tries the
// resume and lets the CLI report what it finds.
async function transcriptExists(sessionId, cwd) {
  try {
    return Boolean(await getSessionInfo(sessionId, { dir: cwd }));
  } catch {
    return true;
  }
}

// checkCli is the image build's guard: the SDK drives the installed CLI over
// a private protocol, so the two must be the same release.
function checkCli(claudeBin) {
  const sdkPackage = path.join(
    path.dirname(fileURLToPath(import.meta.resolve("@anthropic-ai/claude-agent-sdk"))),
    "package.json",
  );
  const want = JSON.parse(readFileSync(sdkPackage, "utf8")).claudeCodeVersion;
  const got = spawnSync(claudeBin, ["--version"], { encoding: "utf8" });
  if (got.status !== 0 || !sameCliVersion(got.stdout, want)) {
    process.stderr.write(
      `claude-runner: ${claudeBin} reports ${JSON.stringify((got.stdout || "").trim())}, ` +
        `but the Agent SDK is built for Claude Code ${want}\n`,
    );
    return EXIT_TURN_FAILED;
  }
  process.stdout.write(`${claudeBin} ${want}\n`);
  return EXIT_OK;
}

async function main(argv) {
  let args;
  try {
    args = parseArgs(argv);
  } catch (err) {
    if (err instanceof UsageError) {
      process.stderr.write(`claude-runner: ${err.message}\n`);
      return EXIT_USAGE;
    }
    throw err;
  }
  const claudeBin = resolveClaudeBin(process.env);
  if (args.checkCli) {
    return checkCli(claudeBin);
  }
  const home = process.env.HOME;
  if (!home) {
    process.stderr.write("claude-runner: HOME is not set\n");
    return EXIT_USAGE;
  }
  const cwd = process.cwd();
  const sessionFile = path.join(home, SESSION_FILE);
  const recordedId = args.resume ? readSessionId(sessionFile) : null;
  const plan = resumePlan({
    resume: args.resume,
    recordedId,
    transcriptExists: recordedId ? await transcriptExists(recordedId, cwd) : false,
  });
  if (plan.kind === "lost") {
    await notice(`the previous conversation (${plan.sessionId}) is no longer on disk, so this turn starts a new one`);
  }

  const plugins = readPlugins(home);
  const canUseTool = async (toolName, input) => {
    const verdict = decideTool(toolName, plugins, { k3s: Boolean(args.k3sMcpUrl) });
    if (verdict.allow) {
      return { behavior: "allow", updatedInput: input };
    }
    await notice(`denied ${toolName}: ${verdict.message}`);
    return { behavior: "deny", message: verdict.message };
  };
  const options = queryOptions({
    model: args.model,
    effort: args.effort,
    permissionMode: args.permissionMode,
    k3sMcpUrl: args.k3sMcpUrl,
    cwd,
    claudeBin,
    plan,
    canUseTool,
    stderr: (data) => process.stderr.write(data),
  });

  let outcome = null;
  const input = new TurnInput({
    prompt: args.prompt,
    waitMs: backgroundWaitFromEnv(process.env),
    onExpire: (taskIds) => {
      notice(`background work still running after the turn's wait (${taskIds.join(", ")}); asking the model to stop it`).catch(() => {});
    },
  });
  try {
    for await (const message of query({ prompt: input.messages(), options })) {
      await emit(JSON.stringify(message) + "\n");
      input.observe(message);
      const verdict = classify(message);
      // Recorded as soon as the CLI reports it, so a turn that fails later
      // still leaves the conversation it did create for the next resume.
      if (verdict.sessionId) {
        writeSessionId(sessionFile, verdict.sessionId);
      }
      if (verdict.outcome) {
        outcome = verdict.outcome;
        for (const error of verdict.errors) {
          await notice(error);
        }
      }
    }
  } catch (err) {
    await notice(`the turn failed: ${err && err.message ? err.message : String(err)}`);
    return EXIT_TURN_FAILED;
  } finally {
    input.close();
  }
  return outcome === "completed" ? EXIT_OK : EXIT_TURN_FAILED;
}

main(process.argv.slice(2)).then(
  (code) => {
    process.exitCode = code;
  },
  (err) => {
    process.stderr.write(`claude-runner: ${err && err.stack ? err.stack : err}\n`);
    process.exitCode = EXIT_TURN_FAILED;
  },
);
