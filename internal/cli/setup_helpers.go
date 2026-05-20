package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// claudeCodeConfigPath returns the user-level Claude Code config path.
func claudeCodeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
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
