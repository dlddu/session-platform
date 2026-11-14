package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The skills MCP (AC-J5): codex lists every skill with a SKILL.md path, and
// would normally read it with its shell, which a codex session does not have.
// So the agent binary, run as `/agent skills-mcp`, is a stdio MCP server codex
// spawns from the managed config, and it serves the skill files and nothing
// else: reads confined to CODEX_SKILLS_DIR, regular files only, no symlink in
// the path, a size cap per file. It speaks the MCP stdio transport — one
// JSON-RPC message per line on stdin and stdout, logs on stderr.
const (
	codexSkillsMCPServerName = "session-platform-skills"
	maxCodexSkillFileBytes   = 256 << 10
	// maxCodexSkillsMCPLine bounds one inbound frame; the only client is codex.
	maxCodexSkillsMCPLine = 1 << 20
	maxCodexSkillsListed  = 500
	maxCodexSkillFiles    = 200
)

type codexSkillsServer struct {
	root string
	// agentURL is the agent's loopback base URL; refresh_skills is offered
	// only when it is set (AC-J7).
	agentURL string
	// client posts the refresh; a field so tests can bound it.
	client *http.Client
	logger *slog.Logger
}

// serveCodexSkillsMCP answers requests until stdin closes. A frame it cannot
// parse is answered with a parse error rather than ending the session, since
// codex keeps one server process for the whole turn.
func serveCodexSkillsMCP(stdin io.Reader, stdout io.Writer, root, agentURL string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	server := codexSkillsServer{root: root, agentURL: strings.TrimRight(strings.TrimSpace(agentURL), "/"),
		client: &http.Client{Timeout: codexSkillsRefreshTimeout}, logger: logger}
	reader := bufio.NewReaderSize(stdin, 64<<10)
	out := bufio.NewWriter(stdout)
	for {
		line, err := readCodexMCPLine(reader)
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return out.Flush()
		}
		if errors.Is(err, errCodexMCPLineTooLong) {
			if err := writeCodexMCP(out, jsonRPCResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &jsonRPCError{Code: jsonRPCInvalidRequest, Message: "request exceeds the frame limit"}}); err != nil {
				return err
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if response, ok := server.handle(line); ok {
			if err := writeCodexMCP(out, response); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return out.Flush()
		}
	}
}

var errCodexMCPLineTooLong = errors.New("frame too long")

// readCodexMCPLine reads one line, discarding the rest of one that is longer
// than the frame limit.
func readCodexMCPLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if len(line)+len(chunk) > maxCodexSkillsMCPLine {
			for isPrefix && err == nil {
				_, isPrefix, err = r.ReadLine()
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return nil, errCodexMCPLineTooLong
		}
		line = append(line, chunk...)
		if err != nil {
			return line, err
		}
		if !isPrefix {
			return line, nil
		}
	}
}

func writeCodexMCP(out *bufio.Writer, response jsonRPCResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return out.Flush()
}

func (s codexSkillsServer) handle(line []byte) (jsonRPCResponse, bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return jsonRPCResponse{}, false
	}
	var req jsonRPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return jsonRPCResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &jsonRPCError{Code: jsonRPCParseError, Message: "malformed JSON-RPC request"}}, true
	}
	if len(req.ID) == 0 || string(req.ID) == "null" {
		// A notification (notifications/initialized among them) takes no reply.
		return jsonRPCResponse{}, false
	}
	reply := func(result any) (jsonRPCResponse, bool) {
		return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
	}
	fail := func(code int, message string) (jsonRPCResponse, bool) {
		return jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: &jsonRPCError{Code: code, Message: message}}, true
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return fail(jsonRPCInvalidRequest, "not a JSON-RPC 2.0 request")
	}
	switch req.Method {
	case "initialize":
		version := sessionMCPProtocolVersion
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) == nil && params.ProtocolVersion != "" {
			version = params.ProtocolVersion
		}
		return reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": codexSkillsMCPServerName, "version": sessionMCPServerVersion},
		})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		return reply(map[string]any{"tools": codexSkillsToolDefinitions(s.agentURL != "")})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fail(jsonRPCInvalidParams, "tools/call needs a name and arguments")
		}
		text, err := s.call(params.Name, params.Arguments)
		if errors.Is(err, errCodexUnknownTool) {
			return fail(jsonRPCInvalidParams, "unknown tool "+params.Name)
		}
		if err != nil {
			return reply(codexToolResult(err.Error(), true))
		}
		return reply(codexToolResult(text, false))
	default:
		return fail(jsonRPCMethodNotFound, "skills MCP does not implement "+req.Method)
	}
}

func codexToolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func codexSkillsToolDefinitions(refresh bool) []map[string]any {
	tools := []map[string]any{
		{
			"name":        "list_skills",
			"description": "List the skills available in this session, each with its name, description and files.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			"name": "read_skill",
			"description": "Read a skill's SKILL.md, or another file of that skill. `name` is the skill's directory " +
				"name, as in its listed path (<name>/SKILL.md). `file` is a path inside that directory and defaults to SKILL.md.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Skill directory name."},
					"file": map[string]any{"type": "string", "description": "File inside the skill directory; default SKILL.md."},
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
		},
	}
	if refresh {
		tools = append(tools, map[string]any{
			"name": "refresh_skills",
			"description": "Fetch the platform's plugin marketplace again and update this session's skills, keeping " +
				"the conversation. It can take up to two minutes, and runs at most once per turn. It reports which " +
				"skills were added, updated or removed.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		})
	}
	return tools
}

var errCodexUnknownTool = errors.New("unknown tool")

func (s codexSkillsServer) call(name string, raw json.RawMessage) (string, error) {
	switch name {
	case "list_skills":
		return s.listSkills()
	case "read_skill":
		var args struct {
			Name string `json:"name"`
			File string `json:"file"`
		}
		if len(raw) != 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", errors.New("arguments must be an object with a string name and an optional string file")
			}
		}
		return s.readSkill(args.Name, args.File)
	case "refresh_skills":
		if s.agentURL == "" {
			return "", errCodexUnknownTool
		}
		return s.refreshSkills()
	default:
		return "", errCodexUnknownTool
	}
}

// openRoot opens the skills directory itself, refusing one that is a symlink.
func (s codexSkillsServer) openRoot() (*os.Root, error) {
	if strings.TrimSpace(s.root) == "" {
		return nil, errors.New("no skills directory is configured")
	}
	info, err := os.Lstat(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("there are no skills in this session")
	}
	if err != nil {
		return nil, errors.New("the skills directory cannot be read")
	}
	if !info.IsDir() {
		return nil, errors.New("the skills directory is not a directory")
	}
	return os.OpenRoot(s.root)
}

type codexSkillListing struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Files       []string `json:"files"`
}

