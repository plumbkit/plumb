package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/alecthomas/chroma/v2/quick"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
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

		// Use chroma to highlight TOML if stdout is a terminal, else just print it.
		chromaStyle := "nord"
		if t, ok := tui.AvailableThemes[cfg.UI.Theme]; ok && t.ChromaStyle != "" {
			chromaStyle = t.ChromaStyle
		}
		if fileInfo, _ := os.Stdout.Stat(); (fileInfo.Mode() & os.ModeCharDevice) != 0 {
			if err := quick.Highlight(os.Stdout, buf.String(), "toml", "terminal256", chromaStyle); err != nil {
				fmt.Print(buf.String()) // fallback
			}
		} else {
			fmt.Print(buf.String())
		}
		return nil
	},
}

var (
	configShowWorkspace string
	configShowAdapters  bool
)

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show resolved configuration with source provenance",
	Long: `Print the resolved configuration as plumb actually sees it, with each
layer (defaults → global → project → env) labelled so you can tell where
each value came from. Pass --workspace to include a project-local
.plumb/config.toml in the merge.

Pass --adapters to print only the language-server adapter table (language,
server binary, validation tier, and live activation state). Aliases: --adapter,
--lsp, --lsps, --integration, --integrations.`,
	RunE: runConfigShow,
}

var configReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Tell the running daemon to re-read the global config now",
	Long: `Force the running plumb daemon to reload its global config immediately,
rather than waiting for the file watcher. Live-reloadable settings (edits, git,
walk, log level, topology, cache) take effect at once; settings that still need a
restart are flagged by 'plumb config show'.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		resp, err := dialDaemonCtrl("reload-config")
		if err != nil {
			return err
		}
		if msg, ok := strings.CutPrefix(resp, "error:"); ok {
			return fmt.Errorf("%s", strings.TrimSpace(msg))
		}
		fmt.Println("daemon config reloaded")
		return nil
	},
}

func init() {
	configShowCmd.Flags().StringVar(&configShowWorkspace, "workspace", "",
		"Workspace directory to merge .plumb/config.toml from (defaults to current dir)")
	configShowCmd.Flags().BoolVar(&configShowAdapters, "adapters", false,
		"Print only the language-server adapter table (aliases: --adapter, --lsp, --lsps, --integration, --integrations)")
	configShowCmd.Flags().SetNormalizeFunc(normaliseAdapterFlag)
	configCmd.AddCommand(configPrintCmd, configShowCmd, configReloadCmd)
}

func runConfigShow(_ *cobra.Command, _ []string) error {
	ws := configShowWorkspace
	if ws == "" {
		ws = "."
	}

	defaultsCfg := config.Defaults()
	globalCfg, gerr := config.Load()
	if gerr != nil {
		return fmt.Errorf("loading global config: %w", gerr)
	}
	requestedWorkspace, _ := filepath.Abs(ws)
	resolvedWorkspace, workspaceAttachable, rerr := resolveCLIWorkspaceDetailed(ws, globalCfg)
	if rerr != nil {
		return rerr
	}
	ws = resolvedWorkspace
	projectCfg, perr := config.LoadProject(globalCfg, ws)
	if perr != nil {
		return fmt.Errorf("loading project config: %w", perr)
	}

	tui.RebuildStyles()
	PrintLogo()
	printRecoveredHijacks()

	if configShowAdapters {
		printAdaptersView(projectCfg)
		return nil
	}

	tableBase := configShowTableBase

	// 1. Workspace Context
	fmt.Printf("Workspace Context\n")
	ctxTable := tableBase().Headers("Context", "Exists", "Path").StyleFunc(configShowColStyle(1))

	globalPath := config.GlobalConfigPath()
	projectPath := config.ProjectConfigPath(ws)

	ctxTable.Row("global config", existsIcon(globalPath), contractConfigPath(globalPath))
	ctxTable.Row("project config", existsIcon(projectPath), contractConfigPath(projectPath))
	if requestedWorkspace != "" && requestedWorkspace != ws {
		ctxTable.Row("requested workspace", tui.OkStyle.Render("✓"), contractConfigPath(requestedWorkspace))
	}
	workspaceIcon := tui.OkStyle.Render("✓")
	if !workspaceAttachable {
		workspaceIcon = tui.WarnStyle.Render("!")
	}
	ctxTable.Row("workspace", workspaceIcon, contractConfigPath(ws))
	fmt.Println(renderConfigShowTable(ctxTable))
	if !workspaceAttachable {
		fmt.Println(tui.WarnStyle.Render(
			"! not an attachable workspace — no .plumb/ marker, language root marker, or .git/ above it; " +
				"the daemon would refuse to attach here (run `plumb init`, or enable [workspace].auto_attach)"))
	}

	// 1b. Directories — where plumb keeps its runtime, data, and log files.
	printDirectoriesSection()

	// 2. MCP Integration Status
	fmt.Printf("\nMCP Integration Status\n")

	mcpTable := configShowTableBase().
		Headers("Client", "Exists", "Registered", "Path").
		StyleFunc(configShowColStyle(1, 2))

	for _, c := range allSetupClients() {
		path, _ := c.pathFn()
		mcpTable.Row(c.name, existsIcon(path), registeredIcon(path), contractConfigPath(path))
	}
	fmt.Println(renderConfigShowTable(mcpTable))

	// 3. Plumb Configuration
	fmt.Printf("\nPlumb Configuration\n")
	cfgTable := tableBase()

	logFileDisplay := projectCfg.LogFile
	if logFileDisplay == "" {
		logFileDisplay = contractConfigPath(daemonLogPath())
	}

	addConfigSection(cfgTable, "core", [][]string{
		{"log_level", projectCfg.LogLevel, sourceFor("log_level", defaultsCfg.LogLevel, globalCfg.LogLevel, projectCfg.LogLevel)},
		{"log_format", projectCfg.LogFormat, sourceFor("log_format", defaultsCfg.LogFormat, globalCfg.LogFormat, projectCfg.LogFormat)},
		{"log_file", logFileDisplay, sourceFor("log_file", defaultsCfg.LogFile, globalCfg.LogFile, projectCfg.LogFile)},
	})

	addConfigSection(cfgTable, "cache", [][]string{
		{"ttl", projectCfg.Cache.TTL.String(), sourceFor("ttl", defaultsCfg.Cache.TTL, globalCfg.Cache.TTL, projectCfg.Cache.TTL)},
		{"max_size", fmt.Sprintf("%d", projectCfg.Cache.MaxSize), sourceFor("max_size", defaultsCfg.Cache.MaxSize, globalCfg.Cache.MaxSize, projectCfg.Cache.MaxSize)},
	})

	addConfigSection(cfgTable, "edits", [][]string{
		{"strict", fmt.Sprintf("%v", projectCfg.Edits.Strict), sourceFor("strict", defaultsCfg.Edits.Strict, globalCfg.Edits.Strict, projectCfg.Edits.Strict)},
		{"rate_limit_per_minute", fmt.Sprintf("%d", projectCfg.Edits.RateLimitPerMinute), sourceFor("rate_limit_per_minute", defaultsCfg.Edits.RateLimitPerMinute, globalCfg.Edits.RateLimitPerMinute, projectCfg.Edits.RateLimitPerMinute)},
		{"post_write_diagnostics_ms", fmt.Sprintf("%d", projectCfg.Edits.PostWriteDiagnosticsMs), sourceFor("post_write_diagnostics_ms", defaultsCfg.Edits.PostWriteDiagnosticsMs, globalCfg.Edits.PostWriteDiagnosticsMs, projectCfg.Edits.PostWriteDiagnosticsMs)},
	})

	addConfigSection(cfgTable, "walk", [][]string{
		{"refuse_home_roots", fmt.Sprintf("%v", projectCfg.Walk.RefuseHomeRoots), sourceFor("refuse_home_roots", defaultsCfg.Walk.RefuseHomeRoots, globalCfg.Walk.RefuseHomeRoots, projectCfg.Walk.RefuseHomeRoots)},
	})

	addConfigSection(cfgTable, "workspace", [][]string{
		{"auto_attach", fmt.Sprintf("%v", projectCfg.Workspace.AutoAttach), sourceFor("auto_attach", defaultsCfg.Workspace.AutoAttach, globalCfg.Workspace.AutoAttach, projectCfg.Workspace.AutoAttach)},
		{"auto_attach_persist", fmt.Sprintf("%v", projectCfg.Workspace.AutoAttachPersist), sourceFor("auto_attach_persist", defaultsCfg.Workspace.AutoAttachPersist, globalCfg.Workspace.AutoAttachPersist, projectCfg.Workspace.AutoAttachPersist)},
	})

	addConfigSection(cfgTable, "git", [][]string{
		{"allow_writes", fmt.Sprintf("%v", projectCfg.Git.AllowWrites), sourceFor("allow_writes", defaultsCfg.Git.AllowWrites, globalCfg.Git.AllowWrites, projectCfg.Git.AllowWrites)},
		{"allow_destructive", fmt.Sprintf("%v", projectCfg.Git.AllowDestructive), sourceFor("allow_destructive", defaultsCfg.Git.AllowDestructive, globalCfg.Git.AllowDestructive, projectCfg.Git.AllowDestructive)},
		{"allow_push", fmt.Sprintf("%v", projectCfg.Git.AllowPush), sourceFor("allow_push", defaultsCfg.Git.AllowPush, globalCfg.Git.AllowPush, projectCfg.Git.AllowPush)},
		{"protected_branches", fmt.Sprintf("%v", projectCfg.Git.ProtectedBranches), sourceFor("protected_branches", defaultsCfg.Git.ProtectedBranches, globalCfg.Git.ProtectedBranches, projectCfg.Git.ProtectedBranches)},
	})

	addConfigSection(cfgTable, "lsp_query", [][]string{
		{"timeout", projectCfg.LSPQuery.Timeout.String(), sourceFor("timeout", defaultsCfg.LSPQuery.Timeout, globalCfg.LSPQuery.Timeout, projectCfg.LSPQuery.Timeout)},
	})

	sem, gsem, dsem := projectCfg.Semantics, globalCfg.Semantics, defaultsCfg.Semantics
	addConfigSection(cfgTable, "semantics", [][]string{
		{"enabled", fmt.Sprintf("%v", sem.Enabled), sourceFor("enabled", dsem.Enabled, gsem.Enabled, sem.Enabled)},
		{"provider", sem.Provider, sourceFor("provider", dsem.Provider, gsem.Provider, sem.Provider)},
		{"model", sem.Model, sourceFor("model", dsem.Model, gsem.Model, sem.Model)},
		{"base_url", sem.BaseURL, sourceFor("base_url", dsem.BaseURL, gsem.BaseURL, sem.BaseURL)},
		{"api_key", maskConfigKey(sem.APIKey), sourceFor("api_key", dsem.APIKey, gsem.APIKey, sem.APIKey)},
		{"api_key_env", sem.APIKeyEnv, sourceFor("api_key_env", dsem.APIKeyEnv, gsem.APIKeyEnv, sem.APIKeyEnv)},
		{"rerank_candidates", fmt.Sprintf("%d", sem.RerankCandidates), sourceFor("rerank_candidates", dsem.RerankCandidates, gsem.RerankCandidates, sem.RerankCandidates)},
		{"timeout", sem.Timeout.String(), sourceFor("timeout", dsem.Timeout, gsem.Timeout, sem.Timeout)},
	})

	mem, gmem, dmem := projectCfg.Memory, globalCfg.Memory, defaultsCfg.Memory
	addConfigSection(cfgTable, "memory", [][]string{
		{"enabled", fmt.Sprintf("%v", mem.Enabled), sourceFor("enabled", dmem.Enabled, gmem.Enabled, mem.Enabled)},
		{"generated_summaries", fmt.Sprintf("%v", mem.GeneratedSummaries), sourceFor("generated_summaries", dmem.GeneratedSummaries, gmem.GeneratedSummaries, mem.GeneratedSummaries)},
		{"inject_hints", fmt.Sprintf("%v", mem.InjectHints), sourceFor("inject_hints", dmem.InjectHints, gmem.InjectHints, mem.InjectHints)},
		{"hint_budget_bytes", fmt.Sprintf("%d", mem.HintBudgetBytes), sourceFor("hint_budget_bytes", dmem.HintBudgetBytes, gmem.HintBudgetBytes, mem.HintBudgetBytes)},
		{"episodic_budget_bytes", fmt.Sprintf("%d", mem.EpisodicBudgetBytes), sourceFor("episodic_budget_bytes", dmem.EpisodicBudgetBytes, gmem.EpisodicBudgetBytes, mem.EpisodicBudgetBytes)},
		{"max_hints", fmt.Sprintf("%d", mem.MaxHints), sourceFor("max_hints", dmem.MaxHints, gmem.MaxHints, mem.MaxHints)},
		{"idle_summary_minutes", fmt.Sprintf("%d", mem.IdleSummaryMinutes), sourceFor("idle_summary_minutes", dmem.IdleSummaryMinutes, gmem.IdleSummaryMinutes, mem.IdleSummaryMinutes)},
		{"generated_memory_keep", fmt.Sprintf("%d", mem.GeneratedMemoryKeep), sourceFor("generated_memory_keep", dmem.GeneratedMemoryKeep, gmem.GeneratedMemoryKeep, mem.GeneratedMemoryKeep)},
	})

	for _, lang := range sortedLSPKeys(projectCfg.LSP) {
		cfg := projectCfg.LSP[lang]
		globCfg := globalCfg.LSP[lang]
		defCfg := defaultsCfg.LSP[lang]

		addConfigSection(cfgTable, "lsp."+lang, [][]string{
			{"enabled", fmt.Sprintf("%v", cfg.Enabled), sourceFor("enabled", defCfg.Enabled, globCfg.Enabled, cfg.Enabled)},
			{"active", lspActiveStatus(cfg), "derived"},
			{"command", cfg.Command, sourceFor("command", defCfg.Command, globCfg.Command, cfg.Command)},
			{"args", fmt.Sprintf("%v", cfg.Args), sourceFor("args", defCfg.Args, globCfg.Args, cfg.Args)},
			{"root_markers", fmt.Sprintf("%v", cfg.RootMarkers), sourceFor("root_markers", defCfg.RootMarkers, globCfg.RootMarkers, cfg.RootMarkers)},
			{"weak_root_markers", fmt.Sprintf("%v", cfg.WeakRootMarkers), sourceFor("weak_root_markers", defCfg.WeakRootMarkers, globCfg.WeakRootMarkers, cfg.WeakRootMarkers)},
			{"env", fmt.Sprintf("%v", cfg.Env), sourceFor("env", defCfg.Env, globCfg.Env, cfg.Env)},
		})
	}

	fmt.Println(renderConfigShowTable(cfgTable))

	// 4. Reload behaviour — which groups the running daemon applies live versus
	// those that need a restart. Mirrors config.RestartSensitiveEqual; the daemon
	// reports a concrete restart-pending state via the daemon_info tool.
	fmt.Printf("\nReload behaviour\n")
	reloadTable := tableBase().Headers("Config group", "Applies")
	reloadTable.Row("edits, git, walk", tui.OkStyle.Render("live"))
	reloadTable.Row("log_level", tui.OkStyle.Render("live (set-level)"))
	reloadTable.Row("ui.theme", tui.OkStyle.Render("live (TUI)"))
	reloadTable.Row("topology", tui.OkStyle.Render("live (reconciled)"))
	reloadTable.Row("workspace, quality, lsp_query", tui.OkStyle.Render("live on next attach/session"))
	reloadTable.Row("lsp.* servers, cache, log_format", tui.WarnStyle.Render("needs daemon restart"))
	fmt.Println(renderConfigShowTable(reloadTable))
	fmt.Println()

	printAgentProvenance(ws)
	return nil
}

// printDirectoriesSection lists the base directories plumb resolves through
// internal/paths — config, data, state, logs, and the runtime/cache dir — so a
// user can find the daemon log folder, stats database, and socket without
// reading the source. Paths are resolved (not created) here, so a directory the
// daemon has not yet materialised shows as missing.
func printDirectoriesSection() {
	fmt.Printf("\nDirectories\n")
	dirTable := configShowTableBase().Headers("Directory", "Exists", "Path", "Holds").
		StyleFunc(configShowColStyle(1))

	rows := []struct{ name, path, holds string }{
		{"config", paths.ConfigDir(), "config.toml"},
		{"data", paths.DataDir(), "sessions, stats.db"},
		{"state", paths.StateDir(), "regenerable state"},
		{"logs", paths.LogDir(), "daemon.log"},
		{"runtime", paths.CacheDir(), "socket, pid, locks, version"},
	}
	for _, r := range rows {
		dirTable.Row(r.name, existsIcon(r.path), contractConfigPath(r.path), tui.MutedStyle.Render(r.holds))
	}
	fmt.Println(renderConfigShowTable(dirTable))
}

func formatConfigVal(val string) string {
	if val == "" {
		return tui.MutedStyle.Render("(none)")
	}
	return tui.ValStyle.Render(val)
}

func addConfigSection(t *table.Table, name string, items [][]string) {
	var keys, vals, provs strings.Builder
	for i, item := range items {
		if i > 0 {
			keys.WriteString("\n")
			vals.WriteString("\n")
			provs.WriteString("\n")
		}
		keys.WriteString(item[0])
		vals.WriteString(formatConfigVal(item[1]))
		provs.WriteString(tui.MutedStyle.Render(item[2]))
	}
	t.Row(tui.KeyStyle.Render(name), keys.String(), vals.String(), provs.String())
}

func sortedLSPKeys(m map[string]config.LSPConfig) []string {
	keys := make([]string, 0, len(m))
	for lang := range m {
		keys = append(keys, lang)
	}
	sort.Strings(keys)
	return keys
}

var configShowBorder = lipgloss.Border{
	Top:          "─",
	Bottom:       "╌",
	Left:         "│",
	Right:        "│",
	TopLeft:      "╭",
	TopRight:     "╮",
	BottomLeft:   "╰",
	BottomRight:  "╯",
	Middle:       "┼",
	MiddleTop:    "┬",
	MiddleBottom: "┴",
	MiddleLeft:   "├",
	MiddleRight:  "┤",
}

func configShowTableBase() *table.Table {
	return table.New().
		Border(configShowBorder).
		BorderStyle(tui.SepStyle).
		BorderRow(true).
		BorderColumn(true).
		StyleFunc(func(row, col int) lipgloss.Style {
			return lipgloss.NewStyle().Padding(0, 1)
		})
}

func renderConfigShowTable(t *table.Table) string {
	lines := strings.Split(t.Render(), "\n")
	if len(lines) == 0 {
		return ""
	}
	lines[len(lines)-1] = strings.ReplaceAll(lines[len(lines)-1], "╌", "─")
	return strings.Join(lines, "\n")
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

// maskConfigKey renders a literal API key for `config show` without leaking it:
// only whether one is set.
func maskConfigKey(k string) string {
	if k == "" {
		return ""
	}
	return "(set)"
}

func contractConfigPath(p string) string {
	if p == "" {
		return tui.MutedStyle.Render("(none)")
	}
	return render.ContractPath(p)
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
	case "auto_attach":
		return "PLUMB_AUTO_ATTACH"
	case "auto_attach_persist":
		return "PLUMB_AUTO_ATTACH_PERSIST"
	case "allow_writes":
		return "PLUMB_GIT_ALLOW_WRITES"
	case "allow_destructive":
		return "PLUMB_GIT_ALLOW_DESTRUCTIVE"
	case "allow_push":
		return "PLUMB_GIT_ALLOW_PUSH"
	case "timeout":
		return "PLUMB_LSP_QUERY_TIMEOUT"
	}
	return ""
}
