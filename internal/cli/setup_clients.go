package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// setupTarget describes one `plumb setup <client>` command in a data-driven way:
// the lowercase subcommand (use), the human name shown in messages, the config
// path resolver, and the format-specific merge helper. The merge helpers all
// funnel through mergeServerEntry, so each client differs only in path, key,
// entry shape, and serialisation.
type setupTarget struct {
	use    string
	name   string
	pathFn func() (string, error)
	intoFn func(cfgPath, plumbBin string) (added bool, preserved []string, err error)
}

// extraSetupTargets are the command-line MCP-client agents that consume external
// MCP servers and therefore make sense as `plumb setup` targets. The first three
// share Claude Desktop's plain `mcpServers` JSON shape; the rest use a distinct
// key, entry shape, or serialisation (see each setup*Into helper).
var extraSetupTargets = []setupTarget{
	{"cursor", "Cursor", CursorConfigPath, setupClaudeDesktopInto},
	{"augment", "Augment Code", AugmentConfigPath, setupClaudeDesktopInto},
	{"qwen", "Qwen Code", QwenConfigPath, setupClaudeDesktopInto},
	{"antigravity", "Antigravity CLI", AntigravityConfigPath, setupClaudeDesktopInto},
	{"opencode", "OpenCode", OpenCodeConfigPath, setupOpenCodeInto},
	{"crush", "Crush", CrushConfigPath, setupCrushInto},
	{"goose", "Goose", GooseConfigPath, setupGooseInto},
	{"hermes", "Hermes", HermesConfigPath, setupHermesInto},
}

// allSetupClients lists every client `plumb setup` supports, for the `config show`
// MCP table and `plumb doctor`. The first four are the originals (with bespoke
// setup commands, so their intoFn is nil); the rest come from extraSetupTargets.
func allSetupClients() []setupTarget {
	clients := make([]setupTarget, 0, 4+len(extraSetupTargets))
	clients = append(clients,
		setupTarget{"claude-code", "Claude Code", claudeCodeConfigPath, nil},
		setupTarget{"claude-desktop", "Claude Desktop", claudeDesktopConfigPath, nil},
		setupTarget{"gemini", "Gemini CLI", GeminiConfigPath, nil},
		setupTarget{"codex", "Codex", CodexConfigPath, nil},
	)
	return append(clients, extraSetupTargets...)
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
