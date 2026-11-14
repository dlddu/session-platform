package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeSkillTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "skills")
	files := map[string]string{
		"p--alpha/SKILL.md":                       "---\nname: alpha\ndescription: >-\n  Alpha does\n  things.\n---\n# Alpha\n",
		"p--alpha/references/guide.md":            "guide\n",
		"p--alpha/" + miniMaxManagedPluginMarker:  "",
		"mine/SKILL.md":                           "---\ndescription: 'It''s mine'\n---\n",
		"nodesc/SKILL.md":                         "no front matter\n",
		"big/SKILL.md":                            "---\ndescription: big\n---\n",
		"big/huge.txt":                            strings.Repeat("x", maxCodexSkillFileBytes+1),
		"big/exact.txt":                           strings.Repeat("y", maxCodexSkillFileBytes),
		"no-skill-md/README.md":                   "not a skill\n",
		".hidden/SKILL.md":                        "hidden\n",
		"p--alpha/.git/config":                    "x\n",
		"outside-secret.txt":                      "secret\n",
		"p--alpha/references/deeper/nested/n.txt": "nested\n",
		"a--twin/SKILL.md":                        "---\nname: twin\n---\n",
		"b--twin/SKILL.md":                        "---\nname: twin\n---\n",
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"p--alpha/escape.txt":   secret,
		"p--alpha/sibling.txt":  "../mine/SKILL.md",
		"p--alpha/linkdir":      filepath.Join(root, "mine"),
		"linked-skill":          filepath.Join(root, "mine"),
		"p--alpha/marker-alias": miniMaxManagedPluginMarker,
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func runSkillsMCP(t *testing.T, root string, frames ...string) []map[string]any {
	t.Helper()
	return runSkillsMCPWithAgent(t, root, "", frames...)
}

func runSkillsMCPWithAgent(t *testing.T, root, agentURL string, frames ...string) []map[string]any {
	t.Helper()
	var stdout bytes.Buffer
	if err := serveCodexSkillsMCP(strings.NewReader(strings.Join(frames, "\n")), &stdout, root, agentURL, testLogger()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var responses []map[string]any
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 1<<20), 4<<20)
	for scanner.Scan() {
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("response is not one JSON line: %q", scanner.Text())
		}
		responses = append(responses, response)
	}
	return responses
}

func callFrame(id int, tool string, args any) string {
	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	return string(frame)
}

func toolText(t *testing.T, response map[string]any) (string, bool) {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response %v has no result", response)
	}
	content := result["content"].([]any)
	return content[0].(map[string]any)["text"].(string), result["isError"].(bool)
}

