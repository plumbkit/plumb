package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/spf13/cobra"
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

func init() {
	setupCmd.AddCommand(setupClaudeDesktopCmd)
	setupClaudeCodeCmd.Flags().BoolVar(&setupClaudeCodeProjectFlag, "project", false, "Write to .mcp.json in the current directory (project-scoped)")
	setupCmd.AddCommand(setupClaudeCodeCmd)
}

func runSetupClaudeDesktop(_ *cobra.Command, _ []string) error {
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

	fmt.Printf("Registered plumb in %s\n", cfgPath)
	fmt.Printf("Binary: %s\n", plumbBin)
	if len(preserved) > 0 {
		fmt.Printf("Preserved existing MCP servers: %v\n", preserved)
	}
	fmt.Println("Restart Claude Desktop to apply the change.")
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

	fmt.Printf("Registered plumb in Claude Code (%s config)\n", scope)
	fmt.Printf("Config:  %s\n", cfgPath)
	fmt.Printf("Binary:  %s\n", plumbBin)
	if len(preserved) > 0 {
		fmt.Printf("Preserved existing MCP servers: %v\n", preserved)
	}
	fmt.Println("Reload Claude Code (or open a new session) to apply the change.")
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

// claudeCodeConfigPath returns the user-level Claude Code config path.
func claudeCodeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// backupFile copies src to src.<timestamp>.bak in the same directory.
func backupFile(src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	dst := src + "." + stamp + ".bak"
	return os.WriteFile(dst, data, 0o600)
}

// claudeDesktopConfigPath returns the platform-specific path for
// claude_desktop_config.json. It does not check whether the file exists.
func claudeDesktopConfigPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("APPDATA environment variable not set")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	default:
		// Unofficial Linux path — Claude Desktop isn't fully supported there yet.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

// readOrInitClaudeConfig reads cfgPath as JSON into a generic map.
// isNew is true when the file did not exist (empty map returned for first run).
// Any read or parse error is returned — never silently discarded.
func readOrInitClaudeConfig(path string) (m map[string]any, isNew bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, false, fmt.Errorf("creating directory: %w", err)
		}
		return map[string]any{}, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing %s as JSON: %w — will not overwrite", path, err)
	}
	return m, false, nil
}

// writeJSON writes m to path as indented JSON, creating the file if needed.
// It writes to a temp file in the same directory and renames atomically.
func writeJSON(path string, m map[string]any) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".plumb_setup_*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
