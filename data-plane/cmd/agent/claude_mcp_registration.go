// Registration of the session MCP (AC-F6) into the file the Claude CLI reads
// MCP servers from.
//
// Observed on the pinned CLI (2.1.220, Dockerfile's CLAUDE_CODE_VERSION) by
// running it against both files: an `mcpServers` block in
// `$HOME/.claude/settings.json` is ignored — `claude mcp list` prints "No MCP
// servers configured" and every tool call comes back `No such tool available` —
// while the same block at `$HOME/.claude.json` connects. None of this is in the
// CLI's documentation, and the settings file takes the unread key without
// complaint, so the only signal is the missing tool.
//
// That file is the CLI's own: it rewrites it on every run, adding machine and
// account identifiers and per-project history. So the platform merges its one
// key in and leaves the rest byte-for-byte, rather than rendering the file the
// way it owns settings.json.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	claudeCLIConfigFile  = ".claude.json"
	claudeCLIMCPServers  = "mcpServers"
	maxClaudeCLIConfig   = 64 << 20
	claudeCLIConfigPerms = 0o600
)

// ensureClaudeMCPRegistration points the CLI config at this pod's session MCP,
// or clears the platform's entry when the workload type has no MCP surface. It
// runs on every boot including restores, because the helper pod's address is new
// on each round (AC-F4) and an archive always names the previous one.
func ensureClaudeMCPRegistration(homeDir string, tools toolSurface) error {
	path := filepath.Join(homeDir, claudeCLIConfigFile)
	config, err := readClaudeCLIConfig(path)
	if err != nil {
		return err
	}
	servers, err := decodeClaudeCLIMCPServers(config)
	if err != nil {
		return err
	}

	previous, had := servers[sessionMCPServerName]
	if tools.SessionMCP == "" {
		if !had {
			return nil
		}
		delete(servers, sessionMCPServerName)
	} else {
		want, err := json.Marshal(claudeMCPServer{Type: "http", URL: tools.SessionMCP})
		if err != nil {
			return err
		}
		if had && bytes.Equal(previous, want) {
			return nil
		}
		servers[sessionMCPServerName] = want
	}

	if len(servers) == 0 {
		delete(config, claudeCLIMCPServers)
	} else {
		encoded, err := json.Marshal(servers)
		if err != nil {
			return err
		}
		config[claudeCLIMCPServers] = encoded
	}
	return storeClaudeCLIConfig(homeDir, path, config)
}

// loadClaudeMCPRegistration reports the MCP servers the CLI config registers.
func loadClaudeMCPRegistration(homeDir string) (map[string]claudeMCPServer, error) {
	config, err := readClaudeCLIConfig(filepath.Join(homeDir, claudeCLIConfigFile))
	if err != nil {
		return nil, err
	}
	raw, err := decodeClaudeCLIMCPServers(config)
	if err != nil {
		return nil, err
	}
	servers := make(map[string]claudeMCPServer, len(raw))
	for name, encoded := range raw {
		var server claudeMCPServer
		if err := json.Unmarshal(encoded, &server); err != nil {
			// Only the platform's own entry has to be readable as one of these;
			// a session is free to register shapes this struct does not model.
			if name == sessionMCPServerName {
				return nil, fmt.Errorf("decode registered %s server: %w", name, err)
			}
			continue
		}
		servers[name] = server
	}
	return servers, nil
}

// readClaudeCLIConfig returns the config's top-level keys with their values
// unparsed. A missing file is an empty config — the CLI creates it on first run,
// and a fresh session HOME has not run it yet.
func readClaudeCLIConfig(path string) (map[string]json.RawMessage, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("claude CLI config is not a regular file")
	}
	if info.Size() > maxClaudeCLIConfig {
		return nil, fmt.Errorf("claude CLI config exceeds %d bytes", maxClaudeCLIConfig)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	config := map[string]json.RawMessage{}
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode claude CLI config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("claude CLI config has trailing JSON")
		}
		return nil, fmt.Errorf("decode claude CLI config trailer: %w", err)
	}
	return config, nil
}

func decodeClaudeCLIMCPServers(config map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	servers := map[string]json.RawMessage{}
	encoded, ok := config[claudeCLIMCPServers]
	if !ok {
		return servers, nil
	}
	if err := json.Unmarshal(encoded, &servers); err != nil {
		return nil, fmt.Errorf("decode claude CLI mcpServers: %w", err)
	}
	return servers, nil
}

func storeClaudeCLIConfig(homeDir, path string, config map[string]json.RawMessage) error {
	f, err := os.CreateTemp(homeDir, ".claude-config-*.json")
	if err != nil {
		return fmt.Errorf("create claude CLI config: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(claudeCLIConfigPerms); err != nil {
		f.Close()
		return err
	}
	if err := json.NewEncoder(f).Encode(config); err != nil {
		f.Close()
		return fmt.Errorf("encode claude CLI config: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync claude CLI config: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install claude CLI config: %w", err)
	}
	return nil
}