func (s codexSkillsServer) listSkills() (string, error) {
	root, err := s.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return "", errors.New("the skills directory cannot be read")
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return "", errors.New("the skills directory cannot be read")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	skills := []codexSkillListing{}
	for _, entry := range entries {
		if len(skills) >= maxCodexSkillsListed {
			break
		}
		if !entry.IsDir() || !validCodexSkillSegment(entry.Name()) {
			continue
		}
		content, err := readCodexSkillFile(root, entry.Name(), "SKILL.md")
		if err != nil {
			continue
		}
		skills = append(skills, codexSkillListing{
			Name:        entry.Name(),
			Description: skillDescription(content),
			Files:       listCodexSkillFiles(root, entry.Name()),
		})
	}
	encoded, err := json.MarshalIndent(map[string]any{"skills": skills}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// listCodexSkillFiles is the readable files of one skill, symlinks and
// special files left out, bounded so a huge skill cannot flood the listing.
func listCodexSkillFiles(root *os.Root, skill string) []string {
	var files []string
	_ = fs.WalkDir(root.FS(), skill, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != skill && strings.HasPrefix(entry.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || entry.Name() == miniMaxManagedPluginMarker {
			return nil
		}
		files = append(files, strings.TrimPrefix(p, skill+"/"))
		if len(files) >= maxCodexSkillFiles {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func (s codexSkillsServer) readSkill(name, file string) (string, error) {
	if !validCodexSkillSegment(name) {
		return "", errors.New("name must be a skill directory name from list_skills")
	}
	if file == "" {
		file = "SKILL.md"
	}
	root, err := s.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()
	dir, err := resolveCodexSkillName(root, name)
	if err != nil {
		return "", err
	}
	content, err := readCodexSkillFile(root, dir, file)
	if err != nil {
		return "", err
	}
	return string(bytes.ToValidUTF8(content, []byte("\uFFFD"))), nil
}

// resolveCodexSkillName accepts the skill's directory, or the front-matter
// name codex lists it under when exactly one skill has it: codex shows
// `git-repo-cloner`, while the directory is `session-platform--git-repo-cloner`.
func resolveCodexSkillName(root *os.Root, name string) (string, error) {
	if info, err := root.Lstat(name); err == nil && info.IsDir() {
		return name, nil
	}
	dir, err := root.Open(".")
	if err != nil {
		return "", errors.New("the skills directory cannot be read")
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return "", errors.New("the skills directory cannot be read")
	}
	var matches []string
	for _, entry := range entries {
		if !entry.IsDir() || !validCodexSkillSegment(entry.Name()) {
			continue
		}
		content, err := readCodexSkillFile(root, entry.Name(), "SKILL.md")
		if err == nil && skillFrontMatter(content, "name") == name {
			matches = append(matches, entry.Name())
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return name, nil
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%d skills are named %q; pass one of their directory names: %s",
			len(matches), name, strings.Join(matches, ", "))
	}
}

// readCodexSkillFile reads one file of one skill. os.Root already refuses any
// path that leaves the skills directory, through ".." or a symlink; this also
// refuses a symlink that stays inside it, so a skill can never read another
// skill's files, or the marker, by indirection.
func readCodexSkillFile(root *os.Root, skill, file string) ([]byte, error) {
	clean := path.Clean(strings.ReplaceAll(file, `\`, "/"))
	if file == "" || path.IsAbs(file) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
		path.Base(clean) == miniMaxManagedPluginMarker {
		return nil, fmt.Errorf("file %q is not inside the skill", file)
	}
	rel := path.Join(skill, clean)
	current := ""
	for _, part := range strings.Split(rel, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if current == skill {
				return nil, fmt.Errorf("no skill named %q", skill)
			}
			return nil, fmt.Errorf("skill %s has no file %s", skill, clean)
		}
		if err != nil {
			return nil, fmt.Errorf("file %q cannot be read", file)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("file %q is a link, which skills may not read through", file)
		}
		if current != rel && !info.IsDir() {
			return nil, fmt.Errorf("skill %s has no file %s", skill, clean)
		}
	}
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("file %q cannot be read", file)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file %q is not a regular file", file)
	}
	if info.Size() > maxCodexSkillFileBytes {
		return nil, fmt.Errorf("file %q is larger than %d KiB", file, maxCodexSkillFileBytes>>10)
	}
	content, err := io.ReadAll(io.LimitReader(f, maxCodexSkillFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("file %q cannot be read", file)
	}
	if len(content) > maxCodexSkillFileBytes {
		return nil, fmt.Errorf("file %q is larger than %d KiB", file, maxCodexSkillFileBytes>>10)
	}
	return content, nil
}

// skillDescription is the description field of a SKILL.md front matter, for
// the listing. skillFrontMatter understands the plain, quoted and folded
// scalar forms the marketplace uses, and gives up on anything else.
func skillDescription(content []byte) string {
	return skillFrontMatter(content, "description")
}

func skillFrontMatter(content []byte, field string) string {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return ""
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return ""
	}
	lines := strings.Split(front, "\n")
	for i, line := range lines {
		value, ok := strings.CutPrefix(line, field+":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case value == ">" || value == ">-" || value == "|" || value == "|-":
			var parts []string
			for _, next := range lines[i+1:] {
				if next != "" && next[0] != ' ' && next[0] != '\t' {
					break
				}
				parts = append(parts, strings.TrimSpace(next))
			}
			return oneLine(strings.Join(parts, " "))
		case strings.HasPrefix(value, `"`):
			var decoded string
			if json.Unmarshal([]byte(value), &decoded) == nil {
				return oneLine(decoded)
			}
			return oneLine(strings.Trim(value, `"`))
		case strings.HasPrefix(value, "'"):
			return oneLine(strings.ReplaceAll(strings.Trim(value, "'"), "''", "'"))
		default:
			return oneLine(value)
		}
	}
	return ""
}

// codexSkillsRefreshTimeout covers the agent's refresh, which is bounded by
// the sidecar's clone, plus the round trip.
const codexSkillsRefreshTimeout = codexPluginRefreshTimeout + 15*time.Second

// refreshSkills asks the agent to refresh the plugin skills for the running
// turn (AC-J7) and words the result for the model.
func (s codexSkillsServer) refreshSkills() (string, error) {
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: codexSkillsRefreshTimeout}
	}
	resp, err := client.Post(s.agentURL+pluginRefreshTurnPath, "application/json", nil)
	if err != nil {
		s.logger.Warn("refresh_skills: the agent did not answer", "err", err)
		return "", errors.New("the session agent did not answer the refresh; the skills are unchanged")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		reason := strings.TrimSpace(string(bytes.ToValidUTF8(body, []byte("\uFFFD"))))
		if reason == "" {
			reason = http.StatusText(resp.StatusCode)
		}
		return "", fmt.Errorf("refresh refused (%d): %s; the skills are unchanged", resp.StatusCode, reason)
	}
	var result pluginRefresh
	if err := json.Unmarshal(body, &result); err != nil {
		return "", errors.New("the session agent answered the refresh without a result")
	}
	return renderCodexRefreshResult(result), nil
}

func renderCodexRefreshResult(result pluginRefresh) string {
	if result.State == miniMaxSeedStateUnconfigured {
		return "This session has no plugin marketplace to refresh; its skills are unchanged."
	}
	revision := ""
	if result.Revision != "" {
		revision = " at marketplace revision " + shortRevision(result.Revision)
	}
	var parts []string
	for _, group := range []struct {
		kind  string
		names []string
	}{{codexSkillChangeAdded, result.Added}, {codexSkillChangeUpdated, result.Updated}, {codexSkillChangeRemoved, result.Removed}} {
		if len(group.names) == 0 {
			continue
		}
		quoted := make([]string, len(group.names))
		for i, name := range group.names {
			quoted[i] = "`" + name + "`"
		}
		parts = append(parts, group.kind+": "+strings.Join(quoted, ", "))
	}
	if len(parts) == 0 {
		return "Plugin skills refreshed" + revision + "; nothing changed."
	}
	return "Plugin skills refreshed" + revision + " — " + strings.Join(parts, "; ") + ". " +
		"Skill files read earlier in this conversation may differ from the current ones. The skill list given at " +
		"the start of this turn is not updated until the next turn; `list_skills` shows the current set."
}
