package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeMiniMaxMCPConfig(t *testing.T, home, content string) {
	t.Helper()
	path := miniMaxMCPConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The user's servers and the file's other keys survive; an entry a previous
// pod tagged and this boot does not register goes; the platform's names are
// the platform's even when the user wrote one first.
func TestMiniMaxMCPRegistrationMergesByTag(t *testing.T) {
	home := t.TempDir()
	writeMiniMaxMCPConfig(t, home, `{"version":3,"mcpServers":{
		"mine":{"type":"http","url":"http://example.test/mcp"},
		"retired":{"type":"http","url":"http://10.0.0.9:8093/old","sessionPlatformManaged":true},
		"k3s":{"type":"http","url":"http://user-wrote-this/mcp"}}}`)

	err := ensureMiniMaxMCPRegistration(home, map[string]map[string]any{
		sessionMCPServerName: {"type": "http", "url": "http://10.1.2.3:8092/mcp"},
		"k3s":                {"type": "http", "url": "http://10.1.2.3:8093/mcp"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	servers := registeredMiniMaxServers(t, home)
	if servers["mine"]["url"] != "http://example.test/mcp" {
		t.Errorf("user server = %v, want it untouched", servers["mine"])
	}
	if _, ok := servers["retired"]; ok {
		t.Error("a previous pod's server survived")
	}
	if servers["k3s"]["url"] != "http://10.1.2.3:8093/mcp" || servers[sessionMCPServerName][miniMaxManagedMCPKey] != true {
		t.Errorf("platform servers = %v, want this pod's tagged entries", servers)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(t, miniMaxMCPConfigPath(home))), &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["version"]) != "3" {
		t.Errorf("version = %s, want the file's other keys kept", doc["version"])
	}
	if info, err := os.Stat(miniMaxMCPConfigPath(home)); err != nil || info.Mode().Perm() != miniMaxMCPConfigPerms {
		t.Errorf("mcp.json mode = %v (%v), want %o", info.Mode().Perm(), err, miniMaxMCPConfigPerms)
	}
}

// Nothing to register and nothing tagged leaves the file as it was — or
// absent — rather than rewritten.
func TestMiniMaxMCPRegistrationLeavesAnUnchangedFileAlone(t *testing.T) {
	home := t.TempDir()
	if err := ensureMiniMaxMCPRegistration(home, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := os.Stat(miniMaxMCPConfigPath(home)); !os.IsNotExist(err) {
		t.Errorf("mcp.json was created with nothing to register: %v", err)
	}

	const original = `{"mcpServers":{"mine":{"type":"http","url":"http://example.test/mcp"}}}`
	writeMiniMaxMCPConfig(t, home, original)
	if err := ensureMiniMaxMCPRegistration(home, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := readFile(t, miniMaxMCPConfigPath(home)); got != original {
		t.Errorf("mcp.json = %s, want it byte-for-byte", got)
	}
}

func TestMiniMaxMCPRegistrationRefusesAnUnreadableFile(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, home string){
		"not JSON":       func(t *testing.T, home string) { writeMiniMaxMCPConfig(t, home, `{`) },
		"trailing data":  func(t *testing.T, home string) { writeMiniMaxMCPConfig(t, home, `{} {}`) },
		"servers object": func(t *testing.T, home string) { writeMiniMaxMCPConfig(t, home, `{"mcpServers":[]}`) },
		"symlink": func(t *testing.T, home string) {
			writeMiniMaxMCPConfig(t, home, `{}`)
			path := miniMaxMCPConfigPath(home)
			if err := os.Rename(path, path+".real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".real", path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			setup(t, home)
			err := ensureMiniMaxMCPRegistration(home, map[string]map[string]any{
				sessionMCPServerName: {"type": "http", "url": "http://10.1.2.3:8092/mcp"},
			})
			if err == nil {
				t.Fatal("registered into a file it cannot read as mcode's server list")
			}
		})
	}
}
