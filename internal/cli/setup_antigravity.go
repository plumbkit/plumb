package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Antigravity reads MCP servers from more than the standalone mcp/plumb.json files
// that `plumb setup antigravity` writes: older builds and the shared Gemini config
// still consult flat mcp_config.json files (an {"mcpServers": {...}} wrapper) under
// several ~/.gemini subdirectories. A plumb binary that has moved or been rebuilt
// goes stale in those files, and the standalone-only setup never touches them — so
// Antigravity keeps launching a path that no longer exists. The helpers here let
// setup/--all repoint, and doctor detect, a stale plumb entry in every legacy file
// Antigravity actually reads.

// legacyAntigravityDirs are the ~/.gemini subdirectories whose flat
// mcp_config.json Antigravity reads MCP servers from. antigravity-backup is
// deliberately excluded — it is a restore point setup writes, not a live config.
var legacyAntigravityDirs = []string{"config", "antigravity-cli", "antigravity-ide", "antigravity"}

// legacyAntigravityConfigPaths returns the flat mcp_config.json paths under the
// given ~/.gemini base directory. geminiBase is derived from a known standalone
// config path so the set tracks wherever the standalone files live (real home in
// production, a tempdir under test) rather than re-resolving the home directory.
func legacyAntigravityConfigPaths(geminiBase string) []string {
	paths := make([]string, 0, len(legacyAntigravityDirs))
	for _, d := range legacyAntigravityDirs {
		paths = append(paths, filepath.Join(geminiBase, d, "mcp_config.json"))
	}
	return paths
}

// geminiBaseFromStandalone recovers the ~/.gemini base from a standalone config
// path like <base>/antigravity-cli/mcp/plumb.json (three levels up).
func geminiBaseFromStandalone(cfgPath string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(cfgPath)))
}

// readLegacyAntigravityCommand reads the plumb launch binary from a flat
// mcp_config.json (mcpServers.plumb.command). ok is false when the file is absent,
// unparseable, or registers no plumb server.
func readLegacyAntigravityCommand(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", false
	}
	return registeredCommand(cfg, "mcpServers", "command")
}

// reconcileLegacyAntigravityConfigs repoints a stale plumb entry at plumbBin in
// every existing flat mcp_config.json under geminiBase that already registers
// plumb. It never creates a legacy file and never adds plumb to one that lacks it
// — it only fixes a moved or rebuilt binary where Antigravity still reads it.
// Returns the paths it changed.
func reconcileLegacyAntigravityConfigs(geminiBase, plumbBin string) []string {
	var changed []string
	for _, p := range legacyAntigravityConfigPaths(geminiBase) {
		if updateLegacyAntigravityConfig(p, plumbBin) {
			changed = append(changed, p)
		}
	}
	return changed
}

// updateLegacyAntigravityConfig repoints the plumb command in one flat
// mcp_config.json to plumbBin, preserving every other field. It returns false
// (no write) when the file is absent/unparseable, registers no plumb server, or
// already launches plumbBin.
func updateLegacyAntigravityConfig(path, plumbBin string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return false
	}
	entry, ok := servers["plumb"].(map[string]any)
	if !ok {
		return false
	}
	if cur, ok := commandString(entry["command"]); ok && sameBinary(cur, plumbBin) {
		return false
	}
	entry["command"] = plumbBin
	if _, hasArgs := entry["args"]; !hasArgs {
		entry["args"] = []string{"serve"}
	}
	return writeJSON(path, cfg) == nil
}
