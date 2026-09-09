package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
// `plumb doctor`'s canonical-path check. installedFn is optional too: when the
// config file is absent it decides whether the client itself is installed —
// Kimi Code sets it because its mcp.json only appears once an MCP server is
// configured, so an absent file does not imply an absent client. When it
// reports installed, the bulk paths and doctor treat the client as
// installed-but-unregistered (and --all may create the config).
// flags and note are the per-target hooks for a client whose registration takes
// an option: flags registers extra command-line flags on the generated cobra
// command, and note returns an extra hint line printed after the registration
// report (empty for nothing). Only Kimi Code sets them today, for --lean.
// skillsDirFn is the per-client SKILL.md capability check: when set, the client
// consumes plumb's embedded skills and this resolves the user-scoped directory
// they live in — the set `plumb skills` reports on and `plumb skills sync`
// writes to. Registration itself never writes skill files.
// nil means the client has no verified skill channel — its steering arrives as
// the condensed session_start guidance block instead. See setup_skills.go, which
// holds the resolvers and the per-client verification evidence.
// outFn is intoFn's removal counterpart: it takes plumb's entry back out of
// one config path (and only that entry — siblings always survive). See
// setup_uninstall.go, which holds every implementation.
type setupTarget struct {
	use         string
	name        string
	pathFn      func() (string, error)
	pathsFn     func() ([]string, error)
	installedFn func() bool
	intoFn      func(cfgPath, plumbBin string) (added bool, preserved []string, err error)
	extractFn   func(cfgPath string) (binPath string, registered bool, err error)
	outFn       func(cfgPath string) (removed bool, err error)
	flags       func(cmd *cobra.Command)
	note        func() string
	skillsDirFn func() (string, error)
}

// claudeDesktopCommandExtractor reads the plumb launch binary back from a
// Claude Desktop-shaped mcpServers JSON config. Shared by the claude-desktop
// setupTarget and the extra-profile doctor check (checkClaudeDesktopExtraProfiles).
var claudeDesktopCommandExtractor = mapCommandExtractor(readOrInitClaudeConfig, "mcpServers", "command")

