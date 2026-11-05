// The analysis half of surface-check.mjs, kept pure for `node --test`.

// Tools that would give the model a shell, an image or the web (AC-J5).
export const FORBIDDEN_TOOLS = new Set([
  "exec_command",
  "write_stdin",
  "shell",
  "local_shell",
  "container.exec",
  "view_image",
  "image_generation",
  "web_search",
  "web_search_preview",
]);

// The text the managed config's developer_instructions start with
// (data-plane/cmd/agent/codex_config.go, codexInstructionsMarker).
export const MANAGED_INSTRUCTIONS_MARKER = "session-platform codex session";

// Skills codex bundles under $CODEX_HOME/skills/.system when bundled skills
// are on; listing any of them means the managed config did not apply.
export const BUNDLED_SKILL_ROOT = "/skills/.system";

function toolNames(tools, prefix, out) {
  for (const tool of tools || []) {
    if (!tool || typeof tool !== "object") continue;
    const name = tool.name || tool.type;
    if (tool.type === "namespace") {
      toolNames(tool.tools, `${prefix}${name}.`, out);
      continue;
    }
    out.push(`${prefix}${name}`);
    // Code mode nests every other tool inside `exec`, documented as
    // "### `name`" sections of its description.
    if (name === "exec" && typeof tool.description === "string") {
      for (const match of tool.description.matchAll(/^### `([^`]+)`/gm)) {
        out.push(`${prefix}exec>${match[1]}`);
      }
    }
  }
}

function messageTexts(items) {
  const texts = [];
  for (const item of items) {
    if (item && item.type === "message" && Array.isArray(item.content)) {
      for (const part of item.content) {
        if (part && typeof part.text === "string") {
          texts.push({ role: item.role, text: part.text });
        }
      }
    }
  }
  return texts;
}

export function listedSkills(texts) {
  const skills = [];
  const roots = [];
  for (const { text } of texts) {
    const block = text.match(/<skills_instructions>([\s\S]*?)<\/skills_instructions>/);
    if (!block) continue;
    for (const match of block[1].matchAll(/^- `(r\d+)` = `([^`]*)`/gm)) {
      roots.push(match[2]);
    }
    for (const match of block[1].matchAll(/^- ([^:\n]+): .*\(file: ([^)]+)\)\s*$/gm)) {
      skills.push({ name: match[1].trim(), file: match[2].trim() });
    }
  }
  return { skills, roots };
}

export function inspectPromptInput(items) {
  const problems = [];
  if (!Array.isArray(items)) {
    return { tools: [], skills: [], problems: ["prompt input is not a list"] };
  }
  const tools = [];
  for (const item of items) {
    if (item && item.type === "additional_tools") {
      toolNames(item.tools, "", tools);
    }
  }
  for (const tool of tools) {
    const leaf = tool.split(/[.>]/).pop();
    if (FORBIDDEN_TOOLS.has(leaf)) {
      problems.push(`model-visible tool ${tool} is outside the codex surface`);
    }
  }
  const texts = messageTexts(items);
  const { skills, roots } = listedSkills(texts);
  for (const root of roots) {
    if (root.replaceAll("\\", "/").includes(BUNDLED_SKILL_ROOT)) {
      problems.push(`bundled system skills are listed (root ${root})`);
    }
  }
  if (!texts.some(({ role, text }) => role === "developer" && text.includes(MANAGED_INSTRUCTIONS_MARKER))) {
    problems.push("the managed developer_instructions are missing; codex did not load the managed config.toml");
  }
  return { tools, skills: skills.map((s) => s.name), problems };
}
