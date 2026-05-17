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

	tui.RebuildStyles()
	PrintLogo()

	tableBase := func() *table.Table {
		return table.New().
			Border(DottedBorder).
			BorderStyle(tui.SepStyle).
			BorderRow(true).
			BorderColumn(true).
			StyleFunc(func(row, col int) lipgloss.Style {
				return lipgloss.NewStyle().Padding(0, 1)
			})
	}

	// 1. Workspace Context
	fmt.Printf("Workspace Context\n")
	ctxTable := tableBase().Headers("Context", "Exists", "Path")

	globalPath := config.GlobalConfigPath()
	projectPath := config.ProjectConfigPath(ws)

	ctxTable.Row("global config", existsIcon(globalPath), contractConfigPath(globalPath))
	ctxTable.Row("project config", existsIcon(projectPath), contractConfigPath(projectPath))
	ctxTable.Row("workspace", tui.OkStyle.Render("✓"), contractConfigPath(ws))
	fmt.Println(ctxTable.Render())

	// 2. MCP Integration Status
	fmt.Printf("\nMCP Integration Status\n")

	mcpTable := table.New().
		Border(DottedBorder).
		BorderStyle(tui.SepStyle).
		BorderRow(true).
		BorderColumn(true).
		Headers("Client", "Exists", "Registered", "Path").
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				return s.Inherit(tui.HintStyle)
			}
			return s
		})

	claudeDesktopPath, _ := claudeDesktopConfigPath()
	claudeCodePath, _ := claudeCodeConfigPath()
	geminiPath, _ := GeminiConfigPath()
	codexPath, _ := CodexConfigPath()

	mcpTable.Row("Claude Code", existsIcon(claudeCodePath), registeredIcon(claudeCodePath), contractConfigPath(claudeCodePath))
	mcpTable.Row("Claude Desktop", existsIcon(claudeDesktopPath), registeredIcon(claudeDesktopPath), contractConfigPath(claudeDesktopPath))
	mcpTable.Row("Codex", existsIcon(codexPath), registeredIcon(codexPath), contractConfigPath(codexPath))
	mcpTable.Row("Gemini CLI", existsIcon(geminiPath), registeredIcon(geminiPath), contractConfigPath(geminiPath))
	fmt.Println(mcpTable.Render())

	// 3. Plumb Configuration
	fmt.Printf("\nPlumb Configuration\n")
	cfgTable := tableBase()

	formatVal := func(val string) string {
		if val == "" {
			return tui.MutedStyle.Render("(none)")
		}
		return tui.ValStyle.Render(val)
	}

	addSection := func(name string, items [][]string) {
		var keys, vals, provs strings.Builder
		for i, item := range items {
			if i > 0 {
				keys.WriteString("\n")
				vals.WriteString("\n")
				provs.WriteString("\n")
			}
			keys.WriteString(item[0])
			vals.WriteString(formatVal(item[1]))
			provs.WriteString(tui.MutedStyle.Render(item[2]))
		}
		cfgTable.Row(tui.KeyStyle.Render(name), keys.String(), vals.String(), provs.String())
	}

	logFileDisplay := projectCfg.LogFile
	if logFileDisplay == "" {
		logFileDisplay = contractConfigPath(daemonLogPath())
	}

	addSection("core", [][]string{
		{"log_level", projectCfg.LogLevel, sourceFor("log_level", defaultsCfg.LogLevel, globalCfg.LogLevel, projectCfg.LogLevel)},
		{"log_format", projectCfg.LogFormat, sourceFor("log_format", defaultsCfg.LogFormat, globalCfg.LogFormat, projectCfg.LogFormat)},
		{"log_file", logFileDisplay, sourceFor("log_file", defaultsCfg.LogFile, globalCfg.LogFile, projectCfg.LogFile)},
	})

	addSection("cache", [][]string{
		{"ttl", projectCfg.Cache.TTL.Duration.String(), sourceFor("ttl", defaultsCfg.Cache.TTL, globalCfg.Cache.TTL, projectCfg.Cache.TTL)},
		{"max_size", fmt.Sprintf("%d", projectCfg.Cache.MaxSize), sourceFor("max_size", defaultsCfg.Cache.MaxSize, globalCfg.Cache.MaxSize, projectCfg.Cache.MaxSize)},
	})

	addSection("edits", [][]string{
		{"strict", fmt.Sprintf("%v", projectCfg.Edits.Strict), sourceFor("strict", defaultsCfg.Edits.Strict, globalCfg.Edits.Strict, projectCfg.Edits.Strict)},
		{"rate_limit_per_minute", fmt.Sprintf("%d", projectCfg.Edits.RateLimitPerMinute), sourceFor("rate_limit_per_minute", defaultsCfg.Edits.RateLimitPerMinute, globalCfg.Edits.RateLimitPerMinute, projectCfg.Edits.RateLimitPerMinute)},
		{"post_write_diagnostics_ms", fmt.Sprintf("%d", projectCfg.Edits.PostWriteDiagnosticsMs), sourceFor("post_write_diagnostics_ms", defaultsCfg.Edits.PostWriteDiagnosticsMs, globalCfg.Edits.PostWriteDiagnosticsMs, projectCfg.Edits.PostWriteDiagnosticsMs)},
	})

	addSection("walk", [][]string{
		{"refuse_home_roots", fmt.Sprintf("%v", projectCfg.Walk.RefuseHomeRoots), sourceFor("refuse_home_roots", defaultsCfg.Walk.RefuseHomeRoots, globalCfg.Walk.RefuseHomeRoots, projectCfg.Walk.RefuseHomeRoots)},
	})

	// Collect and sort LSP adapters
	var lspLangs []string
	for lang := range projectCfg.LSP {
		lspLangs = append(lspLangs, lang)
	}
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

		addSection("lsp."+lang, [][]string{
			{"enabled", fmt.Sprintf("%v", cfg.Enabled), sourceFor("enabled", defCfg.Enabled, globCfg.Enabled, cfg.Enabled)},
			{"command", cfg.Command, sourceFor("command", defCfg.Command, globCfg.Command, cfg.Command)},
			{"args", fmt.Sprintf("%v", cfg.Args), sourceFor("args", defCfg.Args, globCfg.Args, cfg.Args)},
			{"root_markers", fmt.Sprintf("%v", cfg.RootMarkers), sourceFor("root_markers", defCfg.RootMarkers, globCfg.RootMarkers, cfg.RootMarkers)},
			{"env", fmt.Sprintf("%v", cfg.Env), sourceFor("env", defCfg.Env, globCfg.Env, cfg.Env)},
		})
	}

	fmt.Println(cfgTable.Render())
	fmt.Println()

	return nil
}

func existsIcon(path string) string {
	if path == "" {
		return tui.MutedStyle.Render("-")
	}
	if _, err := os.Stat(path); err == nil {
		return tui.OkStyle.Render("✓")
	}
	return tui.WarnStyle.Render("✗")
}

func registeredIcon(path string) string {
	if path == "" {
		return tui.MutedStyle.Render("-")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tui.WarnStyle.Render("✗")
	}

	// A simple string search is robust enough for checking registration status
	// across the JSON schemas and Codex's TOML schema.
	if strings.Contains(string(data), "plumb") {
		return tui.OkStyle.Render("✓")
	}
	return tui.WarnStyle.Render("✗")
}

func contractConfigPath(p string) string {
	if p == "" {
		return tui.MutedStyle.Render("(none)")
	}
	home, err := os.UserHomeDir()
	if err == nil && strings.HasPrefix(p, home) {
		p = "~" + p[len(home):]
	}
	return p
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
	case "log_format":
		return "PLUMB_LOG_FORMAT"
	case "post_write_diagnostics_ms":
		return "PLUMB_POST_WRITE_DIAG_MS"
	}
	return ""
}
