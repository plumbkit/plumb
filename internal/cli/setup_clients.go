package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// setupTarget describes one `plumb setup <client>` command in a data-driven way:
// the lowercase subcommand (use), the human name shown in messages, the config
// path resolver, the format-specific merge helper, and an extractor that reads
// back the binary the config currently launches plumb with. The merge helpers all
// funnel through mergeServerEntry, so each client differs only in path, key,
// entry shape, and serialisation. extractFn powers `plumb doctor`'s mismatched-
// binary detection and `plumb setup --all`'s "is plumb already registered?" gate.
// pathsFn is optional: when set it overrides pathFn for `plumb setup --all`,
// resolving every config path to manage instead of just one — currently only
// Claude Desktop sets it, for its heuristic sibling-profile discovery
// (claudeDesktopConfigPaths). pathFn stays the single source of truth for
// `plumb doctor`'s canonical-path check.
type setupTarget struct {
	use       string
	name      string
	pathFn    func() (string, error)
	pathsFn   func() ([]string, error)
	intoFn    func(cfgPath, plumbBin string) (added bool, preserved []string, err error)
	extractFn func(cfgPath string) (binPath string, registered bool, err error)
}

// claudeDesktopCommandExtractor reads the plumb launch binary back from a
// Claude Desktop-shaped mcpServers JSON config. Shared by the claude-desktop
// setupTarget and the extra-profile doctor check (checkClaudeDesktopExtraProfiles).
var claudeDesktopCommandExtractor = mapCommandExtractor(readOrInitClaudeConfig, "mcpServers", "command")

