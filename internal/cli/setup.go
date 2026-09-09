package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure plumb with external tools",
	Long: `Register plumb as an MCP server in a client's config: run a subcommand
(e.g. ` + "`plumb setup claude-code`" + `) to register a single client, or ` + "`plumb setup --all`" + ` to
sweep every one.

Registration is config-only: it never installs skill files. Skill-capable
clients (claude-code, codex, junie, kimi-code, zcode) get their skills from
` + "`plumb skills sync`" + ` — setup prints a hint when it notices them missing or
stale, and bare ` + "`plumb skills`" + ` shows the full status table.

--all is the single bulk flag: it registers plumb in every installed client
and repoints existing registrations at this binary — the one-shot first-time
setup, and the repair after the binary moves or is rebuilt elsewhere (see the
registered-binary check in ` + "`plumb doctor`" + `). Clients with no config file at
all are left untouched — plumb cannot tell an absent config from an
uninstalled client, so use the client's named subcommand to create one. The
exceptions are Junie, detected via its home dir (~/.junie), Kimi Code,
detected via its data dir ($KIMI_CODE_HOME, or ~/.kimi-code), ZCode, detected
via its home dir (~/.zcode), and DeepSeek Harness, detected via its home dir
($DSH_HOME, or ~/.dsh): their MCP configs only exist once an entry is
configured, so --all creates them fresh.

--repair and --install-missing are deprecated hidden aliases of --all with
the same effect. Bare ` + "`plumb setup`" + ` (no flags) opens an interactive
picker on a terminal: registered clients arrive checked, space toggles —
unchecking a registered client marks it for uninstall, shown in the warn
colour — and enter applies the selection. Without a terminal it prints this
help.

Every subcommand also takes --uninstall, which reverses its registration:
plumb's entry — and only plumb's — is removed from the client's config
(after a backup), and the skill files plumb itself installed in that client
are taken too. A skill the user rewrote keeps no provenance marker and is
left in place, reported.`, RunE: runSetupAll,
}

var (
	setupRepairFlag         bool
	setupAllFlag            bool
	setupInstallMissingFlag bool
)

var setupClaudeDesktopCmd = &cobra.Command{
	Use:   "claude-desktop",
	Short: "Register plumb in Claude Desktop",
	Long: `Register plumb as an MCP server in Claude Desktop's config.

Writes the one config path Anthropic documents (` + "`~/Library/Application Support/Claude/claude_desktop_config.json`" + ` on
macOS). It also heuristically registers plumb in any sibling "Claude*" profile
directory that already has its own claude_desktop_config.json — the shape
produced by the unofficial multi-account technique of launching Claude Desktop
with a distinct --user-data-dir, or installing the app a second time under a
different name. This is a best-effort naming match, not an Anthropic-documented
mechanism, so an unusually-named profile may be missed.`,
	RunE: runSetupClaudeDesktop,
}

var setupClaudeCodeProjectFlag bool

var setupClaudeCodeCmd = &cobra.Command{
	Use:   "claude-code",
	Short: "Register plumb in Claude Code",
	Long: `Register plumb as an MCP server in Claude Code (the CLI tool).

By default writes to the user-level config (~/.claude.json), which makes plumb
available in every project. Use --project to write to .mcp.json in the current
directory instead, scoping plumb to that project only.`,
	RunE: runSetupClaudeCode,
}

// Gemini CLI and Codex register through the shared runSetupTarget body (see
// setup_clients.go). Their own command functions used to be line-for-line copies
// of it, which meant a fix to the registration flow had to be made in three
// places; the two clients differ only in the target description. Both take
// --lean, registered through their targets' flags hook (see setup_lean.go) so
// the option travels with the target description rather than the command.
var setupGeminiCmd = &cobra.Command{
	Use:   "gemini",
	Short: "Register plumb in Gemini CLI",
	RunE:  func(_ *cobra.Command, _ []string) error { return runSetupTargetOrUninstall(geminiTarget) },
}

var setupCodexCmd = &cobra.Command{
	Use:   "codex",
	Short: "Register plumb in Codex",
	RunE:  func(_ *cobra.Command, _ []string) error { return runSetupTargetOrUninstall(codexTarget) },
}

func init() {
	setupCmd.Flags().BoolVar(&setupRepairFlag, "repair", false, "Deprecated alias of --all")
	setupCmd.Flags().BoolVar(&setupAllFlag, "all", false,
		"Register plumb in every installed client, and repoint existing registrations at this binary")
	setupCmd.Flags().BoolVar(&setupInstallMissingFlag, "install-missing", false,
		"Also register plumb in installed clients that don't have it yet")
	// One-release bridge: --repair used to be the repoint-only sweep and
	// --install-missing the only way to register missing clients; --all now does
	// both. The old flags still parse but warn and stay out of help.
	_ = setupCmd.Flags().MarkHidden("repair")
	_ = setupCmd.Flags().MarkDeprecated("repair", "use --all instead — it also repoints, and registers clients that lack plumb")
	_ = setupCmd.Flags().MarkHidden("install-missing")
	_ = setupCmd.Flags().MarkDeprecated("install-missing", "use --all instead")
	setupCmd.AddCommand(setupClaudeDesktopCmd)
	registerUninstallFlag(setupClaudeDesktopCmd)
	setupClaudeCodeCmd.Flags().BoolVar(&setupClaudeCodeProjectFlag, "project", false, "Write to .mcp.json in the current directory (project-scoped)")
	registerUninstallFlag(setupClaudeCodeCmd)
	setupCmd.AddCommand(setupClaudeCodeCmd)
	registerTargetFlags(setupGeminiCmd, geminiTarget)
	registerUninstallFlag(setupGeminiCmd)
	setupCmd.AddCommand(setupGeminiCmd)
	registerTargetFlags(setupCodexCmd, codexTarget)
	registerUninstallFlag(setupCodexCmd)
	setupCmd.AddCommand(setupCodexCmd)
}

