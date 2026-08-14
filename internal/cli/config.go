package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/alecthomas/chroma/v2/quick"
	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/theme"
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
		if p, ok := theme.Get(cfg.UI.Theme); ok && p.ChromaStyle != "" {
			chromaStyle = p.ChromaStyle
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
	// What this project ASKED for in the capability-granting sections, which is
	// not the same question as what is in effect — the table below can only ever
	// show the latter. Errors are non-fatal: an unreadable project file is
	// already reported by the Workspace Context section and by doctor.
	policy, _ := config.ProjectPolicyStatusFor(ws)

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
		ctxTable.Row("requested workspace", configShowOkStyle().Render("✓"), contractConfigPath(requestedWorkspace))
	}
	workspaceIcon := configShowOkStyle().Render("✓")
	if !workspaceAttachable {
		workspaceIcon = configShowWarnStyle().Render("!")
	}
	ctxTable.Row("workspace", workspaceIcon, contractConfigPath(ws))
	fmt.Println(renderConfigShowTable(ctxTable))
	if !workspaceAttachable {
		fmt.Println(configShowWarnStyle().Render(
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
		{"max_size", strconv.Itoa(projectCfg.Cache.MaxSize), sourceFor("max_size", defaultsCfg.Cache.MaxSize, globalCfg.Cache.MaxSize, projectCfg.Cache.MaxSize)},
	})

	addConfigSection(cfgTable, "edits", [][]string{
		{"strict", strconv.FormatBool(projectCfg.Edits.Strict), sourceFor("strict", defaultsCfg.Edits.Strict, globalCfg.Edits.Strict, projectCfg.Edits.Strict)},
		{"block_dirty_writes", strconv.FormatBool(projectCfg.Edits.BlockDirtyWrites), sourceFor("block_dirty_writes", defaultsCfg.Edits.BlockDirtyWrites, globalCfg.Edits.BlockDirtyWrites, projectCfg.Edits.BlockDirtyWrites)},
		{"rate_limit_per_minute", strconv.Itoa(projectCfg.Edits.RateLimitPerMinute), sourceFor("rate_limit_per_minute", defaultsCfg.Edits.RateLimitPerMinute, globalCfg.Edits.RateLimitPerMinute, projectCfg.Edits.RateLimitPerMinute)},
		{"post_write_diagnostics_ms", strconv.Itoa(projectCfg.Edits.PostWriteDiagnosticsMs), sourceFor("post_write_diagnostics_ms", defaultsCfg.Edits.PostWriteDiagnosticsMs, globalCfg.Edits.PostWriteDiagnosticsMs, projectCfg.Edits.PostWriteDiagnosticsMs)},
		{"post_write_cross_file", strconv.FormatBool(projectCfg.Edits.PostWriteCrossFile), sourceFor("post_write_cross_file", defaultsCfg.Edits.PostWriteCrossFile, globalCfg.Edits.PostWriteCrossFile, projectCfg.Edits.PostWriteCrossFile)},
		{"post_write_cross_file_settle_ms", strconv.Itoa(projectCfg.Edits.PostWriteCrossFileSettleMs), sourceFor("post_write_cross_file_settle_ms", defaultsCfg.Edits.PostWriteCrossFileSettleMs, globalCfg.Edits.PostWriteCrossFileSettleMs, projectCfg.Edits.PostWriteCrossFileSettleMs)},
	})

	addConfigSection(cfgTable, "walk", [][]string{
		{"refuse_home_roots", strconv.FormatBool(projectCfg.Walk.RefuseHomeRoots), sourceFor("refuse_home_roots", defaultsCfg.Walk.RefuseHomeRoots, globalCfg.Walk.RefuseHomeRoots, projectCfg.Walk.RefuseHomeRoots)},
	})

	wsRows := [][]string{
		{"auto_attach", strconv.FormatBool(projectCfg.Workspace.AutoAttach), sourceFor("auto_attach", defaultsCfg.Workspace.AutoAttach, globalCfg.Workspace.AutoAttach, projectCfg.Workspace.AutoAttach)},
		{"auto_attach_persist", strconv.FormatBool(projectCfg.Workspace.AutoAttachPersist), sourceFor("auto_attach_persist", defaultsCfg.Workspace.AutoAttachPersist, globalCfg.Workspace.AutoAttachPersist, projectCfg.Workspace.AutoAttachPersist)},
	}
	// Trusted per-workspace roots granted manually (via the TUI / CLI), recorded
	// in plumb's data dir — never in the project config. Distinct provenance from
	// the config-file fields above, so shown with a "data-dir grant" source.
	if ws != "" {
		granted := config.NewWorkspaceRootsStore().Get(ws)
		wsRows = append(wsRows,
			[]string{"extra_roots (granted)", fmt.Sprintf("%v", granted.ExtraRoots), "data-dir grant"},
			[]string{"read_roots (granted)", fmt.Sprintf("%v", granted.ReadRoots), "data-dir grant"},
		)
	}
	addConfigSection(cfgTable, "workspace", wsRows)

	addConfigSection(cfgTable, "git", [][]string{
		{"allow_writes", strconv.FormatBool(projectCfg.Git.AllowWrites), policySourceFor(policy, "git.allow_writes", sourceFor("allow_writes", defaultsCfg.Git.AllowWrites, globalCfg.Git.AllowWrites, projectCfg.Git.AllowWrites))},
		{"allow_destructive", strconv.FormatBool(projectCfg.Git.AllowDestructive), policySourceFor(policy, "git.allow_destructive", sourceFor("allow_destructive", defaultsCfg.Git.AllowDestructive, globalCfg.Git.AllowDestructive, projectCfg.Git.AllowDestructive))},
		{"allow_push", strconv.FormatBool(projectCfg.Git.AllowPush), policySourceFor(policy, "git.allow_push", sourceFor("allow_push", defaultsCfg.Git.AllowPush, globalCfg.Git.AllowPush, projectCfg.Git.AllowPush))},
		{"protected_branches", fmt.Sprintf("%v", projectCfg.Git.ProtectedBranches), policySourceFor(policy, "git.protected_branches", sourceFor("protected_branches", defaultsCfg.Git.ProtectedBranches, globalCfg.Git.ProtectedBranches, projectCfg.Git.ProtectedBranches))},
	})

	addConfigSection(cfgTable, "lsp_query", [][]string{
		{"timeout", projectCfg.LSPQuery.Timeout.String(), sourceFor("timeout", defaultsCfg.LSPQuery.Timeout, globalCfg.LSPQuery.Timeout, projectCfg.LSPQuery.Timeout)},
	})

	sem, gsem, dsem := projectCfg.Semantics, globalCfg.Semantics, defaultsCfg.Semantics
	addConfigSection(cfgTable, "semantics", [][]string{
		{"enabled", strconv.FormatBool(sem.Enabled), sourceFor("enabled", dsem.Enabled, gsem.Enabled, sem.Enabled)},
		{"provider", sem.Provider, sourceFor("provider", dsem.Provider, gsem.Provider, sem.Provider)},
		{"model", sem.Model, sourceFor("model", dsem.Model, gsem.Model, sem.Model)},
		{"base_url", sem.BaseURL, sourceFor("base_url", dsem.BaseURL, gsem.BaseURL, sem.BaseURL)},
		{"api_key", maskConfigKey(sem.APIKey), sourceFor("api_key", dsem.APIKey, gsem.APIKey, sem.APIKey)},
		{"api_key_env", sem.APIKeyEnv, sourceFor("api_key_env", dsem.APIKeyEnv, gsem.APIKeyEnv, sem.APIKeyEnv)},
		{"rerank_candidates", strconv.Itoa(sem.RerankCandidates), sourceFor("rerank_candidates", dsem.RerankCandidates, gsem.RerankCandidates, sem.RerankCandidates)},
		{"timeout", sem.Timeout.String(), sourceFor("timeout", dsem.Timeout, gsem.Timeout, sem.Timeout)},
	})

	mem, gmem, dmem := projectCfg.Memory, globalCfg.Memory, defaultsCfg.Memory
	addConfigSection(cfgTable, "memory", [][]string{
		{"enabled", strconv.FormatBool(mem.Enabled), sourceFor("enabled", dmem.Enabled, gmem.Enabled, mem.Enabled)},
		{"generated_summaries", strconv.FormatBool(mem.GeneratedSummaries), sourceFor("generated_summaries", dmem.GeneratedSummaries, gmem.GeneratedSummaries, mem.GeneratedSummaries)},
		{"inject_hints", strconv.FormatBool(mem.InjectHints), sourceFor("inject_hints", dmem.InjectHints, gmem.InjectHints, mem.InjectHints)},
		{"hint_budget_bytes", strconv.Itoa(mem.HintBudgetBytes), sourceFor("hint_budget_bytes", dmem.HintBudgetBytes, gmem.HintBudgetBytes, mem.HintBudgetBytes)},
		{"episodic_budget_bytes", strconv.Itoa(mem.EpisodicBudgetBytes), sourceFor("episodic_budget_bytes", dmem.EpisodicBudgetBytes, gmem.EpisodicBudgetBytes, mem.EpisodicBudgetBytes)},
		{"max_hints", strconv.Itoa(mem.MaxHints), sourceFor("max_hints", dmem.MaxHints, gmem.MaxHints, mem.MaxHints)},
		{"idle_summary_minutes", strconv.Itoa(mem.IdleSummaryMinutes), sourceFor("idle_summary_minutes", dmem.IdleSummaryMinutes, gmem.IdleSummaryMinutes, mem.IdleSummaryMinutes)},
		{"generated_memory_keep", strconv.Itoa(mem.GeneratedMemoryKeep), sourceFor("generated_memory_keep", dmem.GeneratedMemoryKeep, gmem.GeneratedMemoryKeep, mem.GeneratedMemoryKeep)},
	})

	col, gcol, dcol := projectCfg.Collab, globalCfg.Collab, defaultsCfg.Collab
	addConfigSection(cfgTable, "collab", [][]string{
		{"peer_awareness", strconv.FormatBool(col.PeerAwareness), sourceFor("peer_awareness", dcol.PeerAwareness, gcol.PeerAwareness, col.PeerAwareness)},
		{"hint_budget_bytes", strconv.Itoa(col.HintBudgetBytes), sourceFor("hint_budget_bytes", dcol.HintBudgetBytes, gcol.HintBudgetBytes, col.HintBudgetBytes)},
		{"intents", strconv.FormatBool(col.Intents), sourceFor("intents", dcol.Intents, gcol.Intents, col.Intents)},
		{"mailbox", strconv.FormatBool(col.Mailbox), sourceFor("mailbox", dcol.Mailbox, gcol.Mailbox, col.Mailbox)},
		{"cross_project", strconv.FormatBool(col.CrossProject), sourceFor("cross_project", dcol.CrossProject, gcol.CrossProject, col.CrossProject)},
		{"chat_budget_bytes", strconv.Itoa(col.ChatBudgetBytes), sourceFor("chat_budget_bytes", dcol.ChatBudgetBytes, gcol.ChatBudgetBytes, col.ChatBudgetBytes)},
		{"max_wait_seconds", strconv.Itoa(col.MaxWaitSeconds), sourceFor("max_wait_seconds", dcol.MaxWaitSeconds, gcol.MaxWaitSeconds, col.MaxWaitSeconds)},
		{"knowledge_handoff", strconv.FormatBool(col.KnowledgeHandoff), sourceFor("knowledge_handoff", dcol.KnowledgeHandoff, gcol.KnowledgeHandoff, col.KnowledgeHandoff)},
		{"intent_ttl_minutes", strconv.Itoa(col.IntentTTLMinutes), sourceFor("intent_ttl_minutes", dcol.IntentTTLMinutes, gcol.IntentTTLMinutes, col.IntentTTLMinutes)},
	})

	for _, lang := range sortedLSPKeys(projectCfg.LSP) {
		cfg := projectCfg.LSP[lang]
		globCfg := globalCfg.LSP[lang]
		defCfg := defaultsCfg.LSP[lang]

		prefix := "lsp." + lang + "."
		addConfigSection(cfgTable, "lsp."+lang, [][]string{
			{"enabled", strconv.FormatBool(cfg.Enabled), sourceFor("enabled", defCfg.Enabled, globCfg.Enabled, cfg.Enabled)},
			{"active", lspActiveStatus(cfg), "derived"},
			{"command", cfg.Command, policySourceFor(policy, prefix+"command", sourceFor("command", defCfg.Command, globCfg.Command, cfg.Command))},
			{"args", fmt.Sprintf("%v", cfg.Args), policySourceFor(policy, prefix+"args", sourceFor("args", defCfg.Args, globCfg.Args, cfg.Args))},
			{"root_markers", fmt.Sprintf("%v", cfg.RootMarkers), policySourceFor(policy, prefix+"root_markers", sourceFor("root_markers", defCfg.RootMarkers, globCfg.RootMarkers, cfg.RootMarkers))},
			{"weak_root_markers", fmt.Sprintf("%v", cfg.WeakRootMarkers), policySourceFor(policy, prefix+"weak_root_markers", sourceFor("weak_root_markers", defCfg.WeakRootMarkers, globCfg.WeakRootMarkers, cfg.WeakRootMarkers))},
			{"env", fmt.Sprintf("%v", cfg.Env), policySourceFor(policy, prefix+"env", sourceFor("env", defCfg.Env, globCfg.Env, cfg.Env))},
		})
	}

	fmt.Println(renderConfigShowTable(cfgTable))
	printProjectPolicyNotice(ws, policy)

	// 4. Reload behaviour — which groups the running daemon applies live versus
	// those that need a restart. Mirrors config.RestartSensitiveEqual; the daemon
	// reports a concrete restart-pending state via the daemon_info tool.
	fmt.Printf("\nReload behaviour\n")
	reloadTable := tableBase().Headers("Config group", "Applies")
	reloadTable.Row("edits, git, walk", configShowOkStyle().Render("live"))
	reloadTable.Row("log_level", configShowOkStyle().Render("live (set-level)"))
	reloadTable.Row("ui.theme", configShowOkStyle().Render("live (TUI)"))
	reloadTable.Row("topology", configShowOkStyle().Render("live (reconciled)"))
	reloadTable.Row("workspace, quality, lsp_query", configShowOkStyle().Render("live on next attach/session"))
	reloadTable.Row("lsp.* servers, cache, log_format", configShowWarnStyle().Render("needs daemon restart"))
	fmt.Println(renderConfigShowTable(reloadTable))
	fmt.Println()

	printAgentProvenance(ws)
	return nil
}