// extraSetupTargets are the command-line MCP-client agents that consume external
// MCP servers and therefore make sense as `plumb setup` targets. The first three
// share Claude Desktop's plain `mcpServers` JSON shape; the rest use a distinct
// key, entry shape, or serialisation (see each setup*Into helper).
var extraSetupTargets = []setupTarget{
	{use: "cursor", name: "Cursor", pathFn: CursorConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
	{use: "augment", name: "Augment Code", pathFn: AugmentConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
	{use: "qwen", name: "Qwen Code", pathFn: QwenConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
	{use: "antigravity", name: "Antigravity CLI", pathFn: AntigravityConfigPath, intoFn: setupAntigravityInto, extractFn: antigravityCommandExtractor},
	{use: "antigravity-desktop", name: "Antigravity Desktop", pathFn: AntigravityDesktopConfigPath, intoFn: setupAntigravityInto, extractFn: antigravityCommandExtractor},
	{use: "opencode", name: "OpenCode", pathFn: OpenCodeConfigPath, intoFn: setupOpenCodeInto, extractFn: mapCommandExtractor(readOrInitClaudeConfig, "mcp", "command")},
	{use: "crush", name: "Crush", pathFn: CrushConfigPath, intoFn: setupCrushInto, extractFn: mapCommandExtractor(readOrInitClaudeConfig, "mcp", "command")},
	{use: "goose", name: "Goose", pathFn: GooseConfigPath, intoFn: setupGooseInto, extractFn: mapCommandExtractor(readOrInitYAMLConfig, "extensions", "cmd")},
	{use: "hermes", name: "Hermes", pathFn: HermesConfigPath, intoFn: setupHermesInto, extractFn: mapCommandExtractor(readOrInitYAMLConfig, "mcp_servers", "command")},
}

// allSetupClients lists every client `plumb setup` supports, for the `config show`
// MCP table, `plumb doctor`, and `plumb setup --all`. The first four are the
// originals; their intoFn/extractFn are wired here (the bespoke setup commands
// call the same intoFns directly) so the bulk repair and doctor checks can drive
// every client uniformly. The rest come from extraSetupTargets.
func allSetupClients() []setupTarget {
	clients := make([]setupTarget, 0, 4+len(extraSetupTargets))
	clients = append(clients,
		setupTarget{use: "claude-code", name: "Claude Code", pathFn: claudeCodeConfigPath, intoFn: setupClaudeCodeInto, extractFn: claudeDesktopCommandExtractor},
		setupTarget{use: "claude-desktop", name: "Claude Desktop", pathFn: claudeDesktopConfigPath, pathsFn: claudeDesktopConfigPaths, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
		setupTarget{use: "gemini", name: "Gemini CLI", pathFn: GeminiConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
		setupTarget{use: "codex", name: "Codex", pathFn: CodexConfigPath, intoFn: setupCodexInto, extractFn: mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command")},
	)
	return append(clients, extraSetupTargets...)
}

// registeredCommand extracts the launch binary plumb is registered with from a
// parsed client config: servers[serversKey]["plumb"][cmdField]. ok is false when
// no plumb entry is present or the command field is missing or empty.
func registeredCommand(cfg map[string]any, serversKey, cmdField string) (string, bool) {
	servers, ok := cfg[serversKey].(map[string]any)
	if !ok {
		return "", false
	}
	entry, ok := servers["plumb"].(map[string]any)
	if !ok {
		return "", false
	}
	return commandString(entry[cmdField])
}

// commandString reads a launch command stored as either a bare string or an argv
// array (the binary is element 0). ok is false for any other shape or an empty
// value.
func commandString(v any) (string, bool) {
	switch c := v.(type) {
	case string:
		return c, c != ""
	case []any:
		if len(c) > 0 {
			if s, ok := c[0].(string); ok {
				return s, s != ""
			}
		}
	}
	return "", false
}

// mapCommandExtractor builds an extractFn for a client whose config is a single
// read-parseable map holding the plumb server under servers[serversKey].
func mapCommandExtractor(read func(string) (map[string]any, bool, error), serversKey, cmdField string) func(string) (string, bool, error) {
	return func(cfgPath string) (string, bool, error) {
		cfg, _, err := read(cfgPath)
		if err != nil {
			return "", false, err
		}
		bin, ok := registeredCommand(cfg, serversKey, cmdField)
		return bin, ok, nil
	}
}

// antigravityCommandExtractor reads the standalone Antigravity plumb.json, whose
// top-level object is the plumb entry itself (command + args).
func antigravityCommandExtractor(cfgPath string) (string, bool, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return "", false, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return "", false, err
	}
	bin, ok := commandString(m["command"])
	return bin, ok, nil
}

func init() {
	for _, t := range extraSetupTargets {
		setupCmd.AddCommand(&cobra.Command{
			Use:   t.use,
			Short: fmt.Sprintf("Register plumb as an MCP server in %s's config", t.name),
			RunE:  func(_ *cobra.Command, _ []string) error { return runSetupTarget(t) },
		})
	}
}

// runSetupTarget is the shared command body for every extraSetupTargets entry.
func runSetupTarget(t setupTarget) error {
	PrintLogo()
	cfgPath, err := t.pathFn()
	if err != nil {
		return fmt.Errorf("locating %s config: %w", t.name, err)
	}

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	added, preserved, err := t.intoFn(cfgPath, plumbBin)
	if err != nil {
		return err
	}

	if !added {
		fmt.Printf("plumb is already registered in %s — no changes made.\n", t.name)
		fmt.Printf("Config: %s\n", cfgPath)
		return nil
	}

	ctxStr := fmt.Sprintf("Registered in %s\nConfig: %s\nBinary: %s", t.name, cfgPath, plumbBin)
	if len(preserved) > 0 {
		ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preserved)
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
	fmt.Printf("\nRestart %s to apply the change.\n", t.name)
	return nil
}

// runSetupAll repoints every client that already registers plumb at the current
// binary, leaving clients that aren't installed or don't use plumb untouched. It
// is the bulk repair for a moved or rebuilt binary — the fix `plumb doctor`
// points at when a client's registered binary no longer matches the running one.
// Without --all the bare `plumb setup` command just prints help.
func runSetupAll(cmd *cobra.Command, _ []string) error {
	if !setupAllFlag {
		return cmd.Help()
	}
	PrintLogo()

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	tui.RebuildStyles()
	lines := make([]string, 0, len(allSetupClients()))
	changed := 0
	for _, c := range allSetupClients() {
		status, didChange := refreshClient(c, plumbBin)
		if didChange {
			changed++
		}
		lines = append(lines, fmt.Sprintf("%-22s %s", c.name, status))
	}

	body := fmt.Sprintf("Current binary: %s\n\n%s", plumbBin, strings.Join(lines, "\n"))
	fmt.Println(render.ContextBox(tui.MutedStyle.Render(body), tui.SepStyle))
	if changed == 0 {
		fmt.Println("\nNo changes — every registered client already points at this binary.")
	} else {
		fmt.Printf("\nRepointed %d client(s). Restart them to apply.\n", changed)
	}
	return nil
}

// refreshClient repoints one client's plumb registration at plumbBin, but only
// when the client is installed and already references plumb — it never adds plumb
// to a client that doesn't use it. Returns a human status line and whether it
// changed anything. A client with pathsFn set (currently only Claude Desktop) is
// refreshed at every resolved path, not just one.
func refreshClient(c setupTarget, plumbBin string) (status string, changed bool) {
	if c.intoFn == nil {
		return "skipped (no updater)", false
	}
	paths, err := resolveTargetPaths(c)
	if err != nil {
		return "error: " + err.Error(), false
	}

	statuses := make([]string, 0, len(paths))
	for _, cfgPath := range paths {
		s, didChange := refreshClientAt(c, cfgPath, plumbBin)
		if didChange {
			changed = true
		}
		if len(paths) > 1 {
			s = render.ContractPath(cfgPath) + ": " + s
		}
		statuses = append(statuses, s)
	}
	return strings.Join(statuses, "; "), changed
}

// resolveTargetPaths returns every config path refreshClient should manage for
// c: c.pathsFn's full list when set, otherwise the single c.pathFn path.
func resolveTargetPaths(c setupTarget) ([]string, error) {
	if c.pathsFn != nil {
		return c.pathsFn()
	}
	p, err := c.pathFn()
	if err != nil {
		return nil, err
	}
	return []string{p}, nil
}

// refreshClientAt is refreshClient's single-path body.
func refreshClientAt(c setupTarget, cfgPath, plumbBin string) (status string, changed bool) {
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return "not installed — skipped", false
	}
	if !clientHasPlumb(c, cfgPath) {
		return "plumb not registered — skipped", false
	}
	added, _, err := c.intoFn(cfgPath, plumbBin)
	if err != nil {
		return "error: " + err.Error(), false
	}
	if !added {
		return "already current", false
	}
	return "updated → " + render.ContractPath(plumbBin), true
}

// clientHasPlumb reports whether cfgPath already registers a plumb server, using
// the structured extractor when available and falling back to a substring scan.
func clientHasPlumb(c setupTarget, cfgPath string) bool {
	if c.extractFn != nil {
		if _, registered, err := c.extractFn(cfgPath); err == nil {
			return registered
		}
	}
	data, err := os.ReadFile(cfgPath)
	return err == nil && strings.Contains(string(data), "plumb")
}

// setupOpenCodeInto registers plumb under OpenCode's top-level "mcp" key. A local
// (stdio) server packs the binary and its args into a single "command" array and
// is enabled by default.
func setupOpenCodeInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "mcp", readOrInitClaudeConfig, writeJSON,
		map[string]any{"type": "local", "command": []string{plumbBin, "serve"}, "enabled": true},
		func(existing map[string]any) bool {
			return stringSliceEqual(existing["command"], []string{plumbBin, "serve"})
		},
	)
}

// setupCrushInto registers plumb under Crush's top-level "mcp" key as a stdio
// server (separate command + args, unlike OpenCode's combined array).
func setupCrushInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "mcp", readOrInitClaudeConfig, writeJSON,
		map[string]any{"type": "stdio", "command": plumbBin, "args": []string{"serve"}},
		func(existing map[string]any) bool { return existing["command"] == plumbBin },
	)
}

