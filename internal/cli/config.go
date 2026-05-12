package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"

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

	globalPath := config.GlobalConfigPath()
	projectPath := config.ProjectConfigPath(ws)

	tui.RebuildStyles()

	fmt.Printf("\nWorkspace Context\n\n")

	// Print paths block without borders
	printPath := func(label, path string, exists bool) {
		mark := tui.OkStyle.Render("✓")
		if !exists {
			mark = tui.WarnStyle.Render("✗")
		}
		if path == "" {
			path = tui.MutedStyle.Render("(none)")
		}
		
		// Pad the raw string first, then color it
		paddedLabel := fmt.Sprintf("%-15s", label)
		labelCol := lipgloss.NewStyle().Foreground(tui.ActiveTheme.Key).Render(paddedLabel)
		
		fmt.Printf("%s %s  %s\n", labelCol, mark, path)
	}

	_, gErr := os.Stat(globalPath)
	printPath("global config", globalPath, gErr == nil)
	
	_, pErr := os.Stat(projectPath)
	printPath("project config", projectPath, pErr == nil)
	
	// The workspace always exists if we got this far
	printPath("workspace", ws, true)

	fmt.Printf("\nPlumb Configuration\n\n")

	// Build the config table manually to get perfect section dividers
	type Section struct {
		Name  string
		Items [][]string // [key, val, prov]
	}

	var sections []Section

	sections = append(sections, Section{
		Name: "core",
		Items: [][]string{
			{"log_level", projectCfg.LogLevel, sourceFor("log_level", defaultsCfg.LogLevel, globalCfg.LogLevel, projectCfg.LogLevel)},
			{"log_file", projectCfg.LogFile, sourceFor("log_file", defaultsCfg.LogFile, globalCfg.LogFile, projectCfg.LogFile)},
		},
	})

	sections = append(sections, Section{
		Name: "cache",
		Items: [][]string{
			{"ttl", projectCfg.Cache.TTL.Duration.String(), sourceFor("ttl", defaultsCfg.Cache.TTL, globalCfg.Cache.TTL, projectCfg.Cache.TTL)},
			{"max_size", fmt.Sprintf("%d", projectCfg.Cache.MaxSize), sourceFor("max_size", defaultsCfg.Cache.MaxSize, globalCfg.Cache.MaxSize, projectCfg.Cache.MaxSize)},
		},
	})

	sections = append(sections, Section{
		Name: "edits",
		Items: [][]string{
			{"strict", fmt.Sprintf("%v", projectCfg.Edits.Strict), sourceFor("strict", defaultsCfg.Edits.Strict, globalCfg.Edits.Strict, projectCfg.Edits.Strict)},
			{"rate_limit_per_minute", fmt.Sprintf("%d", projectCfg.Edits.RateLimitPerMinute), sourceFor("rate_limit_per_minute", defaultsCfg.Edits.RateLimitPerMinute, globalCfg.Edits.RateLimitPerMinute, projectCfg.Edits.RateLimitPerMinute)},
		},
	})

	sections = append(sections, Section{
		Name: "walk",
		Items: [][]string{
			{"refuse_home_roots", fmt.Sprintf("%v", projectCfg.Walk.RefuseHomeRoots), sourceFor("refuse_home_roots", defaultsCfg.Walk.RefuseHomeRoots, globalCfg.Walk.RefuseHomeRoots, projectCfg.Walk.RefuseHomeRoots)},
		},
	})

	// Collect and sort LSP adapters
	var lspLangs []string
	for lang := range projectCfg.LSP {
		lspLangs = append(lspLangs, lang)
	}
	// Sort manually for determinism (or we can just append them, map iteration is random)
	for i := 0; i < len(lspLangs)-1; i++ {
		for j := i + 1; j < len(lspLangs); j++ {
			if lspLangs[i] > lspLangs[j] {
				lspLangs[i], lspLangs[j] = lspLangs[j], lspLangs[i]
			}
		}
	}

	for _, lang := range lspLangs {
		cfg := projectCfg.LSP[lang]
		globCfg := globalCfg.LSP[lang]
		defCfg := defaultsCfg.LSP[lang]

		argsStr := fmt.Sprintf("%v", cfg.Args)
		if len(cfg.Args) == 0 {
			argsStr = "[]"
		}
		
		markersStr := fmt.Sprintf("%v", cfg.RootMarkers)
		if len(cfg.RootMarkers) == 0 {
			markersStr = "[]"
		}
		
		envStr := fmt.Sprintf("%v", cfg.Env)
		if len(cfg.Env) == 0 {
			envStr = "{}"
		}

		sections = append(sections, Section{
			Name: "lsp." + lang,
			Items: [][]string{
				{"enabled", fmt.Sprintf("%v", cfg.Enabled), sourceFor("enabled", defCfg.Enabled, globCfg.Enabled, cfg.Enabled)},
				{"command", cfg.Command, sourceFor("command", defCfg.Command, globCfg.Command, cfg.Command)},
				{"args", argsStr, sourceFor("args", defCfg.Args, globCfg.Args, cfg.Args)},
				{"root_markers", markersStr, sourceFor("root_markers", defCfg.RootMarkers, globCfg.RootMarkers, cfg.RootMarkers)},
				{"env", envStr, sourceFor("env", defCfg.Env, globCfg.Env, cfg.Env)},
			},
		})
	}

	w0, w1, w2, w3 := 12, 24, 20, 16 // Min widths
	for _, sec := range sections {
		if lipgloss.Width(sec.Name) > w0 {
			w0 = lipgloss.Width(sec.Name)
		}
		for _, row := range sec.Items {
			if lipgloss.Width(row[0]) > w1 {
				w1 = lipgloss.Width(row[0])
			}
			if lipgloss.Width(row[1]) > w2 {
				w2 = lipgloss.Width(row[1])
			}
			if lipgloss.Width(row[2]) > w3 {
				w3 = lipgloss.Width(row[2])
			}
		}
	}

	// Helpers to draw table parts
	drawTop := func() {
		fmt.Println(tui.SepStyle.Render(fmt.Sprintf("╭%s┬%s┬%s┬%s╮", strings.Repeat("─", w0+2), strings.Repeat("─", w1+2), strings.Repeat("─", w2+2), strings.Repeat("─", w3+2))))
	}
	drawMid := func() {
		fmt.Println(tui.SepStyle.Render(fmt.Sprintf("├%s┼%s┼%s┼%s┤", strings.Repeat("─", w0+2), strings.Repeat("─", w1+2), strings.Repeat("─", w2+2), strings.Repeat("─", w3+2))))
	}
	drawBot := func() {
		fmt.Println(tui.SepStyle.Render(fmt.Sprintf("╰%s┴%s┴%s┴%s╯", strings.Repeat("─", w0+2), strings.Repeat("─", w1+2), strings.Repeat("─", w2+2), strings.Repeat("─", w3+2))))
	}

	drawTop()
	for sIdx, sec := range sections {
		if sIdx > 0 {
			drawMid()
		}
		for rIdx, row := range sec.Items {
			secName := ""
			if rIdx == 0 {
				secName = sec.Name
			}
			fmt.Printf("%s %-*s %s %-*s %s %-*s %s %-*s %s\n",
				tui.SepStyle.Render("│"),
				w0, tui.KeyStyle.Render(secName),
				tui.SepStyle.Render("│"),
				w1, row[0],
				tui.SepStyle.Render("│"),
				w2, tui.ValStyle.Render(row[1]),
				tui.SepStyle.Render("│"),
				w3, tui.MutedStyle.Render(row[2]),
				tui.SepStyle.Render("│"),
			)
		}
	}
	drawBot()

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
	return tui.WarnStyle.Render("✗ Not found") + tui.MutedStyle.Render(fmt.Sprintf(" (%s)", path))
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
	case "log_level":
		return "PLUMB_LOG_LEVEL"
	case "log_file":
		return "PLUMB_LOG_FILE"
	case "refuse_home_roots":
		return "PLUMB_REFUSE_HOME_ROOTS"
	}
	return ""
}