// policySourceFor annotates a provenance label for a key in one of the
// capability-granting sections.
//
// The plain label answers "where did the value in effect come from", and for a
// forced-back key that answer is "global config" — true, and exactly the thing
// that makes an ignored project setting invisible. The suffix restores the other
// half: the project asked for something here, and it is not what you are looking
// at.
func policySourceFor(st config.ProjectPolicyStatus, key, base string) string {
	if st.Asked(key) {
		if st.Trusted {
			return "project config (trusted)"
		}
		return base + " — project asked, UNTRUSTED"
	}
	// This key is capability-granting and the project did not ask for it, so the
	// value cannot have come from the project — whatever sourceFor's value
	// comparison inferred. Saying "project config" here would attribute a value to
	// a file that does not set it, which is the misattribution this annotation
	// exists to prevent, pointing the other way. Env keeps its own label: it
	// really is the highest layer.
	if base == "project config" {
		return "global config"
	}
	return base
}

// printProjectPolicyNotice states, in one place and in full, what this project's
// config asked for in the capability-granting sections and whether it is in
// force. It prints the requested VALUES, not just the key names, because that is
// what a user needs in order to decide whether to trust them — and because there
// is otherwise nowhere at all to see them: the resolved table above shows the
// value in effect, which for an untrusted request is precisely not what the
// project wrote.
func printProjectPolicyNotice(ws string, st config.ProjectPolicyStatus) {
	if st.Spec.IsEmpty() {
		return
	}
	if st.Trusted {
		fmt.Println(configShowOkStyle().Render(
			fmt.Sprintf("✓ this project's capability-granting config is trusted — %d key(s) in effect:", len(st.Spec))))
		for _, line := range st.Spec.Describe() {
			fmt.Println(configShowMutedStyle().Render("    " + line))
		}
		fmt.Println()
		return
	}
	fmt.Println(configShowWarnStyle().Render(
		fmt.Sprintf("! this project's .plumb/config.toml sets %d capability-granting key(s) that are NOT in effect —", len(st.Spec)) +
			"\n  plumb ignores [git] and the exec-deciding [lsp.<lang>] fields from an untrusted project config" +
			"\n  (a cloned repository ships one, and it would otherwise run its own argv on attach):"))
	for _, line := range st.Spec.Describe() {
		fmt.Println(configShowWarnStyle().Render("    " + line))
	}
	fmt.Println(configShowWarnStyle().Render(
		"  → run `plumb trust " + ws + "` to honour them; the values above are what you would be approving"))
	fmt.Println()
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
		dirTable.Row(r.name, existsIcon(r.path), contractConfigPath(r.path), configShowMutedStyle().Render(r.holds))
	}
	fmt.Println(renderConfigShowTable(dirTable))
}

func formatConfigVal(val string) string {
	if val == "" {
		return configShowMutedStyle().Render("(none)")
	}
	return configShowValStyle().Render(val)
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
		provs.WriteString(configShowMutedStyle().Render(item[2]))
	}
	t.Row(configShowKeyStyle().Render(name), keys.String(), vals.String(), provs.String())
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
		BorderStyle(configShowSepStyle()).
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
		return configShowMutedStyle().Render("-")
	}
	if _, err := os.Stat(path); err == nil {
		return configShowOkStyle().Render("✓")
	}
	return configShowWarnStyle().Render("✗")
}

func registeredIcon(path string) string {
	if path == "" {
		return configShowMutedStyle().Render("-")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return configShowWarnStyle().Render("✗")
	}

	// A simple string search is robust enough for checking registration status
	// across the JSON schemas and Codex's TOML schema.
	if strings.Contains(string(data), "plumb") {
		return configShowOkStyle().Render("✓")
	}
	return configShowWarnStyle().Render("✗")
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
		return configShowMutedStyle().Render("(none)")
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
	case "block_dirty_writes":
		return "PLUMB_BLOCK_DIRTY_WRITES"
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
