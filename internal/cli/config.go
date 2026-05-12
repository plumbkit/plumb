package cli

import (
	"bytes"
	"fmt"
	"os"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/alecthomas/chroma/v2/quick"
	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/tui"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect plumb configuration",
}

var configPrintCmd = &cobra.Command{
	Use:   "print",
	Short: "Print the resolved configuration as TOML",
	RunE: func(_ *cobra.Command, _ []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		
		var buf bytes.Buffer
		if err := config.Print(cfg, &buf); err != nil {
			return err
		}

		// Use chroma to highlight TOML if stdout is a terminal, else just print it
		if fileInfo, _ := os.Stdout.Stat(); (fileInfo.Mode() & os.ModeCharDevice) != 0 {
			if err := quick.Highlight(os.Stdout, buf.String(), "toml", "terminal256", "nord"); err != nil {
				fmt.Print(buf.String()) // fallback
			}
		} else {
			fmt.Print(buf.String())
		}
		return nil
	},
}

var configShowWorkspace string

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show resolved configuration with source provenance",
	Long: `Print the resolved configuration as plumb actually sees it, with each
layer (defaults → global → project → env) labelled so you can tell where
each value came from. Pass --workspace to include a project-local
.plumb/config.toml in the merge.`,
	RunE: runConfigShow,
}

func init() {
	configShowCmd.Flags().StringVar(&configShowWorkspace, "workspace", "",
		"Workspace directory to merge .plumb/config.toml from (defaults to current dir)")
	configCmd.AddCommand(configPrintCmd, configShowCmd)
}

func runConfigShow(_ *cobra.Command, _ []string) error {
	ws := configShowWorkspace
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil {
			ws = cwd
		}
	}

	defaultsCfg := config.Defaults()
	globalCfg, gerr := config.Load()
	if gerr != nil {
		return fmt.Errorf("loading global config: %w", gerr)
	}
	projectCfg, perr := config.LoadProject(globalCfg, ws)
	if perr != nil {
		return fmt.Errorf("loading project config: %w", perr)
	}

	tui.RebuildStyles()

	fmt.Printf("\nPlumb Configuration\n\n")

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(tui.SepStyle).
		BorderRow(false).
		BorderColumn(true).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().Padding(0, 1)
			if col == 0 {
				return s.Inherit(tui.KeyStyle)
			}
			if col == 1 {
				return s.Inherit(tui.ValStyle)
			}
			return s.Inherit(tui.MutedStyle)
		})

	t.Row("edits", "strict", fmt.Sprintf("%v", projectCfg.Edits.Strict), sourceFor("strict", defaultsCfg.Edits.Strict, globalCfg.Edits.Strict, projectCfg.Edits.Strict))
	t.Row("", "rate_limit_per_minute", fmt.Sprintf("%d", projectCfg.Edits.RateLimitPerMinute), sourceFor("rate_limit_per_minute", defaultsCfg.Edits.RateLimitPerMinute, globalCfg.Edits.RateLimitPerMinute, projectCfg.Edits.RateLimitPerMinute))
	t.Row("cache", "ttl", projectCfg.Cache.TTL.Duration.String(), sourceFor("ttl", defaultsCfg.Cache.TTL, globalCfg.Cache.TTL, projectCfg.Cache.TTL))
	t.Row("", "max_size", fmt.Sprintf("%d", projectCfg.Cache.MaxSize), sourceFor("max_size", defaultsCfg.Cache.MaxSize, globalCfg.Cache.MaxSize, projectCfg.Cache.MaxSize))
	t.Row("core", "log_level", projectCfg.LogLevel, sourceFor("log_level", defaultsCfg.LogLevel, globalCfg.LogLevel, projectCfg.LogLevel))
	if projectCfg.LogFile != "" {
		t.Row("", "log_file", projectCfg.LogFile, sourceFor("log_file", defaultsCfg.LogFile, globalCfg.LogFile, projectCfg.LogFile))
	}

	fmt.Println(t.Render())

	fmt.Printf("\nMCP Integration Status\n\n")

	mcpTable := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(tui.SepStyle).
		BorderRow(false).
		BorderColumn(true).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().Padding(0, 1)
			if col == 0 {
				return s.Inherit(tui.KeyStyle)
			}
			return s
		})

	claudeDesktopPath, _ := claudeDesktopConfigPath()
	geminiPath, _ := GeminiConfigPath()

	mcpTable.Row("Claude Desktop", integrationStatus(claudeDesktopPath))
	mcpTable.Row("Gemini CLI", integrationStatus(geminiPath))
	
	fmt.Println(mcpTable.Render())
	fmt.Println()

	return nil
}

func integrationStatus(path string) string {
	if path == "" {
		return tui.MutedStyle.Render("Not configured")
	}
	if _, err := os.Stat(path); err == nil {
		return tui.OkStyle.Render("✓ Registered") + tui.MutedStyle.Render(fmt.Sprintf(" (%s)", path))
	}
	return tui.MutedStyle.Render("Not found")
}

// sourceFor returns a short label naming the layer that supplied the
// current value. Comparison is order-sensitive: env > project > global > default.
func sourceFor(field string, def, global, final any) string {
	if v := envForField(field); v != "" {
		return fmt.Sprintf("env (%s=%s)", envVarForField(field), v)
	}
	switch {
	case fmt.Sprintf("%v", final) != fmt.Sprintf("%v", global):
		return "project config"
	case fmt.Sprintf("%v", global) != fmt.Sprintf("%v", def):
		return "global config"
	default:
		return "default"
	}
}

func envForField(field string) string {
	return os.Getenv(envVarForField(field))
}

func envVarForField(field string) string {
	switch field {
	case "strict":
		return "PLUMB_STRICT_EDITS"
	case "rate_limit_per_minute":
		return "PLUMB_WRITE_RATE_LIMIT"
	}
	return ""
}
