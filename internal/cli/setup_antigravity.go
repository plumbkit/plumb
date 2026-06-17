package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Antigravity reads MCP servers from the flat mcp_config.json files (an
// {"mcpServers": {...}} wrapper) under ~/.gemini — primarily the shared
// config/mcp_config.json, which serves both the Antigravity CLI and IDE
// (confirmed against Antigravity's MCP docs and a live install, 2026-06). The
// standalone mcp/<server>.json files earlier plumb builds wrote are NOT a
// user-authored format: Antigravity regenerates those mcp/ directories from the
// shared config, so a plumb entry written only there is ignored — the cause of
// the "plumb never connects in Antigravity" reports. The helpers here create or
// repoint plumb in every flat config Antigravity reads; doctor validates them.

// legacyAntigravityDirs are the ~/.gemini subdirectories whose flat
// mcp_config.json Antigravity reads MCP servers from. config/ is canonical — it
// serves both the CLI and the IDE; the rest cover per-surface layouts seen in
// the wild. antigravity-backup is deliberately excluded — it is a restore point
// setup writes, not a live config.
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

// ensureAntigravityFlatConfigs registers plumb at plumbBin in the flat
// mcp_config.json files Antigravity reads, creating a file (and its directory)
// when absent. The shared config/ file is canonical — it serves both the CLI and
// IDE — so it is always ensured; the per-surface dirs (antigravity-cli, etc.)
// are only written when they already exist, so plumb never materialises a
// surface for an Antigravity product that isn't installed. Each write preserves
// sibling servers and backs up an existing file. Returns the paths it changed.
func ensureAntigravityFlatConfigs(geminiBase, plumbBin string) []string {
	var changed []string
	for _, d := range legacyAntigravityDirs {
		dir := filepath.Join(geminiBase, d)
		if d != "config" {
			if _, err := os.Stat(dir); err != nil {
				continue
			}
		}
		p := filepath.Join(dir, "mcp_config.json")
		if added, err := ensureFlatAntigravityConfig(p, plumbBin); err == nil && added {
			changed = append(changed, p)
		}
	}
	return changed
}

// ensureFlatAntigravityConfig creates or repoints the plumb entry in one flat
// mcp_config.json (an {"mcpServers": {...}} wrapper), preserving every sibling
// server and backing up an existing file. It reuses the shared mergeServerEntry,
// so it returns added=false (no write) only when plumb is already registered at
// plumbBin. The binary comparison is symlink-aware, matching doctor.
func ensureFlatAntigravityConfig(path, plumbBin string) (bool, error) {
	added, _, err := mergeServerEntry(
		path, "mcpServers", readOrInitClaudeConfig, writeJSON,
		map[string]any{"command": plumbBin, "args": []string{"serve"}},
		func(existing map[string]any) bool {
			cur, ok := commandString(existing["command"])
			return ok && sameBinary(cur, plumbBin)
		},
	)
	return added, err
}
