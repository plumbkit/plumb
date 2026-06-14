package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// claudeCodeConfigPath returns the user-level Claude Code config path.
func claudeCodeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// homeRelConfigPath joins parts under the user's home directory. It is the
// common shape of the per-client config-path helpers below.
func homeRelConfigPath(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

// CursorConfigPath returns the global Cursor MCP config (~/.cursor/mcp.json),
// shared by the Cursor editor and the cursor-agent CLI.
func CursorConfigPath() (string, error) {
	return homeRelConfigPath(".cursor", "mcp.json")
}

// AugmentConfigPath returns the Augment Code (auggie CLI) settings path
// (~/.augment/settings.json).
func AugmentConfigPath() (string, error) {
	return homeRelConfigPath(".augment", "settings.json")
}

// QwenConfigPath returns the Qwen Code settings path (~/.qwen/settings.json).
// Qwen Code is a Gemini-CLI fork and shares its mcpServers JSON shape.
func QwenConfigPath() (string, error) {
	return homeRelConfigPath(".qwen", "settings.json")
}

// OpenCodeConfigPath returns the OpenCode global config
// (~/.config/opencode/opencode.json).
func OpenCodeConfigPath() (string, error) {
	return homeRelConfigPath(".config", "opencode", "opencode.json")
}

// CrushConfigPath returns the Crush global config (~/.config/crush/crush.json).
func CrushConfigPath() (string, error) {
	return homeRelConfigPath(".config", "crush", "crush.json")
}

// GooseConfigPath returns the Goose config (~/.config/goose/config.yaml).
func GooseConfigPath() (string, error) {
	return homeRelConfigPath(".config", "goose", "config.yaml")
}

// HermesConfigPath returns the Hermes Agent config (~/.hermes/config.yaml).
func HermesConfigPath() (string, error) {
	return homeRelConfigPath(".hermes", "config.yaml")
}

// AntigravityConfigPath returns the global Antigravity CLI MCP config
// (~/.gemini/antigravity-cli/mcp/plumb.json).
func AntigravityConfigPath() (string, error) {
	return homeRelConfigPath(".gemini", "antigravity-cli", "mcp", "plumb.json")
}

// AntigravityDesktopConfigPath returns the global Antigravity Desktop MCP config
// (~/.gemini/antigravity/mcp/plumb.json).
func AntigravityDesktopConfigPath() (string, error) {
	return homeRelConfigPath(".gemini", "antigravity", "mcp", "plumb.json")
}

// GeminiConfigPath returns the platform-specific path for Gemini CLI's
// settings.json. It does not check whether the file exists.
func GeminiConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "settings.json"), nil
}

// CodexConfigPath returns the Codex config.toml path. CODEX_HOME overrides the
// default home-relative config directory.
func CodexConfigPath() (string, error) {
	if codexHome := os.Getenv("CODEX_HOME"); codexHome != "" {
		return filepath.Join(codexHome, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// backupFile copies src to src.<timestamp>.bak in the same directory.
func backupFile(src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	dst := src + "." + stamp + ".bak"
	return os.WriteFile(dst, data, 0o600) //nolint:gosec // G703: dst is derived from OS-native config path helpers (UserHomeDir, Executable), not user input
}

// claudeDesktopConfigPath returns the platform-specific path for
// claude_desktop_config.json. It does not check whether the file exists.
func claudeDesktopConfigPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("APPDATA environment variable not set")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	default:
		// Unofficial Linux path — Claude Desktop isn't fully supported there yet.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

// readOrInitClaudeConfig reads cfgPath as JSON into a generic map.
// isNew is true when the file did not exist (empty map returned for first run).
// Any read or parse error is returned — never silently discarded.
func readOrInitClaudeConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as JSON: %w — will not overwrite", path, err)
	}
	return m, false, nil
}

// readOrInitCodexConfig reads cfgPath as TOML into a generic map.
// isNew is true when the file did not exist.
func readOrInitCodexConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as TOML: %w — will not overwrite", path, err)
	}
	return m, false, nil
}

