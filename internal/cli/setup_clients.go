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
// `plumb doctor`'s canonical-path check. installedFn is optional too: when the
// config file is absent it decides whether the client itself is installed —
// Kimi Code sets it because its mcp.json only appears once an MCP server is
// configured, so an absent file does not imply an absent client. When it
// reports installed, the bulk paths and doctor treat the client as
// installed-but-unregistered (and --install-missing may create the config).
// flags and note are the per-target hooks for a client whose registration takes
// an option: flags registers extra command-line flags on the generated cobra
// command, and note returns an extra hint line printed after the registration
// report (empty for nothing). Only Kimi Code sets them today, for --lean.
// skillsDirFn is the per-client SKILL.md capability check: when set, the client
// consumes plumb's embedded skills and this resolves the user-scoped directory
// they install into, so registering the client also installs and refreshes them.
// nil means the client has no verified skill channel — its steering arrives as
// the condensed session_start guidance block instead. See setup_skills.go, which
// holds the resolvers and the per-client verification evidence.
type setupTarget struct {
	use         string
	name        string
	pathFn      func() (string, error)
	pathsFn     func() ([]string, error)
	installedFn func() bool
	intoFn      func(cfgPath, plumbBin string) (added bool, preserved []string, err error)
	extractFn   func(cfgPath string) (binPath string, registered bool, err error)
	flags       func(cmd *cobra.Command)
	note        func() string
	skillsDirFn func() (string, error)
}

// claudeDesktopCommandExtractor reads the plumb launch binary back from a
// Claude Desktop-shaped mcpServers JSON config. Shared by the claude-desktop
// setupTarget and the extra-profile doctor check (checkClaudeDesktopExtraProfiles).
var claudeDesktopCommandExtractor = mapCommandExtractor(readOrInitClaudeConfig, "mcpServers", "command")

// extraSetupTargets are the command-line MCP-client agents that consume external
// MCP servers and therefore make sense as `plumb setup` targets. The first four
// share Claude Desktop's plain `mcpServers` JSON shape; the rest use a distinct
// key, entry shape, or serialisation (see each setup*Into helper).
var extraSetupTargets = []setupTarget{
	{use: "cursor", name: "Cursor", pathFn: CursorConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
	{use: "augment", name: "Augment Code", pathFn: AugmentConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
	{use: "qwen", name: "Qwen Code", pathFn: QwenConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor},
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
		flags:       registerKimiLeanFlag,
		note:        kimiLeanNote,
		skillsDirFn: kimiCodeSkillsDir,
	},
	{use: "antigravity", name: "Antigravity CLI", pathFn: AntigravityConfigPath, intoFn: setupAntigravityInto, extractFn: antigravityCommandExtractor},
	{use: "antigravity-desktop", name: "Antigravity Desktop", pathFn: AntigravityDesktopConfigPath, intoFn: setupAntigravityInto, extractFn: antigravityCommandExtractor},
	{use: "opencode", name: "OpenCode", pathFn: OpenCodeConfigPath, intoFn: setupOpenCodeInto, extractFn: mapCommandExtractor(readOrInitClaudeConfig, "mcp", "command")},
	{use: "crush", name: "Crush", pathFn: CrushConfigPath, intoFn: setupCrushInto, extractFn: mapCommandExtractor(readOrInitClaudeConfig, "mcp", "command")},
	{use: "goose", name: "Goose", pathFn: GooseConfigPath, intoFn: setupGooseInto, extractFn: mapCommandExtractor(readOrInitYAMLConfig, "extensions", "cmd")},
	{use: "hermes", name: "Hermes", pathFn: HermesConfigPath, intoFn: setupHermesInto, extractFn: mapCommandExtractor(readOrInitYAMLConfig, "mcp_servers", "command")},
}

// The four original setup targets, named so that both the bespoke commands in
// setup.go and the bulk paths here drive them from one description.
//
// claudeCodeTarget and claudeDesktopTarget still have hand-written command
// bodies: Claude Code adds --project scoping and skill installation, and Claude
// Desktop writes several config paths (its sibling-profile heuristic) with
// per-path reporting. geminiTarget and codexTarget do not — their commands were
// line-for-line copies of runSetupTarget and now call it directly.
var (
	claudeCodeTarget    = setupTarget{use: "claude-code", name: "Claude Code", pathFn: claudeCodeConfigPath, intoFn: setupClaudeCodeInto, extractFn: claudeDesktopCommandExtractor, skillsDirFn: claudeSkillsDir}
	claudeDesktopTarget = setupTarget{use: "claude-desktop", name: "Claude Desktop", pathFn: claudeDesktopConfigPath, pathsFn: claudeDesktopConfigPaths, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor}
	geminiTarget        = setupTarget{use: "gemini", name: "Gemini CLI", pathFn: GeminiConfigPath, intoFn: setupClaudeDesktopInto, extractFn: claudeDesktopCommandExtractor}
	codexTarget         = setupTarget{use: "codex", name: "Codex", pathFn: CodexConfigPath, intoFn: setupCodexInto, extractFn: mapCommandExtractor(readOrInitCodexConfig, "mcp_servers", "command"), skillsDirFn: codexSkillsDir}
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
			Short: fmt.Sprintf("Register plumb as an MCP server in %s's config", t.name),
			RunE:  func(_ *cobra.Command, _ []string) error { return runSetupTarget(t) },
		}
		if t.flags != nil {
			t.flags(cmd)
		}
		if t.skillsDirFn != nil {
			registerNoSkillFlag(cmd)
		}
		setupCmd.AddCommand(cmd)
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
		// Skills are refreshed on the already-registered path too: re-running
		// setup after an upgrade is the documented way to pick up new skill
		// content, and it would be a no-op on every machine plumb is already
		// registered on if this returned first.
		installAndPrintSkills(t)
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
	installAndPrintSkills(t)
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

// runSetupAll repoints every client that already registers plumb at the current
// binary, leaving clients that aren't installed or don't use plumb untouched. It
// is the bulk repair for a moved or rebuilt binary — the fix `plumb doctor`
// points at when a client's registered binary no longer matches the running one.
// With --install-missing it also registers plumb in installed-but-unregistered
// clients (config file present but no plumb entry), covering first-time setup in
// one command. Either flag triggers the bulk run; bare `plumb setup` prints help.
func runSetupAll(cmd *cobra.Command, _ []string) error {
	if !setupAllFlag && !setupInstallMissingFlag {
		return cmd.Help()
	}
	PrintLogo()

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render("Current binary: "+plumbBin), tui.SepStyle))
	fmt.Println()

	t := render.DottedTableBase(tui.SepStyle, tui.HintStyle).
		Headers("Client", "Status", "Config")
	changed, unregistered := 0, 0
	for _, c := range allSetupClients() {
		rows, didChange := refreshClient(c, plumbBin, setupInstallMissingFlag)
		if didChange {
			changed++
		}
		for _, r := range rows {
			if r.status == "not registered" {
				unregistered++
			}
			t.Row(r.name, r.status, r.detail)
		}
	}
	fmt.Println(t.Render())

	printSetupAllSummary(changed, unregistered)
	return nil
}