// extraSetupTargets are the command-line MCP-client agents that consume external
// MCP servers and therefore make sense as `plumb setup` targets. The first six
// share Claude Desktop's plain `mcpServers` JSON shape; the rest use a distinct
// key, entry shape, or serialisation (see each setup*Into helper).
var extraSetupTargets = []setupTarget{
	{use: "cursor", name: "Cursor", pathFn: CursorConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor, outFn: removeMcpServersJSON},
	{use: "augment", name: "Augment Code", pathFn: AugmentConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor, outFn: removeMcpServersJSON},
	{use: "qwen", name: "Qwen Code", pathFn: QwenConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor, outFn: removeMcpServersJSON},
	{use: "junie", name: "Junie", pathFn: JunieConfigPath, installedFn: junieInstalled, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor, outFn: removeMcpServersJSON, skillsDirFn: junieSkillsDir},
	{
		use: "kimi-code", name: "Kimi Code", pathFn: KimiCodeConfigPath, installedFn: kimiCodeInstalled,
		// Kimi Code is the one target with an option: --lean additionally writes a
		// client-side enabledTools allowlist. The flag is read at call time, so the
		// bulk --all/--install-missing paths (which never set it) keep registering
		// bare and preserve any allowlist already present. See setup_kimi.go.
		intoFn: func(cfgPath, plumbBin string) (bool, []string, error) {
			return kimiCodeInto(cfgPath, plumbBin, setupKimiLeanFlag)
		},
		extractFn:   claudeDesktopCommandExtractor,
		outFn:       removeMcpServersJSON,
		flags:       registerKimiLeanFlag,
		note:        kimiLeanNote,
		skillsDirFn: kimiCodeSkillsDir,
	},
	{
		use: "kimi-work", name: "Kimi Work", pathFn: KimiWorkConfigPath, installedFn: kimiWorkInstalled,
		// The daimon-based desktop app bundles its own kimi-code kernel home
		// (KimiWorkConfigPath), so it needs a registration separate from the
		// CLI's. No --lean flag (enabledTools is unverified against the app —
		// kimiWorkInto) and no skillsDirFn: the app's kernel reads the CLI's
		// user skills dir (~/.kimi-code/skills) — verified on a live Kimi Work
		// session (2026-08-20), where skills synced by `plumb skills sync
		// kimi-code` appeared in the app's session — so the kimi-code target
		// already covers skill delivery, and declaring the same dir here would
		// double-list it in `plumb skills`.
		intoFn:    kimiWorkInto,
		extractFn: claudeDesktopCommandExtractor,
		outFn:     removeMcpServersJSON,
	},
	{use: "antigravity", name: "Antigravity CLI", pathFn: AntigravityConfigPath, intoFn: setupAntigravityInto, extractFn: antigravityCommandExtractor, outFn: setupAntigravityOut},
	{use: "antigravity-desktop", name: "Antigravity Desktop", pathFn: AntigravityDesktopConfigPath, intoFn: setupAntigravityInto, extractFn: antigravityCommandExtractor, outFn: setupAntigravityOut},
	{use: "opencode", name: "OpenCode", pathFn: OpenCodeConfigPath, intoFn: setupOpenCodeInto, extractFn: mapCommandExtractor(readOrInitClaudeConfig, "mcp", "command"), outFn: removeMcpJSON},
	{use: "crush", name: "Crush", pathFn: CrushConfigPath, intoFn: setupCrushInto, extractFn: mapCommandExtractor(readOrInitClaudeConfig, "mcp", "command"), outFn: removeMcpJSON},
	{use: "goose", name: "Goose", pathFn: GooseConfigPath, intoFn: setupGooseInto, extractFn: mapCommandExtractor(readOrInitYAMLConfig, "extensions", "cmd"), outFn: removeGooseYAML},
	{use: "hermes", name: "Hermes", pathFn: HermesConfigPath, intoFn: setupHermesInto, extractFn: mapCommandExtractor(readOrInitYAMLConfig, "mcp_servers", "command"), outFn: removeHermesYAML},
	// ZCode nests its servers under mcp.servers in ~/.zcode/cli/config.json and
	// enforces a strict server schema — setup_zcode.go holds the entry shape and
	// the reasons there is no --lean here.
	{use: "zcode", name: "ZCode", pathFn: ZCodeConfigPath, installedFn: zcodeInstalled, intoFn: setupZCodeInto, extractFn: zcodeCommandExtractor, outFn: setupZCodeOut, skillsDirFn: zcodeSkillsDir},
	// DeepSeek Harness writes a YAML patch row into its home-level user patch
	// layer rather than a server map — see setup_dsh.go for the node-level merge.
	{use: "dsh", name: "DeepSeek Harness", pathFn: DSHConfigPath, installedFn: dshInstalled, intoFn: setupDSHInto, extractFn: dshCommandExtractor, outFn: setupDSHOut, note: dshSetupNote},
}

// The four original setup targets, named so that both the bespoke commands in
// setup.go and the bulk paths here drive them from one description.
//
// claudeCodeTarget and claudeDesktopTarget still have hand-written command
// bodies: Claude Code adds --project scoping, and Claude Desktop writes several
// config paths (its sibling-profile heuristic) with per-path reporting.
// geminiTarget and codexTarget do not — their commands were line-for-line
// copies of runSetupTarget and now call it directly.
var (
	claudeCodeTarget    = setupTarget{use: "claude-code", name: "Claude Code", pathFn: claudeCodeConfigPath, intoFn: setupClaudeCodeInto, extractFn: claudeDesktopCommandExtractor, outFn: removeMcpServersJSON, skillsDirFn: claudeSkillsDir}
	claudeDesktopTarget = setupTarget{use: "claude-desktop", name: "Claude Desktop", pathFn: claudeDesktopConfigPath, pathsFn: claudeDesktopConfigPaths, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor, outFn: removeMcpServersJSON}
	// Gemini CLI shares Claude Desktop's mcpServers shape but has its own writer
	// (setup_lean.go): --lean writes an includeTools allowlist Claude Desktop
	// does not read.
	geminiTarget = setupTarget{
		use: "gemini", name: "Gemini CLI", pathFn: GeminiConfigPath, intoFn: setupGeminiInto,
		extractFn: claudeDesktopCommandExtractor,
		outFn:     removeMcpServersJSON,
		flags:     leanFlagRegistrar(&setupGeminiLeanFlag, geminiLeanClient),
		note:      func() string { return leanSetupNote(geminiLeanClient, leanChoiceOf(setupGeminiLeanFlag)) },
	}
	codexTarget = setupTarget{
		use: "codex", name: "Codex", pathFn: CodexConfigPath, intoFn: setupCodexInto,
		extractFn:   mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command"),
		outFn:       removeCodexTOML,
		flags:       leanFlagRegistrar(&setupCodexLeanFlag, codexLeanClient),
		note:        func() string { return leanSetupNote(codexLeanClient, leanChoiceOf(setupCodexLeanFlag)) },
		skillsDirFn: codexSkillsDir,
	}
)

