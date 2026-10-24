// Registration of the MCP servers a minimax-code session gets (AC-G5, AC-G6)
// into mcode's own server list, ~/.minimax/mcp.json, rather than into the
// plugins the marketplace ships them in.
//
// Observed on the pinned mcode (0.6.12, Dockerfile's MINIMAX_CODE_VERSION) on
// 2026-10-24: a plugin's MCP server is discovered in the background, and a turn
// takes whatever inventory is ready when it starts — none at all when
// `tools/list` has not answered yet. One `exec` is one process and one turn, so
// every prompt races its own discovery. Against loopback the discovery wins;
// with a few hundred milliseconds of round trips it loses, and on the cluster
// the model was sent no MCP tool on any turn, with no error anywhere. A server
// in mcp.json is listed by the turn itself, which waits for it (for http, up to
// a fixed 2 s).
//
// The file is in HOME, so it is archived and the user or mcode may write it
// too. The platform merges its entries in by name and tags each one; a tagged
// entry this boot does not register is a previous pod's, and goes.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
)

const (
	miniMaxMCPConfigFile  = "mcp.json"
	miniMaxManagedMCPKey  = "sessionPlatformManaged"
	maxMiniMaxMCPConfig   = 16 << 20
	miniMaxMCPConfigPerms = 0o600
)

func miniMaxMCPConfigPath(homeDir string) string {
	return filepath.Join(homeDir, ".minimax", miniMaxMCPConfigFile)
}

// ensureMiniMaxMCPRegistration makes the platform's entries in mcp.json exactly
// servers, leaving every entry it did not tag alone.
func ensureMiniMaxMCPRegistration(homeDir string, servers map[string]map[string]any) error {
	path := miniMaxMCPConfigPath(homeDir)
	doc, err := readMiniMaxMCPConfig(path)
	if err != nil {
		return err
	}
	registered := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &registered); err != nil {
			return fmt.Errorf("decode mcode mcpServers: %w", err)
		}
	}

	changed := false
	for name, raw := range registered {
		if _, ours := servers[name]; !ours && isMiniMaxManagedMCPServer(raw) {
			delete(registered, name)
			changed = true
		}
	}
	for name, server := range servers {
		entry := maps.Clone(server)
		entry[miniMaxManagedMCPKey] = true
		want, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if bytes.Equal(registered[name], want) {
			continue
		}
		registered[name] = want
		changed = true
	}
	if !changed {
		return nil
	}

	encoded, err := json.Marshal(registered)
	if err != nil {
		return err
	}
	doc["mcpServers"] = encoded
	return storeMiniMaxMCPConfig(path, doc)
}

func isMiniMaxManagedMCPServer(raw json.RawMessage) bool {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil {
		return false
	}
	var managed bool
	return json.Unmarshal(entry[miniMaxManagedMCPKey], &managed) == nil && managed
}

// readMiniMaxMCPConfig returns the file's top-level keys with their values
// unparsed. A missing file is an empty one: mcode creates it only when a server
// is first added, and a fresh session HOME has none.
func readMiniMaxMCPConfig(path string) (map[string]json.RawMessage, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("mcode MCP config is not a regular file")
	}
	if info.Size() > maxMiniMaxMCPConfig {
		return nil, fmt.Errorf("mcode MCP config exceeds %d bytes", maxMiniMaxMCPConfig)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc := map[string]json.RawMessage{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode mcode MCP config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("mcode MCP config has trailing data")
	}
	if doc == nil {
		doc = map[string]json.RawMessage{}
	}
	return doc, nil
}

func storeMiniMaxMCPConfig(path string, doc map[string]json.RawMessage) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create mcode data directory: %w", err)
	}
	content, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode mcode MCP config: %w", err)
	}
	f, err := os.CreateTemp(dir, ".mcp-*.json")
	if err != nil {
		return fmt.Errorf("create mcode MCP config: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(miniMaxMCPConfigPerms); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(content, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("write mcode MCP config: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync mcode MCP config: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install mcode MCP config: %w", err)
	}
	return nil
}
