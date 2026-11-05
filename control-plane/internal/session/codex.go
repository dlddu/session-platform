package session

// WorkloadTypeCodex selects the Codex SDK workload (docs/prd/codex-workload.md,
// AC-J1~J6). It runs on the claude-code execution model (AC-E2~E5) with its
// own CLI, a per-session ChatGPT login and a tool surface narrowed to
// conversation, platform plugin skills and the K3s MCP.
const WorkloadTypeCodex WorkloadType = "codex"
