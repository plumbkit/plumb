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
	Long: `Register plumb as an MCP server in an external client's config.

Registration is config-only: it never installs skill files. Skill-capable
clients (claude-code, codex, kimi-code, zcode) get their skills from
` + "`plumb skills sync`" + ` — setup prints a hint when it notices them missing or
stale, and bare ` + "`plumb skills`" + ` shows the full status table.

Run a subcommand (e.g. ` + "`plumb setup claude-code`" + `) to register a single
client, or use a bulk flag:

  --repair  Repoint every already-registered client at the current plumb
            binary — the repair after the binary moves or is rebuilt
            elsewhere (see the registered-binary check in ` + "`plumb doctor`" + `).
            It never adds plumb to a client that lacked it.
  --all     --repair, plus register plumb in installed clients that do not
            have it yet (config file present but no plumb entry). Clients
            with no config file at all are left untouched — plumb cannot
            tell an absent config from an uninstalled client, so use the
            client's named subcommand to create one. The exceptions are Kimi
            Code, detected via its data dir ($KIMI_CODE_HOME, or
            ~/.kimi-code), and ZCode, detected via its home dir (~/.zcode):
            their MCP configs only exist once a server is configured, so
            --all creates them fresh.`,
	RunE: runSetupAll,
}

var (
	setupRepairFlag         bool
	setupAllFlag            bool
	setupInstallMissingFlag bool
)

var setupClaudeDesktopCmd = &cobra.Command{
	Use:   "claude-desktop",
	Short: "Register plumb as an MCP server in Claude Desktop's config",
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
	Short: "Register plumb as an MCP server in Claude Code's config",
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
	Short: "Register plumb as an MCP server in Gemini CLI's config",
	RunE:  func(_ *cobra.Command, _ []string) error { return runSetupTarget(geminiTarget) },
}

var setupCodexCmd = &cobra.Command{
	Use:   "codex",
	Short: "Register plumb as an MCP server in Codex's config",
	RunE:  func(_ *cobra.Command, _ []string) error { return runSetupTarget(codexTarget) },
}

func init() {
	setupCmd.Flags().BoolVar(&setupRepairFlag, "repair", false,
		"Repoint every already-registered client at the current plumb binary (no new registrations)")
	setupCmd.Flags().BoolVar(&setupAllFlag, "all", false,
		"--repair, plus register plumb in installed clients that don't have it yet")
	setupCmd.Flags().BoolVar(&setupInstallMissingFlag, "install-missing", false,
		"Also register plumb in installed clients that don't have it yet")
	// One-release bridge: --install-missing used to be the only way to get the
	// behaviour --all now has. It still works but warns and stays out of help.
	_ = setupCmd.Flags().MarkHidden("install-missing")
	_ = setupCmd.Flags().MarkDeprecated("install-missing", "use --all instead")
	setupCmd.AddCommand(setupClaudeDesktopCmd)
	setupClaudeCodeCmd.Flags().BoolVar(&setupClaudeCodeProjectFlag, "project", false, "Write to .mcp.json in the current directory (project-scoped)")
	setupCmd.AddCommand(setupClaudeCodeCmd)
	registerTargetFlags(setupGeminiCmd, geminiTarget)
	setupCmd.AddCommand(setupGeminiCmd)
	registerTargetFlags(setupCodexCmd, codexTarget)
	setupCmd.AddCommand(setupCodexCmd)
}

func runSetupClaudeDesktop(_ *cobra.Command, _ []string) error {
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
	PrintLogo()
	var cfgPath string
	var scope string
	if setupClaudeCodeProjectFlag {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getting working directory: %w", err)
		}
		cfgPath = filepath.Join(cwd, ".mcp.json")
		scope = "project"
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locating home directory: %w", err)
		}
		cfgPath = filepath.Join(home, ".claude.json")
		scope = "user"
	}

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