// writeJSON writes m to path as indented JSON, creating the file if needed.
// It writes to a temp file in the same directory and renames atomically.
func writeJSON(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".plumb_setup_*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// writeTOML writes m to path as TOML, creating the file if needed.
// It writes to a temp file in the same directory and renames atomically.
func writeTOML(path string, m map[string]any) error {
	data, err := toml.Marshal(m)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".plumb_setup_*.toml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// readOrInitYAMLConfig reads cfgPath as YAML into a generic map.
// isNew is true when the file did not exist.
func readOrInitYAMLConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as YAML: %w — will not overwrite", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, false, nil
}

// writeYAML writes m to path as YAML, creating the file if needed.
// It writes to a temp file in the same directory and renames atomically.
func writeYAML(path string, m map[string]any) error {
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".plumb_setup_*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// mergeServerEntry is the shared, format-agnostic merge used by every
// `plumb setup <client>` command. It reads cfgPath via read, finds (or creates)
// the server map under serversKey, and inserts entry under the "plumb" key —
// preserving every other entry. read/write select the serialisation (JSON, TOML,
// or YAML); same reports whether an existing plumb entry already points at this
// binary, making the operation idempotent.
//
// Returns added=false (no write) when plumb is already registered identically.
// preserved lists the names of the other servers that were kept.
func mergeServerEntry(
	cfgPath, serversKey string,
	read func(string) (map[string]any, bool, error),
	write func(string, map[string]any) error,
	entry map[string]any,
	same func(existing map[string]any) bool,
) (added bool, preserved []string, err error) {
	cfg, isNew, err := read(cfgPath)
	if err != nil {
		return false, nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}

	if cfg[serversKey] == nil {
		cfg[serversKey] = map[string]any{}
	}
	servers, ok := cfg[serversKey].(map[string]any)
	if !ok {
		return false, nil, fmt.Errorf("%s in %s is not an object — cannot safely modify it", serversKey, cfgPath)
	}

	for name := range servers {
		if name != "plumb" {
			preserved = append(preserved, name)
		}
	}
	sort.Strings(preserved)

	existing, _ := servers["plumb"].(map[string]any)
	if existing != nil && same(existing) {
		return false, preserved, nil
	}

	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}

	// Merge the canonical fields onto any existing plumb entry rather than
	// replacing it wholesale, so user-added keys survive a re-register or a
	// `plumb setup --all` repoint — most importantly Codex's per-tool
	// [mcp_servers.plumb.tools.*] approval tables, which a replace would drop.
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range entry {
		existing[k] = v
	}
	servers["plumb"] = existing

	if err := write(cfgPath, cfg); err != nil {
		return false, nil, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, preserved, nil
}

// claudeSkillsDir returns the user-level Claude Code skills directory
// (~/.claude/skills). It does not create the directory.
func claudeSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// installSkill writes content to <skillsDir>/<name>/SKILL.md, creating
// the directory if needed. Returns "installed", "updated", or "unchanged".
// If the file already exists with different content it is backed up first.
// Atomic write via temp-file + rename.
func installSkill(skillsDir, name, content string) (string, error) {
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating skill directory: %w", err)
	}

	dst := filepath.Join(dir, "SKILL.md")
	existing, readErr := os.ReadFile(dst)

	switch {
	case readErr == nil && string(existing) == content:
		return "unchanged", nil
	case readErr == nil:
		// File exists but content differs — back up before overwriting.
		if err := backupFile(dst); err != nil {
			return "", fmt.Errorf("backing up %s: %w", dst, err)
		}
	case os.IsNotExist(readErr):
		// File does not exist — fresh install, no backup needed.
	default:
		return "", fmt.Errorf("reading %s: %w", dst, readErr)
	}

	tmp, err := os.CreateTemp(dir, ".plumb_skill_*.md")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return "", fmt.Errorf("installing skill: %w", err)
	}

	if os.IsNotExist(readErr) {
		return "installed", nil
	}
	return "updated", nil
}

func stringSliceEqual(got any, want []string) bool {
	gotSlice, ok := got.([]any)
	if !ok {
		return false
	}
	if len(gotSlice) != len(want) {
		return false
	}
	for i, gotItem := range gotSlice {
		if gotItem != want[i] { //nolint:gosec // G602: len(gotSlice)==len(want) is asserted by the length guard above
			return false
		}
	}
	return true
}
