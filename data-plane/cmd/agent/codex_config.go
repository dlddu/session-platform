package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The managed $HOME/.codex/config.toml (AC-J5). codex reads it on every run,
// and it is platform-owned: rendered whole from this pod's environment on every
// boot and restore, so an archived copy — or one the model was talked into
// changing — never survives a restart. Observed on the pinned codex (0.166.0)
// with `codex debug prompt-input`: these switches remove the shell
// (exec_command, write_stdin), view_image and the bundled system skills from
// what the model is offered. apply_patch and the collaboration tools remain,
// because the remote model catalog forces code mode and multi-agent on; the
// read-only sandbox, the never-ask approval policy and codex-runner's kill
// switch are what stop a file edit or a command from happening.
//
// codex-task's config (docs/prd/codex-task-workload.md, AC-M1) is the same
// document with the shell left on and the sandbox opened: the session pod is
// the execution boundary, as it is for claude-code. Everything else that
// codex-chat turns off stays off.
const (
	codexConfigDirName  = ".codex"
	codexConfigFileName = "config.toml"

	// A K3s MCP write waits for the K3s MCP's own human approval, which codex's
	// default per-call timeout would cut off.
	codexK3SToolTimeout = 15 * time.Minute
	// A session DB statement is bounded by the sidecar (sessionDBCallTimeout);
	// this only has to outlast it.
	codexSessionDBToolTimeout = sessionDBCallTimeout + 15*time.Second
	// refresh_skills waits for a marketplace clone (AC-J7), which codex's
	// default per-call timeout would cut off.
	codexSkillsToolTimeout = codexSkillsRefreshTimeout + 15*time.Second
	// Scheduler calls only touch the sidecar's memory (AC-P1).
	codexSchedulerToolTimeout = 30 * time.Second

	// codexInstructionsMarker and codexTaskInstructionsMarker are what the
	// image's surface check looks for to prove the managed config of the
	// mode it checks was the one codex loaded. Neither contains the other.
	// Keep in sync with codex-runner/surface.mjs.
	codexInstructionsMarker     = "session-platform codex session"
	codexTaskInstructionsMarker = "session-platform codex task session"
)

// codexDisabledFeatures are the codex features that would give the model a
// tool or a side channel outside conversation, plugin skills and the K3s MCP.
var codexDisabledFeatures = append(slices.Clone(codexTaskEnabledFeatures), codexTaskDisabledFeatures...)

// codexTaskEnabledFeatures are the shell features codex-task sets on rather
// than leaving to Codex's defaults, which a version bump can change (AC-M1).
var codexTaskEnabledFeatures = []string{"shell_tool", "unified_exec"}

// codexTaskDisabledFeatures are codex-chat's minus the shell (AC-M1).
var codexTaskDisabledFeatures = []string{
	"view_image", "image_generation",
	"apps", "plugins", "remote_plugin", "computer_use", "browser_use", "in_app_browser",
	"sleep_tool", "goals", "hooks", "memories", "tool_suggest", "skill_mcp_dependency_install",
}

const codexDeveloperInstructions = "You are a " + codexInstructionsMarker + ". " +
	"You have no shell, no file editing, no web search and no image tools: an attempt to run a command, " +
	"change a file or search the web ends the turn. " +
	"Skills are listed with a file path, but you cannot open that path yourself. " +
	"Read a skill with the `read_skill` tool of the `skills` MCP server: pass the skill's name, or its directory " +
	"(the path segment before /SKILL.md, for example `session-platform--example`), as `name`, and a file " +
	"inside that directory as `file` when the skill refers to one. `list_skills` lists them. " +
	codexRefreshInstructions + codexSchedulerInstructions +
	"When the `k3s` MCP server is present, its tools act on the homelab cluster; it asks a human before any write. " +
	"When the `db` MCP server is present, it is this session's own SQLite database, kept across restarts: " +
	"`raw_query` runs a statement that returns rows, `raw_exec` one that changes data. Discover `ledger_get/list/put/delete` for task bookkeeping that carries over to the next chat session on this slot; never share this DB with workers. Raw counters are not committed totals; open raw transactions roll back at call end."

const codexTaskDeveloperInstructions = "You are a " + codexTaskInstructionsMarker + ": a coding agent with a shell " +
	"and file editing in this session's own workspace, which is a dedicated pod. Web search and image tools are off. " +
	"Skills are listed with a file path; open SKILL.md there, or read it with the `read_skill` tool of the `skills` " +
	"MCP server (`list_skills` lists them). " +
	codexRefreshInstructions + codexSchedulerInstructions +
	"When the `k3s` MCP server is present, its tools act on the homelab cluster; it asks a human before any write."

// codexRefreshInstructions is shared by both modes (AC-J7).
const codexRefreshInstructions = "When the user asks to refresh or update the plugins or skills, call `refresh_skills` " +
	"of the `skills` MCP server, when it is offered, and read again any changed skill you rely on. "

// codexSchedulerInstructions is shared by both modes (AC-P1).
const codexSchedulerInstructions = "To run a prompt later or repeatedly in this session, use the `scheduler` MCP server, " +
	"when it is offered: pass times as ISO 8601 durations or RFC 3339 times, and tell the user the time it returns. "

// codexConfig is what varies between pods.
type codexConfig struct {
	// task renders codex-task's surface (AC-M1) instead of codex-chat's.
	task bool
	// k3sMCPURL is the sidecar's MCP endpoint, or empty when it holds no token.
	k3sMCPURL string
	// sessionDBURL is the sidecar's session DB MCP (AC-N6), or empty.
	sessionDBURL string
	// schedulerURL is the sidecar's scheduler MCP (AC-P1), or empty.
	schedulerURL string
	// agentBinary is this binary, which codex spawns as the skills MCP.
	agentBinary string
	skillsDir   string
	// agentURL is the agent on loopback; set, the skills MCP offers
	// refresh_skills (AC-J7).
	agentURL string
}