func runSetupClaudeDesktop(_ *cobra.Command, _ []string) error {
	if setupUninstallFlag {
		return runSetupUninstall(claudeDesktopTarget)
	}
	PrintLogo()
	cfgPaths, err := claudeDesktopConfigPaths()
	if err != nil {
		return fmt.Errorf("locating Claude Desktop config: %w", err)
	}

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	lines := make([]string, 0, len(cfgPaths))
	var preservedAny []string
	changed := 0
	for _, cfgPath := range cfgPaths {
		added, preserved, err := setupClaudeDesktopInto(cfgPath, plumbBin)
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s: error: %v", render.ContractPath(cfgPath), err))
			continue
		}
		if !added {
			lines = append(lines, render.ContractPath(cfgPath)+": already current")
			continue
		}
		changed++
		lines = append(lines, render.ContractPath(cfgPath)+": registered")
		preservedAny = append(preservedAny, preserved...)
	}

	if changed == 0 {
		fmt.Println("plumb is already registered in every detected Claude Desktop profile — no changes made.")
		fmt.Println(strings.Join(lines, "\n"))
		return nil
	}

	ctxStr := fmt.Sprintf("Binary: %s\n\n%s", plumbBin, strings.Join(lines, "\n"))
	if len(cfgPaths) > 1 {
		ctxStr += fmt.Sprintf("\n\n%d extra profile(s) matched the unofficial \"Claude*\" multi-account naming\nconvention (see `plumb setup claude-desktop --help`).", len(cfgPaths)-1)
	}
	if len(preservedAny) > 0 {
		ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preservedAny)
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
	fmt.Println("\nRestart Claude Desktop to apply the change.")
	return nil
}

// setupClaudeDesktopInto merges the plumb entry into the Claude Desktop config
// at cfgPath without disturbing any other entries. Returns added=false when
// plumb was already registered with the same binary (no write performed).
// preserved lists the names of servers that were already present and kept.
func setupClaudeDesktopInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "mcpServers", readOrInitClaudeConfig, writeJSON,
		map[string]any{"command": plumbBin, "args": []string{"serve"}},
		func(existing map[string]any) bool { return existing["command"] == plumbBin },
	)
}

func runSetupClaudeCode(_ *cobra.Command, _ []string) error {
	cfgPath, scope, err := claudeCodeConfigTarget()
	if err != nil {
		return err
	}
	if setupUninstallFlag {
		// Skills are user-scoped, so only the user-scope uninstall takes them;
		// a --project uninstall touches just the project's .mcp.json.
		return uninstallTargetAt(claudeCodeTarget, []string{cfgPath}, scope == "user")
	}
	PrintLogo()

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	added, preserved, err := setupClaudeCodeInto(cfgPath, plumbBin)
	if err != nil {
		return err
	}

	if !added {
		fmt.Printf("plumb is already registered in Claude Code (%s) — no changes made.\n", scope)
		fmt.Printf("Config: %s\n", cfgPath)
	} else {
		ctxStr := fmt.Sprintf("Registered in Claude Code (%s config)\nConfig: %s\nBinary: %s", scope, cfgPath, plumbBin)
		if len(preserved) > 0 {
			ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preserved)
		}
		tui.RebuildStyles()
		fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
		fmt.Println("\nReload Claude Code (or open a new session) to apply the change.")
	}

	printSkillsDriftHint(claudeCodeTarget)
	return nil
}

// claudeCodeConfigTarget resolves the config file and scope label the
// claude-code command acts on: the user-level ~/.claude.json, or .mcp.json in
// the current directory under --project.
func claudeCodeConfigTarget() (cfgPath, scope string, err error) {
	if setupClaudeCodeProjectFlag {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("getting working directory: %w", err)
		}
		return filepath.Join(cwd, ".mcp.json"), "project", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".claude.json"), "user", nil
}

// setupClaudeCodeInto merges the plumb entry into a Claude Code config file.
// Claude Code requires a "type":"stdio" field that Claude Desktop does not.
func setupClaudeCodeInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "mcpServers", readOrInitClaudeConfig, writeJSON,
		map[string]any{"type": "stdio", "command": plumbBin, "args": []string{"serve"}},
		func(existing map[string]any) bool { return existing["command"] == plumbBin },
	)
}

// setupCodexInto and setupGeminiInto live in setup_lean.go — both clients take
// --lean, so their writers share the client-side allowlist seam.