// printSetupAllSummary prints the trailing summary line(s) for `plumb setup
// --all`. When some clients are installed-but-unregistered and --install-missing
// was not passed, it points at the flag that would register them — the fix for
// the "I ran --all and nothing happened" first-time-setup confusion.
func printSetupAllSummary(changed, unregistered int) {
	if changed == 0 {
		if setupInstallMissingFlag {
			fmt.Println("\nNo changes — every installed client already has this binary registered.")
		} else {
			fmt.Println("\nNo changes — every registered client already points at this binary.")
		}
	} else {
		fmt.Printf("\nUpdated %d client(s). Restart them to apply.\n", changed)
	}
	if !setupInstallMissingFlag && unregistered > 0 {
		fmt.Printf("\n%d installed client(s) don't have plumb yet — run `plumb setup --install-missing` to register them.\n", unregistered)
	}
}

// clientRow is one row in the `plumb setup --all` table: a client name (blank on
// the continuation rows of a multi-path client, so the paths group visually), a
// short status word, and the config path or error the status refers to.
type clientRow struct {
	name   string
	status string
	detail string
}

// refreshClient repoints one client's plumb registration at plumbBin. It repoints
// a client that already references plumb; when installMissing is set it also
// registers plumb in an installed client whose config file exists but has no plumb
// entry. It never fabricates a config for a client with no config file at all —
// unless the target's installedFn vouches that the client is installed anyway
// (Kimi Code's mcp.json only exists once an MCP server is configured), in which
// case installMissing creates the config fresh.
// Returns one table row per managed config path and whether any of them changed. A
// client with pathsFn set (currently only Claude Desktop) yields one row per
// resolved path, not just one.
func refreshClient(c setupTarget, plumbBin string, installMissing bool) (rows []clientRow, changed bool) {
	if c.intoFn == nil {
		return []clientRow{{name: c.name, status: "skipped", detail: "no updater"}}, false
	}
	paths, err := resolveTargetPaths(c)
	if err != nil {
		return []clientRow{{name: c.name, status: "error", detail: err.Error()}}, false
	}

	registered := false
	for i, cfgPath := range paths {
		status, detail, didChange := refreshClientAt(c, cfgPath, plumbBin, installMissing)
		if didChange {
			changed = true
		}
		if plumbIsRegistered(status) {
			registered = true
		}
		name := c.name
		if i > 0 {
			name = ""
		}
		rows = append(rows, clientRow{name: name, status: status, detail: detail})
	}

	// Skills are part of what a repoint has to bring back into line. `--all` is
	// the post-rebuild repair, and session_start points the agent at skills that
	// only the named per-client command used to refresh — so a rebuilt binary
	// left them stale, or, for a client registered by --install-missing, absent
	// entirely while the guidance still cited them.
	if registered {
		if status, detail, didChange := refreshSkills(c); status != "" {
			rows = append(rows, clientRow{status: status, detail: detail})
			changed = changed || didChange
		}
	}
	return rows, changed
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

// refreshClientAt is refreshClient's single-path body. It returns a short status
// word plus a detail cell — the contracted config path, or the error text when
// status is "error". A config-present client without a plumb entry is only
// registered ("registered") when installMissing is set; otherwise it is reported
// "not registered" and left untouched. A repointed existing entry reports
// "updated". An absent config means "not installed" — unless the target's
// installedFn detects the client anyway, in which case it is treated as
// installed-but-unregistered (and installMissing creates the config).
func refreshClientAt(c setupTarget, cfgPath, plumbBin string, installMissing bool) (status, detail string, changed bool) {
	detail = render.ContractPath(cfgPath)
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if c.installedFn == nil || !c.installedFn() {
			return "not installed", detail, false
		}
		// Installed client whose config does not exist yet (Kimi Code) — fall
		// through as installed-but-unregistered.
	}
	hadPlumb := clientHasPlumb(c, cfgPath)
	if !hadPlumb && !installMissing {
		return "not registered", detail, false
	}
	added, _, err := c.intoFn(cfgPath, plumbBin)
	if err != nil {
		return "error", err.Error(), false
	}
	if !added {
		return "already current", detail, false
	}
	if hadPlumb {
		return "updated", detail, true
	}
	return "registered", detail, true
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