func renderCodexConfig(cfg codexConfig) []byte {
	doc := &tomlDocument{}
	doc.comment("Managed by session-platform and rewritten on every start; local edits do not persist.")
	sandbox, instructions, disabled := "read-only", codexDeveloperInstructions, codexDisabledFeatures
	if cfg.task {
		sandbox, instructions, disabled = "danger-full-access", codexTaskDeveloperInstructions, codexTaskDisabledFeatures
	}
	doc.set("approval_policy", tomlString("never"))
	doc.set("sandbox_mode", tomlString(sandbox))
	doc.set("web_search", tomlString("disabled"))
	// The login is the session's own and is archived with HOME (AC-J3), so it
	// has to be a file, never an OS keyring the next pod does not have.
	doc.set("cli_auth_credentials_store", tomlString("file"))
	doc.set("forced_login_method", tomlString("chatgpt"))
	doc.set("check_for_update_on_startup", tomlBool(false))
	doc.set("developer_instructions", tomlString(instructions))

	doc.table("features")
	if cfg.task {
		for _, feature := range codexTaskEnabledFeatures {
			doc.set(feature, tomlBool(true))
		}
	}
	for _, feature := range disabled {
		doc.set(feature, tomlBool(false))
	}
	doc.table("skills", "bundled")
	doc.set("enabled", tomlBool(false))

	// Every server runs its tools without asking: approval_policy=never has
	// no one to ask, the skills server only reads, the K3s MCP gates its own
	// writes behind the sidecar's deny filter, and the session DB is the
	// session's own (AC-N6).
	if cfg.k3sMCPURL != "" {
		doc.table("mcp_servers", codexK3SServerName)
		doc.set("url", tomlString(cfg.k3sMCPURL))
		doc.set("default_tools_approval_mode", tomlString("approve"))
		doc.set("tool_timeout_sec", tomlInt(int64(codexK3SToolTimeout/time.Second)))
	}
	if cfg.sessionDBURL != "" {
		doc.table("mcp_servers", codexSessionDBServerName)
		doc.set("url", tomlString(cfg.sessionDBURL))
		doc.set("default_tools_approval_mode", tomlString("approve"))
		doc.set("tool_timeout_sec", tomlInt(int64(codexSessionDBToolTimeout/time.Second)))
	}
	if cfg.schedulerURL != "" {
		doc.table("mcp_servers", codexSchedulerServerName)
		doc.set("url", tomlString(cfg.schedulerURL))
		doc.set("default_tools_approval_mode", tomlString("approve"))
		doc.set("tool_timeout_sec", tomlInt(int64(codexSchedulerToolTimeout/time.Second)))
	}
	doc.table("mcp_servers", codexSkillsServerName)
	doc.set("command", tomlString(cfg.agentBinary))
	doc.set("args", tomlStringArray([]string{codexSkillsMCPCommand}))
	skillsEnv := [][2]string{{codexSkillsDirEnv, cfg.skillsDir}}
	if cfg.agentURL != "" {
		skillsEnv = append(skillsEnv, [2]string{codexAgentURLEnv, cfg.agentURL})
	}
	doc.set("env", tomlInlineStringTable(skillsEnv))
	doc.set("default_tools_approval_mode", tomlString("approve"))
	doc.set("tool_timeout_sec", tomlInt(int64(codexSkillsToolTimeout/time.Second)))
	return doc.bytes()
}

func writeCodexConfig(path string, content []byte) error {
	dir := filepath.Dir(path)
	if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
		return fmt.Errorf("codex home %s is not a directory", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create codex home: %w", err)
	}
	f, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return fmt.Errorf("create codex config: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("write codex config: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install codex config: %w", err)
	}
	return nil
}

// tomlDocument is the small TOML writer the managed config needs: top-level
// keys, then tables of scalar values. Every value is encoded by a function
// below, so no value can break out of its string or its line.
type tomlDocument struct {
	b strings.Builder
}

func (d *tomlDocument) comment(text string) {
	d.b.WriteString("# " + strings.ReplaceAll(text, "\n", " ") + "\n")
}

func (d *tomlDocument) table(path ...string) {
	keys := make([]string, len(path))
	for i, key := range path {
		keys[i] = tomlKey(key)
	}
	d.b.WriteString("\n[" + strings.Join(keys, ".") + "]\n")
}

func (d *tomlDocument) set(key, encoded string) {
	d.b.WriteString(tomlKey(key) + " = " + encoded + "\n")
}

func (d *tomlDocument) bytes() []byte { return []byte(d.b.String()) }

var tomlBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func tomlKey(key string) string {
	if tomlBareKey.MatchString(key) {
		return key
	}
	return tomlString(key)
}

// tomlString encodes a TOML basic string. Invalid UTF-8 is replaced, since
// TOML documents must be UTF-8, and every control character is escaped, since
// a basic string may contain none but tab.
func tomlString(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func tomlBool(v bool) string { return strconv.FormatBool(v) }

func tomlInt(v int64) string { return strconv.FormatInt(v, 10) }

func tomlStringArray(values []string) string {
	encoded := make([]string, len(values))
	for i, v := range values {
		encoded[i] = tomlString(v)
	}
	return "[" + strings.Join(encoded, ", ") + "]"
}

// tomlInlineStringTable keeps the caller's order, so the rendering is stable.
func tomlInlineStringTable(pairs [][2]string) string {
	encoded := make([]string, len(pairs))
	for i, kv := range pairs {
		encoded[i] = tomlKey(kv[0]) + " = " + tomlString(kv[1])
	}
	return "{ " + strings.Join(encoded, ", ") + " }"
}
