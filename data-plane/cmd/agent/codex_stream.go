package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// codexStreamProjector turns codex-runner's stdout — one JSON record per line,
// the main thread's events in `codex exec`'s format plus the runner's own
// notices (sub-agent progress among them, AC-J8) — into the
// user-facing text stream (AC-J2), the counterpart of claudeStreamProjector.
// What the user sees is the conversation and what happens around it: the
// runner's notices (the ChatGPT sign-in steps of AC-J3, the kill switch), each
// finished assistant message, one line per K3s MCP or skills call with its
// failure if it fails, and a failed turn. Reasoning, usage and every other
// record are dropped, since tool records carry tool output and could carry
// anything. A line that is not JSON at all is a diagnostic and is passed
// through, as the other projectors do.
type codexStreamProjector struct {
	dst io.Writer
	buf []byte
	// discarding is set while the rest of an oversized line is skipped.
	discarding bool
}

const (
	// codexMaxRecordBytes bounds one buffered line. A record past it is
	// dropped whole rather than cut, because half a JSON record is not one.
	codexMaxRecordBytes      = 8 << 20
	codexOversizedRecordLine = "[session-platform: dropped an oversized codex record]\n"
	// codexMaxDetailBytes bounds an error message or tool name, which come
	// from a remote service and are shown on one line.
	codexMaxDetailBytes = 4 << 10

	codexNoticeType = "session_platform.notice"
)

func newCodexStreamProjector(dst io.Writer) *codexStreamProjector {
	return &codexStreamProjector{dst: dst}
}

func (p *codexStreamProjector) Write(chunk []byte) (int, error) {
	originalLen := len(chunk)
	for len(chunk) > 0 {
		newline := bytes.IndexByte(chunk, '\n')
		if p.discarding {
			if newline < 0 {
				return originalLen, nil
			}
			p.discarding = false
			chunk = chunk[newline+1:]
			continue
		}
		if newline < 0 {
			if len(p.buf)+len(chunk) > codexMaxRecordBytes {
				p.buf = nil
				p.discarding = true
				if _, err := io.WriteString(p.dst, codexOversizedRecordLine); err != nil {
					return 0, err
				}
				return originalLen, nil
			}
			p.buf = append(p.buf, chunk...)
			return originalLen, nil
		}
		if len(p.buf)+newline > codexMaxRecordBytes {
			p.buf = nil
			if _, err := io.WriteString(p.dst, codexOversizedRecordLine); err != nil {
				return 0, err
			}
			chunk = chunk[newline+1:]
			continue
		}
		line := append(p.buf, chunk[:newline]...)
		p.buf = nil
		chunk = chunk[newline+1:]
		if err := p.projectLine(line); err != nil {
			return 0, err
		}
	}
	return originalLen, nil
}

// Close projects a final line the runner did not terminate, which is what a
// killed runner leaves behind.
func (p *codexStreamProjector) Close() error {
	line := p.buf
	p.buf = nil
	p.discarding = false
	if len(line) == 0 {
		return nil
	}
	return p.projectLine(line)
}

type codexStreamRecord struct {
	Type    string          `json:"type"`
	Text    json.RawMessage `json:"text"`
	Message json.RawMessage `json:"message"`
	Error   json.RawMessage `json:"error"`
	Item    json.RawMessage `json:"item"`
}

type codexStreamItem struct {
	Type   string          `json:"type"`
	Text   json.RawMessage `json:"text"`
	Server json.RawMessage `json:"server"`
	Tool   json.RawMessage `json:"tool"`
	Status json.RawMessage `json:"status"`
	Error  json.RawMessage `json:"error"`
}

func (p *codexStreamProjector) projectLine(line []byte) error {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	if !json.Valid(line) {
		return p.writeLine(string(line))
	}
	var record codexStreamRecord
	if json.Unmarshal(line, &record) != nil {
		return nil
	}
	switch record.Type {
	case codexNoticeType:
		if text := jsonString(record.Text); text != "" {
			return p.writeLine(text)
		}
	case "turn.failed":
		return p.writeError(errorMessage(record.Error))
	case "error":
		return p.writeError(jsonString(record.Message))
	case "item.started", "item.completed":
		var item codexStreamItem
		if len(record.Item) == 0 || json.Unmarshal(record.Item, &item) != nil {
			return nil
		}
		return p.projectItem(record.Type, item)
	}
	return nil
}

func (p *codexStreamProjector) projectItem(event string, item codexStreamItem) error {
	switch item.Type {
	case "agent_message":
		if event != "item.completed" {
			return nil
		}
		if text := jsonString(item.Text); text != "" {
			return p.writeLine(text)
		}
	case "mcp_tool_call":
		name := oneLine(jsonString(item.Server)) + "." + oneLine(jsonString(item.Tool))
		if event == "item.started" {
			return p.writeLine("· " + name)
		}
		message := errorMessage(item.Error)
		if message == "" && jsonString(item.Status) != "failed" {
			return nil
		}
		if message == "" {
			message = "failed"
		}
		return p.writeLine("error: " + name + ": " + oneLine(message))
	}
	return nil
}

func (p *codexStreamProjector) writeError(message string) error {
	if message == "" {
		message = "codex reported an error"
	}
	return p.writeLine("error: " + oneLine(message))
}

func (p *codexStreamProjector) writeLine(text string) error {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	_, err := io.WriteString(p.dst, text)
	return err
}

// jsonString is raw's string value, or empty for anything that is not one.
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// errorMessage reads the runner's {"message": ...} error shape, and a bare string
// as well.
func errorMessage(raw json.RawMessage) string {
	if s := jsonString(raw); s != "" {
		return s
	}
	var shaped struct {
		Message json.RawMessage `json:"message"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &shaped) != nil {
		return ""
	}
	return jsonString(shaped.Message)
}

// oneLine keeps a remote detail on its own line and within codexMaxDetailBytes,
// cutting only at a rune boundary.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
	if len(s) <= codexMaxDetailBytes {
		return s
	}
	cut := validUTF8PrefixAtMost([]byte(s), codexMaxDetailBytes)
	return s[:cut] + "…"
}
