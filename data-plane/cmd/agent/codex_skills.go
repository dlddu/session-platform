package main

import (
	"encoding/base64"
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

// A codex session gets the skills of the platform's marketplace plugins and
// nothing else a plugin carries (AC-J5). The k3s-mcp-proxy sidecar clones the
// marketplace — this container has no GitHub credential — and serves it at
// /plugin-seed in minimax-code's bundle format (minimax_marketplace.go). codex
// discovers user skills in $HOME/.agents/skills/<name>/SKILL.md, so each
// seeded skill is written there as <plugin>--<skill>, tagged with the same
// marker minimax-code uses. Only tagged directories are ever replaced or
// removed: a skill the user installed by hand survives every sync, and HOME is
// archived, so the sync is redone on every boot and restore.
const codexSkillNameSeparator = "--"

// codexSidecar is what the K3s MCP proxy sidecar offered this boot.
type codexSidecar struct {
	// configured is the proxy's own report that it holds a K3s MCP token.
	configured bool
	plugins    []miniMaxSeedPlugin
	// revision is the marketplace commit the plugins were read from.
	revision string
}

// codexSkill is one skill directory: its files keyed by slash path.
type codexSkill map[string][]byte

func codexSkillsDir(homeDir string) string {
	return filepath.Join(homeDir, ".agents", "skills")
}

// fetchCodexSidecar waits up to wait for the sidecar to answer its health
// route, and when it holds a token, for its marketplace bootstrap to finish.
// It returns whatever it learned before an error, so a sidecar whose seed
// never arrives still gives the session its K3s MCP.
func fetchCodexSidecar(proxyURL string, wait time.Duration) (codexSidecar, error) {
	client := &http.Client{Timeout: miniMaxSeedTimeout}
	return fetchCodexSidecarWith(client, proxyURL, time.Now().Add(wait), miniMaxSeedPollInterval)
}

func fetchCodexSidecarWith(client *http.Client, proxyURL string, deadline time.Time, interval time.Duration) (codexSidecar, error) {
	base := strings.TrimRight(proxyURL, "/")
	var sidecar codexSidecar
	for {
		configured, err := fetchCodexSidecarHealth(client, base+k3sMCPProxyHealthz)
		if err == nil {
			sidecar.configured = configured
			break
		}
		if time.Now().After(deadline) {
			return sidecar, fmt.Errorf("K3s MCP proxy health: %w", err)
		}
		time.Sleep(interval)
	}
	if !sidecar.configured {
		return sidecar, nil
	}
	for {
		bundle, retry, err := fetchMiniMaxSeedOnce(client, base+miniMaxPluginSeedPath)
		if err == nil {
			if bundle.State == miniMaxSeedStateFailed {
				return sidecar, fmt.Errorf("plugin marketplace bootstrap failed: %s", bundle.Error)
			}
			if bundle.State == miniMaxSeedStateReady {
				sidecar.plugins = bundle.Plugins
				sidecar.revision = bundle.Revision
			}
			return sidecar, nil
		}
		if !retry || time.Now().After(deadline) {
			return sidecar, fmt.Errorf("K3s MCP proxy plugin seed: %w", err)
		}
		time.Sleep(interval)
	}
}

func fetchCodexSidecarHealth(client *http.Client, healthURL string) (bool, error) {
	resp, err := client.Get(healthURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %d", resp.StatusCode)
	}
	var health struct {
		Workload   string `json:"workload"`
		Configured bool   `json:"configured"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&health); err != nil {
		return false, fmt.Errorf("decode health: %w", err)
	}
	if health.Workload != workloadK3SMCPProxy {
		return false, fmt.Errorf("health reports workload %q, not %s", health.Workload, workloadK3SMCPProxy)
	}
	return health.Configured, nil
}

// codexSkillsFromSeed extracts the skills of every bundled plugin. A plugin
// whose bundle is malformed costs the session that plugin's skills, not the
// others, and not the pod.
func codexSkillsFromSeed(plugins []miniMaxSeedPlugin, logger *slog.Logger) map[string]codexSkill {
	skills := map[string]codexSkill{}
	for _, plugin := range plugins {
		found, err := codexPluginSkills(plugin)
		if err != nil {
			logger.Warn("skipping the skills of a seeded plugin", "plugin", plugin.Name, "err", err)
			continue
		}
		for name, skill := range found {
			if _, dup := skills[name]; dup {
				logger.Warn("skipping a duplicate seeded skill", "skill", name)
				continue
			}
			skills[name] = skill
		}
	}
	return skills
}

// codexPluginSkills reads the skills a plugin's manifest names. A `skills`
// entry is either one skill (a directory holding SKILL.md) or a directory of
// skills, as Claude plugins allow; with no entry, the conventional skills/
// directory is used.
func codexPluginSkills(plugin miniMaxSeedPlugin) (map[string]codexSkill, error) {
	if !validMiniMaxPluginName(plugin.Name) || strings.Contains(plugin.Name, codexSkillNameSeparator) {
		return nil, fmt.Errorf("plugin name %q cannot name a skill directory", plugin.Name)
	}
	files := map[string][]byte{}
	for rel, encoded := range plugin.Files {
		clean := path.Clean(rel)
		if rel == "" || path.IsAbs(rel) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("file %q is outside the plugin", rel)
		}
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("file %q: %w", rel, err)
		}
		files[clean] = content
	}
	roots := []string{"skills"}
	if manifest, ok := files[".claude-plugin/plugin.json"]; ok {
		var decoded struct {
			Skills any `json:"skills"`
		}
		if err := json.Unmarshal(manifest, &decoded); err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		if decoded.Skills != nil {
			declared, err := pathList(decoded.Skills)
			if err != nil {
				return nil, fmt.Errorf("skills: %w", err)
			}
			roots = roots[:0]
			for _, root := range declared {
				clean := path.Clean(strings.TrimPrefix(root, "./"))
				if path.IsAbs(root) || clean == ".." || strings.HasPrefix(clean, "../") {
					return nil, fmt.Errorf("skills %q is outside the plugin", root)
				}
				roots = append(roots, clean)
			}
		}
	}

	skills := map[string]codexSkill{}
	for _, root := range roots {
		dirs := map[string]string{}
		if _, ok := files[path.Join(root, "SKILL.md")]; ok && root != "." {
			dirs[path.Base(root)] = root
		} else {
			prefix := root + "/"
			if root == "." {
				prefix = ""
			}
			for rel := range files {
				rest, ok := strings.CutPrefix(rel, prefix)
				if !ok {
					continue
				}
				if name, file, ok := strings.Cut(rest, "/"); ok && file == "SKILL.md" {
					dirs[name] = path.Join(root, name)
				}
			}
		}
		for skillName, dir := range dirs {
			if !validCodexSkillSegment(skillName) {
				return nil, fmt.Errorf("skill directory %q cannot name a skill", dir)
			}
			name := plugin.Name + codexSkillNameSeparator + skillName
			if _, dup := skills[name]; dup {
				continue
			}
			skill := codexSkill{}
			for rel, content := range files {
				if inside, ok := strings.CutPrefix(rel, dir+"/"); ok {
					skill[inside] = content
				}
			}
			skills[name] = skill
		}
	}
	return skills, nil
}

func validCodexSkillSegment(name string) bool {
	return validMiniMaxPluginName(name) && !strings.HasPrefix(name, ".")
}

// codexSkillChanges is what one sync did to the managed skills, by directory
// name. A skill whose files were already what the seed carries is in none.
type codexSkillChanges struct {
	Added, Updated, Removed []string
}

func (c codexSkillChanges) empty() bool {
	return len(c.Added)+len(c.Updated)+len(c.Removed) == 0
}

// syncCodexSkills makes the managed skill directories under skillsDir exactly
// skills, leaving every untagged directory alone, and reports what it changed.
func syncCodexSkills(skillsDir string, skills map[string]codexSkill, logger *slog.Logger) (codexSkillChanges, error) {
	var changes codexSkillChanges
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return changes, fmt.Errorf("create codex skills directory: %w", err)
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return changes, fmt.Errorf("read codex skills directory: %w", err)
	}
	userOwned := map[string]bool{}
	installed := map[string]bool{}
	for _, entry := range entries {
		dir := filepath.Join(skillsDir, entry.Name())
		if !entry.IsDir() {
			userOwned[entry.Name()] = true
			continue
		}
		managed, err := isCodexManagedSkill(dir)
		if err != nil {
			return changes, err
		}
		if !managed {
			userOwned[entry.Name()] = true
			continue
		}
		if _, keep := skills[entry.Name()]; keep {
			installed[entry.Name()] = true
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return changes, fmt.Errorf("remove stale codex skill %s: %w", entry.Name(), err)
		}
		changes.Removed = append(changes.Removed, entry.Name())
	}
	names := make([]string, 0, len(skills))
	for name := range skills {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if userOwned[name] {
			logger.Warn("a seeded skill is shadowed by one the user installed; keeping the user's", "skill", name)
			continue
		}
		dir := filepath.Join(skillsDir, name)
		if installed[name] {
			same, err := codexSkillMatches(dir, skills[name])
			if err != nil {
				return changes, err
			}
			if same {
				continue
			}
		}
		if err := installCodexSkill(dir, skills[name]); err != nil {
			return changes, fmt.Errorf("install codex skill %s: %w", name, err)
		}
		if installed[name] {
			changes.Updated = append(changes.Updated, name)
		} else {
			changes.Added = append(changes.Added, name)
		}
	}
	sort.Strings(changes.Removed)
	return changes, nil
}

// codexSkillMatches reports whether the installed dir holds exactly skill's
// files, the marker aside.
func codexSkillMatches(dir string, skill codexSkill) (bool, error) {
	found := 0
	same := true
	err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == miniMaxManagedPluginMarker {
			return nil
		}
		want, ok := skill[filepath.ToSlash(rel)]
		if !ok || !entry.Type().IsRegular() {
			same = false
			return filepath.SkipAll
		}
		got, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(got) != string(want) {
			same = false
			return filepath.SkipAll
		}
		found++
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("compare codex skill %s: %w", filepath.Base(dir), err)
	}
	return same && found == len(skill), nil
}

func isCodexManagedSkill(dir string) (bool, error) {
	info, err := os.Lstat(filepath.Join(dir, miniMaxManagedPluginMarker))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect codex skill %s: %w", filepath.Base(dir), err)
	}
	return info.Mode().IsRegular(), nil
}

// installCodexSkill replaces dir with the skill. The marker is written first,
// so a sync interrupted halfway leaves a directory the next sync still owns.
func installCodexSkill(dir string, skill codexSkill) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, miniMaxManagedPluginMarker), nil, 0o600); err != nil {
		return err
	}
	for rel, content := range skill {
		target, err := insideTree(dir, filepath.FromSlash(rel))
		if err != nil || target == dir || filepath.Base(target) == miniMaxManagedPluginMarker {
			return fmt.Errorf("file %q: invalid path", rel)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}