// setupGooseInto registers plumb as a stdio extension in Goose's YAML config.
// Goose names the executable "cmd" (not "command") and keys extensions under
// "extensions".
func setupGooseInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "extensions", readOrInitYAMLConfig, writeYAML,
		map[string]any{
			"type":    "stdio",
			"name":    "plumb",
			"cmd":     plumbBin,
			"args":    []string{"serve"},
			"enabled": true,
			"timeout": 300,
		},
		func(existing map[string]any) bool { return existing["cmd"] == plumbBin },
	)
}

// setupHermesInto registers plumb under Hermes's "mcp_servers" YAML key. A stdio
// server is implied by the presence of command + args.
func setupHermesInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "mcp_servers", readOrInitYAMLConfig, writeYAML,
		map[string]any{"command": plumbBin, "args": []string{"serve"}},
		func(existing map[string]any) bool { return existing["command"] == plumbBin },
	)
}

// setupAntigravityInto registers plumb in the flat mcp_config.json files
// Antigravity actually reads (primarily the shared ~/.gemini/config one, which
// serves both the CLI and IDE), and also writes the standalone mcp/plumb.json
// for the doctor binary-anchor. The flat-config write is the one that makes
// plumb appear in Antigravity — the standalone mcp/ dir is regenerated by
// Antigravity from the shared config, so a plumb entry written only there is
// ignored.
func setupAntigravityInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	dir := filepath.Dir(cfgPath)
	preserved = listPreservedAntigravityServers(dir)

	// Create or repoint plumb in every flat mcp_config.json Antigravity reads.
	// This is the load-bearing write; the standalone file below is a secondary.
	flatChanged := ensureAntigravityFlatConfigs(geminiBaseFromStandalone(cfgPath), plumbBin)

	if isSameAntigravityConfig(cfgPath, plumbBin) {
		syncAntigravityIdeConfig(dir, plumbBin)
		return len(flatChanged) > 0, preserved, nil
	}

	if _, err := os.Stat(cfgPath); err == nil {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	} else if !os.IsNotExist(err) {
		return false, nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, nil, err
	}

	if err := writeAntigravityConfig(cfgPath, plumbBin); err != nil {
		return false, nil, err
	}

	syncAntigravityIdeConfig(dir, plumbBin)

	return true, preserved, nil
}