// allSetupClients lists every client `plumb setup` supports, for the `config show`
// MCP table, `plumb doctor`, and `plumb setup --all`. Order is display order and
// is deliberate: the four originals first, then extraSetupTargets.
func allSetupClients() []setupTarget {
	clients := make([]setupTarget, 0, 4+len(extraSetupTargets))
	clients = append(clients, claudeCodeTarget, claudeDesktopTarget, geminiTarget, codexTarget)
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
		cmd := &cobra.Command{
			Use:   t.use,
			Short: "Register plumb in " + t.name,
			RunE:  func(_ *cobra.Command, _ []string) error { return runSetupTargetOrUninstall(t) },
		}
		registerTargetFlags(cmd, t)
		registerUninstallFlag(cmd)
		setupCmd.AddCommand(cmd)
	}
}

// registerTargetFlags applies a target's optional flags hook to its command.
// Shared by the generated commands here and the two hand-written ones in
// setup.go (gemini, codex), so a target that grows an option gets it wherever
// its command is built.
func registerTargetFlags(cmd *cobra.Command, t setupTarget) {
	if t.flags != nil {
		t.flags(cmd)
	}
}

// runSetupTarget is the shared command body for every target whose registration
// is a plain single-config merge — all of extraSetupTargets, plus geminiTarget
// and codexTarget.
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
		printSetupNote(t)
		printSkillsDriftHint(t)
		return nil
	}

	ctxStr := fmt.Sprintf("Registered in %s\nConfig: %s\nBinary: %s", t.name, cfgPath, plumbBin)
	if len(preserved) > 0 {
		ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preserved)
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
	fmt.Printf("\nRestart %s to apply the change.\n", t.name)
	printSetupNote(t)
	printSkillsDriftHint(t)
	return nil
}

// printSetupNote prints a target's optional post-registration hint. It runs on
// BOTH the registered and the already-current paths: a repeat
// `plumb setup kimi-code --lean` writes nothing, and the note is the only
// confirmation the user gets that the allowlist is in place. A nil hook, or one
// that returns "", prints nothing.
func printSetupNote(t setupTarget) {
	if t.note == nil {
		return
	}
	if s := t.note(); s != "" {
		fmt.Printf("\n%s\n", s)
	}
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

// setupAntigravityInto registers plumb by writing the one standalone
// mcp/plumb.json the target owns — a {command, args} entry, not an mcpServers
// wrapper. The legacy flat mcp_config.json layer under ~/.gemini is gone:
// Antigravity no longer reads those files, so this standalone file is the one
// true registration surface for both targets.
func setupAntigravityInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	dir := filepath.Dir(cfgPath)
	preserved = listPreservedAntigravityServers(dir)

	if isSameAntigravityConfig(cfgPath, plumbBin) {
		return false, preserved, nil
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

func writeAntigravityConfig(path string, plumbBin string) error {
	entry := map[string]any{
		"command": plumbBin,
		"args":    []string{"serve"},
	}
	return writeJSON(path, entry)
}
