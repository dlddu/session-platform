// surface-check: proves a codex HOME offers the model exactly its mode's
// surface — codex-chat's (docs/prd/codex-workload.md, AC-J5) or codex-task's
// (docs/prd/codex-task-workload.md, AC-M1). It renders what the model would be
// sent with `codex debug prompt-input` and fails on an image tool, on a shell
// or web tool in chat or a missing shell in task, on a bundled system skill, or
// when the mode's managed instructions are missing (which would mean codex did
// not load the managed config).
//
//   node surface-check.mjs --home <HOME> [--mode chat|task]
//                                            (CODEX_HOME = <HOME>/.codex)
//
// MCP servers are not part of that rendering: codex starts them but does not
// wait for their tools, so this check covers what the config removes, not
// what the K3s and skills MCP servers add.
import { spawnSync } from "node:child_process";
import path from "node:path";
import { resolveCodexBin } from "./lib.mjs";
import { inspectPromptInput } from "./surface.mjs";

function usage(message) {
  process.stderr.write(`surface-check: ${message}\nusage: node surface-check.mjs --home <HOME> [--mode chat|task]\n`);
  process.exit(64);
}

const argv = process.argv.slice(2);
let homeArg = null;
let mode = "chat";
for (let i = 0; i < argv.length; i += 2) {
  if (argv[i] === "--home" && argv[i + 1]) {
    homeArg = argv[i + 1];
  } else if (argv[i] === "--mode" && (argv[i + 1] === "chat" || argv[i + 1] === "task")) {
    mode = argv[i + 1];
  } else {
    usage(`unexpected argument ${JSON.stringify(argv[i])}`);
  }
}
if (!homeArg) {
  usage("expected --home <HOME>");
}
const home = path.resolve(homeArg);
const codexHome = path.join(home, ".codex");
const bin = resolveCodexBin(process.env);
const result = spawnSync(bin, ["debug", "prompt-input"], {
  cwd: home,
  env: { HOME: home, CODEX_HOME: codexHome, PATH: process.env.PATH || "/usr/local/bin:/usr/bin:/bin", LANG: "C.UTF-8" },
  encoding: "utf8",
  maxBuffer: 64 << 20,
  timeout: 120_000,
});
if (result.status !== 0) {
  process.stderr.write(result.stderr || "");
  process.stderr.write(`surface-check: codex debug prompt-input exited ${result.status ?? result.signal}\n`);
  process.exit(1);
}
let items;
try {
  items = JSON.parse(result.stdout);
} catch (err) {
  process.stderr.write(`surface-check: prompt input is not JSON: ${err.message}\n`);
  process.exit(1);
}
const report = inspectPromptInput(items, mode);
process.stdout.write(`model-visible tools:\n${report.tools.map((t) => `  ${t}`).join("\n")}\n`);
process.stdout.write(`listed skills: ${report.skills.length ? report.skills.join(", ") : "(none)"}\n`);
if (report.problems.length) {
  for (const problem of report.problems) {
    process.stderr.write(`surface-check: ${problem}\n`);
  }
  process.exit(1);
}
process.stdout.write(`surface-check: ok (${mode})\n`);
