package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/tui"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure plumb with external tools",
}

var setupClaudeDesktopCmd = &cobra.Command{
	Use:   "claude-desktop",
	Short: "Register plumb as an MCP server in Claude Desktop's config",
	RunE:  runSetupClaudeDesktop,
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
	setupCmd.AddCommand(setupClaudeDesktopCmd)
	setupClaudeCodeCmd.Flags().BoolVar(&setupClaudeCodeProjectFlag, "project", false, "Write to .mcp.json in the current directory (project-scoped)")
	setupCmd.AddCommand(setupClaudeCodeCmd)
	setupCmd.AddCommand(setupGeminiCmd)
	setupCmd.AddCommand(setupCodexCmd)
}

func runSetupClaudeDesktop(_ *cobra.Command, _ []string) error {
	PrintLogo()
	cfgPath, err := claudeDesktopConfigPath()
	if err != nil {
		return fmt.Errorf("locating Claude Desktop config: %w", err)
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
		fmt.Println("plumb is already registered in Claude Desktop — no changes made.")
		fmt.Printf("Config: %s\n", cfgPath)
		return nil
	}

	ctxStr := fmt.Sprintf("Registered in %s\nBinary: %s", cfgPath, plumbBin)
	if len(preserved) > 0 {
		ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preserved)
	}

	tui.RebuildStyles()
	ctxBox := lipgloss.NewStyle().
		Border(ContextBorder, false, false, false, true).
		BorderForeground(tui.SepStyle.GetForeground()).
		PaddingLeft(1).
		Render(tui.MutedStyle.Render(ctxStr))
	fmt.Println(ctxBox)
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
	ctxBox := lipgloss.NewStyle().
		Border(ContextBorder, false, false, false, true).
		BorderForeground(tui.SepStyle.GetForeground()).
		PaddingLeft(1).
		Render(tui.MutedStyle.Render(ctxStr))
	fmt.Println(ctxBox)
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
	ctxBox := lipgloss.NewStyle().
		Border(ContextBorder, false, false, false, true).
		BorderForeground(tui.SepStyle.GetForeground()).
		PaddingLeft(1).
		Render(tui.MutedStyle.Render(ctxStr))
	fmt.Println(ctxBox)
	fmt.Println("\nRestart Codex to apply the change.")
	return nil
}

// setupClaudeDesktopInto merges the plumb entry into the Claude Desktop config
// at cfgPath without disturbing any other entries. Returns added=false when
// plumb was already registered with the same binary (no write performed).
// preserved lists the names of servers that were already present and kept.
func setupClaudeDesktopInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	cfg, isNew, err := readOrInitClaudeConfig(cfgPath)
	if err != nil {
		return false, nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}

	if cfg["mcpServers"] == nil {
		cfg["mcpServers"] = map[string]any{}
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return false, nil, fmt.Errorf("mcpServers in %s is not an object — cannot safely modify it", cfgPath)
	}

	// Collect preserved servers (existing, not plumb) for reporting.
	for name := range servers {
		if name != "plumb" {
			preserved = append(preserved, name)
		}
	}
	sort.Strings(preserved)

	// Idempotency check: if plumb is already registered with the same binary, skip the write.
	if existing, exists := servers["plumb"].(map[string]any); exists {
		if existing["command"] == plumbBin {
			return false, preserved, nil
		}
	}

	// Back up the original file before modifying it.
	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}

	servers["plumb"] = map[string]any{
		"command": plumbBin,
		"args":    []string{"serve"},
	}

	if err := writeJSON(cfgPath, cfg); err != nil {
		return false, nil, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, preserved, nil
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
		return nil
	}

	ctxStr := fmt.Sprintf("Registered in Claude Code (%s config)\nConfig: %s\nBinary: %s", scope, cfgPath, plumbBin)
	if len(preserved) > 0 {
		ctxStr += fmt.Sprintf("\nPreserved existing MCP servers: %v", preserved)
	}

	tui.RebuildStyles()
	ctxBox := lipgloss.NewStyle().
		Border(ContextBorder, false, false, false, true).
		BorderForeground(tui.SepStyle.GetForeground()).
		PaddingLeft(1).
		Render(tui.MutedStyle.Render(ctxStr))
	fmt.Println(ctxBox)
	fmt.Println("\nReload Claude Code (or open a new session) to apply the change.")
	return nil
}

// setupClaudeCodeInto merges the plumb entry into a Claude Code config file.
// Claude Code requires a "type":"stdio" field that Claude Desktop does not.
func setupClaudeCodeInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	cfg, isNew, err := readOrInitClaudeConfig(cfgPath)
	if err != nil {
		return false, nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}

	if cfg["mcpServers"] == nil {
		cfg["mcpServers"] = map[string]any{}
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return false, nil, fmt.Errorf("mcpServers in %s is not an object — cannot safely modify it", cfgPath)
	}

	for name := range servers {
		if name != "plumb" {
			preserved = append(preserved, name)
		}
	}
	sort.Strings(preserved)

	if existing, exists := servers["plumb"].(map[string]any); exists {
		if existing["command"] == plumbBin {
			return false, preserved, nil
		}
	}

	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}

	servers["plumb"] = map[string]any{
		"type":    "stdio",
		"command": plumbBin,
		"args":    []string{"serve"},
	}

	if err := writeJSON(cfgPath, cfg); err != nil {
		return false, nil, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, preserved, nil
}

// setupCodexInto merges the plumb entry into Codex's TOML config.
func setupCodexInto(cfgPath, plumbBin string) (added bool, preserved []string, err error) {
	cfg, isNew, err := readOrInitCodexConfig(cfgPath)
	if err != nil {
		return false, nil, fmt.Errorf("reading %s: %w", cfgPath, err)
	}

	if cfg["mcp_servers"] == nil {
		cfg["mcp_servers"] = map[string]any{}
	}
	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		return false, nil, fmt.Errorf("mcp_servers in %s is not an object — cannot safely modify it", cfgPath)
	}

	for name := range servers {
		if name != "plumb" {
			preserved = append(preserved, name)
		}
	}
	sort.Strings(preserved)

	if existing, exists := servers["plumb"].(map[string]any); exists {
		if existing["command"] == plumbBin && stringSliceEqual(existing["args"], []string{"serve"}) {
			return false, preserved, nil
		}
	}

	if !isNew {
		if err := backupFile(cfgPath); err != nil {
			return false, nil, fmt.Errorf("backing up %s: %w", cfgPath, err)
		}
	}

	servers["plumb"] = map[string]any{
		"command": plumbBin,
		"args":    []string{"serve"},
	}

	if err := writeTOML(cfgPath, cfg); err != nil {
		return false, nil, fmt.Errorf("writing %s: %w", cfgPath, err)
	}
	return true, preserved, nil
}