// AC-J5: the stdio handshake codex performs, then both tools.
func TestCodexSkillsMCPHandshake(t *testing.T) {
	root := writeSkillTree(t)
	responses := runSkillsMCP(t, root,
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"progressToken":0}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`,
		`not json`,
		``,
		`{"jsonrpc":"1.0","id":4,"method":"ping"}`,
		callFrame(5, "nope", map[string]any{}),
	)
	if len(responses) != 7 {
		t.Fatalf("responses = %d (%v), want one per request and none for the notification or the blank line", len(responses), responses)
	}
	init := responses[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["serverInfo"].(map[string]any)["name"] != codexSkillsMCPServerName {
		t.Errorf("initialize = %v", init)
	}
	tools := responses[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "list_skills" || tools[1].(map[string]any)["name"] != "read_skill" {
		t.Errorf("tools = %v", tools)
	}
	if responses[2]["result"] == nil {
		t.Error("ping must answer")
	}
	for i, code := range []float64{jsonRPCMethodNotFound, jsonRPCParseError, jsonRPCInvalidRequest, jsonRPCInvalidParams} {
		got, _ := responses[3+i]["error"].(map[string]any)
		if got == nil || got["code"] != code {
			t.Errorf("response %d = %v, want error %v", 3+i, responses[3+i], code)
		}
	}
}

func TestCodexSkillsMCPListSkills(t *testing.T) {
	root := writeSkillTree(t)
	text, isError := toolText(t, runSkillsMCP(t, root, callFrame(1, "list_skills", map[string]any{}))[0])
	if isError {
		t.Fatalf("list_skills failed: %s", text)
	}
	var listing struct {
		Skills []codexSkillListing `json:"skills"`
	}
	if err := json.Unmarshal([]byte(text), &listing); err != nil {
		t.Fatal(err)
	}
	byName := map[string]codexSkillListing{}
	var names []string
	for _, s := range listing.Skills {
		byName[s.Name] = s
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "a--twin,b--twin,big,mine,nodesc,p--alpha" {
		t.Fatalf("skills = %v, want only real, unlinked, visible skill directories", names)
	}
	if byName["p--alpha"].Description != "Alpha does things." || byName["mine"].Description != "It's mine" {
		t.Errorf("descriptions = %q / %q", byName["p--alpha"].Description, byName["mine"].Description)
	}
	files := strings.Join(byName["p--alpha"].Files, ",")
	if files != "SKILL.md,references/deeper/nested/n.txt,references/guide.md" {
		t.Errorf("alpha files = %s, want no marker, links or dot directories", files)
	}
}

func TestCodexSkillsMCPReadSkill(t *testing.T) {
	root := writeSkillTree(t)
	read := func(args map[string]any) (string, bool) {
		return toolText(t, runSkillsMCP(t, root, callFrame(1, "read_skill", args))[0])
	}
	if text, isError := read(map[string]any{"name": "p--alpha"}); isError || !strings.Contains(text, "# Alpha") {
		t.Errorf("default file = %q (%v)", text, isError)
	}
	if text, isError := read(map[string]any{"name": "p--alpha", "file": "references/guide.md"}); isError || text != "guide\n" {
		t.Errorf("nested file = %q (%v)", text, isError)
	}
	if text, isError := read(map[string]any{"name": "alpha", "file": "references/guide.md"}); isError || text != "guide\n" {
		t.Errorf("by listed name = %q (%v), want the skill whose front matter is named alpha", text, isError)
	}
	if text, isError := read(map[string]any{"name": "big", "file": "exact.txt"}); isError || len(text) != maxCodexSkillFileBytes {
		t.Errorf("a file at the cap must be readable (%d bytes, %v)", len(text), isError)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"traversal in file", map[string]any{"name": "p--alpha", "file": "../mine/SKILL.md"}},
		{"deep traversal", map[string]any{"name": "p--alpha", "file": "references/../../../secret.txt"}},
		{"absolute file", map[string]any{"name": "p--alpha", "file": "/etc/passwd"}},
		{"backslash traversal", map[string]any{"name": "p--alpha", "file": `..\mine\SKILL.md`}},
		{"traversal in name", map[string]any{"name": "..", "file": "secret.txt"}},
		{"slash in name", map[string]any{"name": "p--alpha/references", "file": "guide.md"}},
		{"empty name", map[string]any{"name": ""}},
		{"dot name", map[string]any{"name": ".hidden"}},
		{"symlink escaping", map[string]any{"name": "p--alpha", "file": "escape.txt"}},
		{"symlink to a sibling", map[string]any{"name": "p--alpha", "file": "sibling.txt"}},
		{"symlinked directory", map[string]any{"name": "p--alpha", "file": "linkdir/SKILL.md"}},
		{"symlinked skill", map[string]any{"name": "linked-skill"}},
		{"marker", map[string]any{"name": "p--alpha", "file": miniMaxManagedPluginMarker}},
		{"marker alias", map[string]any{"name": "p--alpha", "file": "marker-alias"}},
		{"directory", map[string]any{"name": "p--alpha", "file": "references"}},
		{"too big", map[string]any{"name": "big", "file": "huge.txt"}},
		{"missing skill", map[string]any{"name": "ghost"}},
		{"missing file", map[string]any{"name": "p--alpha", "file": "nope.md"}},
		{"file below a file", map[string]any{"name": "p--alpha", "file": "SKILL.md/x"}},
		{"ambiguous listed name", map[string]any{"name": "twin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, isError := read(tc.args)
			if !isError {
				t.Fatalf("read %v = %q, want a tool error", tc.args, text)
			}
			if strings.Contains(text, "secret") && strings.Contains(text, "outside") {
				t.Fatalf("error leaked content: %q", text)
			}
		})
	}
	responses := runSkillsMCP(t, root, callFrame(1, "read_skill", "not an object"))
	if _, isError := toolText(t, responses[0]); !isError {
		t.Error("non-object arguments must be a tool error")
	}
}

func TestCodexSkillsMCPWithoutADirectory(t *testing.T) {
	for _, root := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		text, isError := toolText(t, runSkillsMCP(t, root, callFrame(1, "list_skills", nil))[0])
		if !isError {
			t.Errorf("root %q: list_skills = %q, want a tool error", root, text)
		}
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "skills")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, isError := toolText(t, runSkillsMCP(t, link, callFrame(1, "list_skills", nil))[0]); !isError {
		t.Error("a symlinked skills directory must be refused")
	}
}

func TestCodexSkillsMCPFrameLimit(t *testing.T) {
	root := writeSkillTree(t)
	huge := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", maxCodexSkillsMCPLine) + `"}}`
	responses := runSkillsMCP(t, root, huge, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(responses) != 2 || responses[0]["error"] == nil || responses[1]["result"] == nil {
		t.Fatalf("responses = %v, want an error for the oversized frame and a reply after it", responses)
	}
}

// AC-J7: refresh_skills is offered only with an agent to call, posts to the
// in-turn route, and words the agent's answer — a result or a refusal — for
// the model without failing the MCP.
func TestCodexSkillsMCPRefreshSkills(t *testing.T) {
	root := writeSkillTree(t)
	listFrame := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	names := func(response map[string]any) []string {
		var out []string
		for _, tool := range response["result"].(map[string]any)["tools"].([]any) {
			out = append(out, tool.(map[string]any)["name"].(string))
		}
		return out
	}
	if got := names(runSkillsMCP(t, root, listFrame)[0]); !reflect.DeepEqual(got, []string{"list_skills", "read_skill"}) {
		t.Errorf("tools without an agent = %v", got)
	}
	if response := runSkillsMCP(t, root, callFrame(2, "refresh_skills", map[string]any{}))[0]; response["error"] == nil {
		t.Errorf("refresh_skills without an agent = %v, want unknown tool", response)
	}

	var status int
	var body string
	var paths []string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer agent.Close()
	if got := names(runSkillsMCPWithAgent(t, root, agent.URL+"/", listFrame)[0]); !reflect.DeepEqual(got, []string{"list_skills", "read_skill", "refresh_skills"}) {
		t.Errorf("tools with an agent = %v", got)
	}
	call := func() (string, bool) {
		t.Helper()
		return toolText(t, runSkillsMCPWithAgent(t, root, agent.URL, callFrame(2, "refresh_skills", map[string]any{}))[0])
	}

	status, body = http.StatusOK, `{"state":"ready","revision":"0123456789abcdef","added":["p--new"],"updated":["p--alpha"],"removed":[]}`
	text, isError := call()
	if isError || !strings.Contains(text, "revision 0123456789ab") || !strings.Contains(text, "added: `p--new`; updated: `p--alpha`") ||
		!strings.Contains(text, "list_skills") || strings.Contains(text, "before following") {
		t.Errorf("refresh result = %q (error %v)", text, isError)
	}
	if !reflect.DeepEqual(paths, []string{"POST " + pluginRefreshTurnPath}) {
		t.Errorf("agent calls = %v", paths)
	}
	status, body = http.StatusOK, `{"state":"ready","added":[],"updated":[],"removed":[]}`
	if text, _ := call(); !strings.Contains(text, "nothing changed") {
		t.Errorf("no-change result = %q", text)
	}
	status, body = http.StatusOK, `{"state":"unconfigured","added":[],"updated":[],"removed":[]}`
	if text, _ := call(); !strings.Contains(text, "no plugin marketplace") {
		t.Errorf("unconfigured result = %q", text)
	}
	status, body = http.StatusTooManyRequests, errPluginRefreshTurnLimit.Error()+"\n"
	if text, isError := call(); !isError || !strings.Contains(text, "429") || !strings.Contains(text, "already refreshed") {
		t.Errorf("refused refresh = %q (error %v)", text, isError)
	}
	agent.Close()
	if text, isError := call(); !isError || !strings.Contains(text, "did not answer") {
		t.Errorf("unreachable agent = %q (error %v)", text, isError)
	}
}
