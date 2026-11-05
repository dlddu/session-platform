package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
const (
	codexConfigDirName  = ".codex"
	codexConfigFileName = "config.toml"

	// A K3s MCP write waits for the K3s MCP's own human approval, which codex's
	// default per-call timeout would cut off.
	codexK3SToolTimeout = 15 * time.Minute

	// codexInstructionsMarker is what the image's surface check looks for to
	// prove the managed config was the one codex loaded.
	codexInstructionsMarker = "session-platform codex session"
)

// codexDisabledFeatures are the codex features that would give the model a
// tool or a side channel outside conversation, plugin skills and the K3s MCP.
var codexDisabledFeatures = []string{
	"shell_tool", "unified_exec", "view_image", "image_generation",
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
	"When the `k3s` MCP server is present, its tools act on the homelab cluster; it asks a human before any write."

// codexConfig is what varies between pods.
type codexConfig struct {
	// k3sMCPURL is the sidecar's MCP endpoint, or empty when it holds no token.
	k3sMCPURL string
	// agentBinary is this binary, which codex spawns as the skills MCP.
	agentBinary string
	skillsDir   string
}

func renderCodexConfig(cfg codexConfig) []byte {
	doc := &tomlDocument{}
	doc.comment("Managed by session-platform and rewritten on every start; local edits do not persist.")
	doc.set("approval_policy", tomlString("never"))
	doc.set("sandbox_mode", tomlString("read-only"))
	doc.set("web_search", tomlString("disabled"))
	// The login is the session's own and is archived with HOME (AC-J3), so it
	// has to be a file, never an OS keyring the next pod does not have.
	doc.set("cli_auth_credentials_store", tomlString("file"))
	doc.set("forced_login_method", tomlString("chatgpt"))
	doc.set("check_for_update_on_startup", tomlBool(false))
	doc.set("developer_instructions", tomlString(codexDeveloperInstructions))

	doc.table("features")
	for _, feature := range codexDisabledFeatures {
		doc.set(feature, tomlBool(false))
	}
	doc.table("skills", "bundled")
	doc.set("enabled", tomlBool(false))

	// Both servers run their tools without asking: approval_policy=never has
	// no one to ask, the skills server only reads, and the K3s MCP gates its
	// own writes behind the sidecar's deny filter.
	if cfg.k3sMCPURL != "" {
		doc.table("mcp_servers", codexK3SServerName)
		doc.set("url", tomlString(cfg.k3sMCPURL))
		doc.set("default_tools_approval_mode", tomlString("approve"))
		doc.set("tool_timeout_sec", tomlInt(int64(codexK3SToolTimeout/time.Second)))
	}
	doc.table("mcp_servers", codexSkillsServerName)
	doc.set("command", tomlString(cfg.agentBinary))
	doc.set("args", tomlStringArray([]string{codexSkillsMCPCommand}))
	doc.set("env", tomlInlineStringTable([][2]string{{codexSkillsDirEnv, cfg.skillsDir}}))
	doc.set("default_tools_approval_mode", tomlString("approve"))
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