func listPreservedAntigravityServers(dir string) []string {
	var preserved []string
	files, err := os.ReadDir(dir)
	if err == nil {
		for _, f := range files {
			if !f.IsDir() && filepath.Ext(f.Name()) == ".json" {
				name := filepath.Base(f.Name())
				name = name[:len(name)-5] // strip .json
				if name != "plumb" {
					preserved = append(preserved, name)
				}
			}
		}
		sort.Strings(preserved)
	}
	return preserved
}

func isSameAntigravityConfig(path string, plumbBin string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var existing map[string]any
	if err := json.Unmarshal(data, &existing); err != nil {
		return false
	}
	return existing["command"] == plumbBin && stringSliceEqual(existing["args"], []string{"serve"})
}

func syncAntigravityIdeConfig(dir string, plumbBin string) {
	if filepath.Base(filepath.Dir(dir)) == "antigravity" {
		idePath := filepath.Join(filepath.Dir(filepath.Dir(dir)), "antigravity-ide", "mcp", "plumb.json")
		if _, err := os.Stat(filepath.Dir(idePath)); err == nil {
			_ = writeAntigravityConfig(idePath, plumbBin)
		}
	}
}

func writeAntigravityConfig(path string, plumbBin string) error {
	entry := map[string]any{
		"command": plumbBin,
		"args":    []string{"serve"},
	}
	return writeJSON(path, entry)
}
