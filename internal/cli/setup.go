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

Run a subcommand (e.g. ` + "`plumb setup claude-code`" + `) to register a single
client, or ` + "`plumb setup --all`" + ` to repoint every already-registered client
at the current plumb binary — the repair after the binary moves or is rebuilt
elsewhere (see the registered-binary check in ` + "`plumb doctor`" + `).`,
	RunE: runSetupAll,
}

var setupAllFlag bool

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

var (
	setupClaudeCodeProjectFlag bool
	setupClaudeCodeNoSkillFlag bool
)

var setupClaudeCodeCmd = &cobra.Command{
	Use:   "claude-code",
	Short: "Register plumb as an MCP server in Claude Code's config",
	Long: `Register plumb as an MCP server in Claude Code (the CLI tool).

By default writes to the user-level config (~/.claude.json), which makes plumb
available in every project. Use --project to write to .mcp.json in the current
directory instead, scoping plumb to that project only.`,
	RunE: runSetupClaudeCode,
}

var setupGeminiCmd = &cobra.Command{
	Use:   "gemini",
	Short: "Register plumb as an MCP server in Gemini CLI's config",
	RunE:  runSetupGemini,
}

var setupCodexCmd = &cobra.Command{
	Use:   "codex",
	Short: "Register plumb as an MCP server in Codex's config",
	RunE:  runSetupCodex,
}

func init() {
	setupCmd.Flags().BoolVar(&setupAllFlag, "all", false,
		"Repoint every already-registered client at the current plumb binary")
	setupCmd.AddCommand(setupClaudeDesktopCmd)
	setupClaudeCodeCmd.Flags().BoolVar(&setupClaudeCodeProjectFlag, "project", false, "Write to .mcp.json in the current directory (project-scoped)")
	setupClaudeCodeCmd.Flags().BoolVar(&setupClaudeCodeNoSkillFlag, "no-skill", false, "Skip installing Claude Code skill files")
	setupCmd.AddCommand(setupClaudeCodeCmd)
	setupCmd.AddCommand(setupGeminiCmd)
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
			lines = append(lines, fmt.Sprintf("%s: already current", render.ContractPath(cfgPath)))
			continue
		}
		changed++
		lines = append(lines, fmt.Sprintf("%s: registered", render.ContractPath(cfgPath)))
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

func runSetupGemini(_ *cobra.Command, _ []string) error {
	PrintLogo()
	cfgPath, err := GeminiConfigPath()
	if err != nil {
		return fmt.Errorf("locating Gemini CLI config: %w", err)
	}

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	added, preserved, err := setupClaudeDesktopInto(cfgPath, plumbBin)
	if err != nil {
		return err
	}

	if !added {
		fmt.Println("plumb is already registered in Gemini CLI — no changes made.")
		fmt.Printf("Config: %s\n", cfgPath)
		return nil
	}

	ctxStr := fmt.Sprintf("Registered in %s\nBinary: %s", cfgPath, plumbBin)
	if len(preserved) > 0 {
		ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preserved)
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
	fmt.Println("\nRestart Gemini CLI to apply the change.")
	return nil
}

func runSetupCodex(_ *cobra.Command, _ []string) error {
	PrintLogo()
	cfgPath, err := CodexConfigPath()
	if err != nil {
		return fmt.Errorf("locating Codex config: %w", err)
	}

	plumbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving plumb binary path: %w", err)
	}

	added, preserved, err := setupCodexInto(cfgPath, plumbBin)
	if err != nil {
		return err
	}

	if !added {
		fmt.Println("plumb is already registered in Codex — no changes made.")
		fmt.Printf("Config: %s\n", cfgPath)
		return nil
	}

	ctxStr := fmt.Sprintf("Registered in Codex\nConfig: %s\nBinary: %s", cfgPath, plumbBin)
	if len(preserved) > 0 {
		ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preserved)
	}

	tui.RebuildStyles()
	fmt.Println(render.ContextBox(tui.MutedStyle.Render(ctxStr), tui.SepStyle))
	fmt.Println("\nRestart Codex to apply the change.")
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

	if !setupClaudeCodeNoSkillFlag {
		installAndPrintSkills()
	}
	return nil
}

// installAndPrintSkills installs the embedded Claude Code skill files to the
// user's ~/.claude/skills directory and prints one line per skill action.
// Errors are non-fatal — a skill install failure does not abort setup.
func installAndPrintSkills() {
	skillsDir, err := claudeSkillsDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: resolving Claude skills directory: %v\n", err)
		return
	}
	for _, skill := range claudeCodeSkills() {
		action, err := installSkill(skillsDir, skill.Name, skill.Content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: installing skill %q: %v\n", skill.Name, err)
			continue
		}
		if action != "unchanged" {
			fmt.Printf("Skill %-20s %s → %s\n", skill.Name,
				action, filepath.Join(skillsDir, skill.Name, "SKILL.md"))
		}
	}
}

// setupClaudeCodeInto merges the plumb entry into a Claude Code config file.
// Claude Code requires a "type":"stdio" field that Claude Desktop does not.
func setupClaudeCodeInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "mcpServers", readOrInitClaudeConfig, writeJSON,
		map[string]any{"type": "stdio", "command": plumbBin, "args": []string{"serve"}},
		func(existing map[string]any) bool { return existing["command"] == plumbBin },
	)
}

// setupCodexInto merges the plumb entry into Codex's TOML config.
func setupCodexInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	return mergeServerEntry(cfgPath, "mcp_servers", readOrInitCodexConfig, writeTOML,
		map[string]any{"command": plumbBin, "args": []string{"serve"}},
		func(existing map[string]any) bool {
			return existing["command"] == plumbBin && stringSliceEqual(existing["args"], []string{"serve"})
		},
	)
}
