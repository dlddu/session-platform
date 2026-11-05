// surface-check: proves a codex HOME offers the model no tool outside the
// codex workload's surface (docs/prd/codex-workload.md, AC-J5). It renders
// what the model would be sent with `codex debug prompt-input` and fails on a
// shell, image or web tool, on a bundled system skill, or when the managed
// config's instructions are missing (which would mean codex did not load it).
//
//   node surface-check.mjs --home <HOME>     (CODEX_HOME = <HOME>/.codex)
//
// MCP servers are not part of that rendering: codex starts them but does not
// wait for their tools, so this check covers what the config removes, not
// what the K3s and skills MCP servers add.
import { spawnSync } from "node:child_process";
import path from "node:path";
import { resolveCodexBin } from "./lib.mjs";
import { inspectPromptInput } from "./surface.mjs";

function usage(message) {
  process.stderr.write(`surface-check: ${message}\nusage: node surface-check.mjs --home <HOME>\n`);
  process.exit(64);
}

const argv = process.argv.slice(2);
if (argv.length !== 2 || argv[0] !== "--home" || !argv[1]) {
  usage("expected --home <HOME>");
}
const home = path.resolve(argv[1]);
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
const report = inspectPromptInput(items);
process.stdout.write(`model-visible tools:\n${report.tools.map((t) => `  ${t}`).join("\n")}\n`);
process.stdout.write(`listed skills: ${report.skills.length ? report.skills.join(", ") : "(none)"}\n`);
if (report.problems.length) {
  for (const problem of report.problems) {
    process.stderr.write(`surface-check: ${problem}\n`);
  }
  process.exit(1);
}
process.stdout.write("surface-check: ok\n");
