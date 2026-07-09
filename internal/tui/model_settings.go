package tui

// model_settings.go — the Settings model: row/key types, the reload-tier
// classification, value formatters, and the construction of the editable rows
// (buildSettingItems / lspSettingItems). Rendering lives in
// model_settings_render.go (layout) and model_settings_rows.go (per-row); the
// theme picker in model_theme_picker.go; key handling in model_settings_keys.go.

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// settingKind classifies how a settings row is edited.
type settingKind int

const (
	settingPopup  settingKind = iota // enter opens a sub-popup (theme picker)
	settingCycle                     // ←→ cycles through a fixed option set
	settingToggle                    // enter/space flips on↔off
	settingNumber                    // ←→ adjusts a numeric value
	settingList                      // enter opens the list editor ([]string values)
	settingText                      // enter opens a single-line text editor (string values)
)

// settingKey identifies which config field a settings row edits. The key
// handler switches on this to mutate settingsCfg and persist via config.Save.
type settingKey int

const (
	skTheme settingKey = iota
	skLogLevel
	skLogFormat
	skLogFile
	skStrict
	skShowWriteDiff
	skBlockDirtyWrites
	skRateLimit
	skPostWriteDiagMs
	skPostWriteCrossFile
	skPostWriteCrossFileSettleMs
	skConcurrentSkewMs
	skRefuseHomeRoots
	skTopology
	skTopoResyncOnAttach
	skTopoWatch
	skTopoMaxFileSize
	skTopoResyncBatch
	skTopoResyncPauseMs
	skTopoResyncIntervalMin
	skQuality
	skQualityMode
	skQualityTimeoutMs
	skQualityMaxFindings
	skGitWrites
	skGitDestructive
	skGitPush
	skCacheTTL
	skCacheMaxSize
	skLSPTimeout
	skAutoAttach
	skAutoAttachPersist
	skAllowDependencyReads
	skChildScanDepth
	skExtraRoots
	skReadRoots
	skProtectedBranches
	skExcludePatterns
	skAnalysers
	skIdleThresholdMin
	skEvictionTTLMin
	skPersistState
	skPersistStateTTLMin
	skMemoryEnabled
	skMemoryGeneratedSummaries
	skMemoryInjectHints
	skMemoryHintBudgetBytes
	skMemoryEpisodicBudgetBytes
	skMemoryMaxHints
	skMemoryIdleSummaryMin
	skMemoryGeneratedKeep
	skPathStyle
	skWebPort
	skAgentConfigWrites
	// Per-language [lsp.<lang>] rows carry the language in settingItem.lspLang.
	skLSPEnabled
	skLSPCommand
	skLSPArgs
	skLSPRootMarkers
	// [semantics] rows (the Semantics tab).
	skSemEnabled
	skSemProvider
	skSemModel
	skSemBaseURL
	skSemAPIKeyEnv
	skSemAPIKey
	skSemRerankCandidates
	skSemTimeout
	// [collab] rows (the General tab, "Collab" group).
	skCollabPeerAwareness
	skCollabHintBudgetBytes
	skCollabIntents
	skCollabMailbox
	skCollabKnowledgeHandoff
	skCollabIntentTTLMin
)

// dottedKeyFor maps a settings row key to its config-field-registry dotted key.
// For a per-language [lsp.<lang>] row it builds the concrete key from lspLang,
// or the "<lang>" template form when lspLang is empty (a tier-only lookup).
func dottedKeyFor(key settingKey, lspLang string) string {
	if field, ok := lspFieldName(key); ok {
		lang := lspLang
		if lang == "" {
			lang = "<lang>"
		}
		return "lsp." + lang + "." + field
	}
	return settingDottedKeys[key]
}

// helpFor returns the registry description for a row, substituting the concrete
// language into a per-language template. Empty when the key is not registered.
func helpFor(key settingKey, lspLang string) string {
	f, ok := config.Lookup(dottedKeyFor(key, lspLang))
	if !ok {
		return ""
	}
	if lspLang != "" {
		return strings.ReplaceAll(f.Description, "<lang>", lspLang)
	}
	return f.Description
}

// stampHelp fills each row's help from the registry, leaving any help already
// set (a dynamic per-language message) untouched — so the description lives in
// one place, the config field registry, never duplicated in this file.
func stampHelp(items []settingItem) []settingItem {
	for i := range items {
		if items[i].help == "" {
			items[i].help = helpFor(items[i].key, items[i].lspLang)
		}
	}
	return items
}

// reloadTierFor reports when a change to a setting takes effect, read from the
// config field registry — the single source of truth shared with the row
// marker, the status line, config show, and the agent-writable-config tool. The
// restart set stays in lock-step with config.RestartSensitiveEqual (log
// format/file + cache).
func reloadTierFor(key settingKey) config.ReloadTier {
	if f, ok := config.Lookup(dottedKeyFor(key, "")); ok {
		return f.ReloadTier
	}
	return config.ReloadLive
}

// settingItem is one selectable row on the Settings screen. Group headers are
// not items — they are derived from the group field during rendering. The
// reload tier is computed from key via reloadTierFor, not stored.
type settingItem struct {
	group      string
	label      string
	kind       settingKind
	key        settingKey
	value      string   // formatted current value
	options    []string // option set for settingCycle
	help       string   // one-line description, shown on the status bar's second line
	overridden bool     // workspace scope: the key is set in the project config (not inherited)
	lspLang    string   // non-empty for per-language [lsp.<lang>] rows; identifies the language
	lspMissing bool     // enabled LSP server whose command is not on PATH
	list       []string // raw entries for settingList rows; rendered one per line
	tab        int      // which rows-pane tab owns this row (settingsTabGeneral/LSP/Semantics)
}

var (
	logLevelOptions    = []string{"debug", "info", "warn", "error"}
	logFormatOptions   = []string{"text", "json"}
	cacheTTLOptions    = []string{"1m", "5m", "10m", "30m", "1h"}
	lspTimeoutOptions  = []string{"0s", "10s", "30s", "1m", "2m"}
	pathStyleOptions   = []string{"compact", "truncate-middle", "full"}
	qualityModeOptions = []string{"background", "sync"}
)

// durValue formats a duration as its matching preset string when one exists,
// so the value column and the cycle option set line up.
func durValue(d config.Duration, presets []string) string {
	for _, p := range presets {
		if pd, err := time.ParseDuration(p); err == nil && pd == d.Duration {
			return p
		}
	}
	return d.String()
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func rateLimitValue(n int) string {
	if n <= 0 {
		return "off"
	}
	return fmt.Sprintf("%d", n)
}

func pathStyleValue(s string) string {
	if s == "" {
		return "compact"
	}
	return s
}

// buildSettingItems returns the full set of editable settings rows in display
// order, one per config field. Theme reflects the live ActiveThemeName;
// everything else comes from the supplied config snapshot. Each row carries a
// one-line help string shown on the status bar's second line when focused.
func buildSettingItems(cfg config.Config) []settingItem {
	itoa := func(n int) string { return fmt.Sprintf("%d", n) }
	return stampHelp(append(append([]settingItem{
		{group: "Appearance", label: "Theme", kind: settingPopup, key: skTheme, value: ActiveThemeName},
		{group: "Appearance", label: "Path style", kind: settingCycle, key: skPathStyle, value: pathStyleValue(cfg.UI.PathStyle), options: pathStyleOptions},

		{group: "Web", label: "Web UI port", kind: settingNumber, key: skWebPort, value: itoa(cfg.Web.Port)},

		{group: "Logging", label: "Log level", kind: settingCycle, key: skLogLevel, value: cfg.LogLevel, options: logLevelOptions},
		{group: "Logging", label: "Log format", kind: settingCycle, key: skLogFormat, value: cfg.LogFormat, options: logFormatOptions},
		{group: "Logging", label: "Log file", kind: settingPopup, key: skLogFile, value: pathOrDefault(cfg.LogFile)},

		{group: "Editing", label: "Strict edits", kind: settingToggle, key: skStrict, value: onOff(cfg.Edits.Strict)},
		{group: "Editing", label: "Show write diff", kind: settingToggle, key: skShowWriteDiff, value: onOff(cfg.Edits.ShowWriteDiff)},
		{group: "Editing", label: "Block dirty writes", kind: settingToggle, key: skBlockDirtyWrites, value: onOff(cfg.Edits.BlockDirtyWrites)},
		{group: "Editing", label: "Rate limit / min", kind: settingNumber, key: skRateLimit, value: rateLimitValue(cfg.Edits.RateLimitPerMinute)},
		{group: "Editing", label: "Post-write diag (ms)", kind: settingNumber, key: skPostWriteDiagMs, value: itoa(cfg.Edits.PostWriteDiagnosticsMs)},
		{group: "Editing", label: "Cross-file diag", kind: settingToggle, key: skPostWriteCrossFile, value: onOff(cfg.Edits.PostWriteCrossFile)},
		{group: "Editing", label: "Cross-file settle (ms)", kind: settingNumber, key: skPostWriteCrossFileSettleMs, value: itoa(cfg.Edits.PostWriteCrossFileSettleMs)},
		{group: "Editing", label: "Concurrent skew (ms)", kind: settingNumber, key: skConcurrentSkewMs, value: itoa(cfg.Edits.ConcurrentWriteSkewMs)},
		{group: "Editing", label: "Refuse home roots", kind: settingToggle, key: skRefuseHomeRoots, value: onOff(cfg.Walk.RefuseHomeRoots)},

		{group: "Indexing", label: "Topology", kind: settingToggle, key: skTopology, value: onOff(cfg.Topology.Enabled)},
		{group: "Indexing", label: "Resync on attach", kind: settingToggle, key: skTopoResyncOnAttach, value: onOff(cfg.Topology.ResyncOnAttach)},
		{group: "Indexing", label: "Watch files", kind: settingToggle, key: skTopoWatch, value: onOff(cfg.Topology.Watch)},
		{group: "Indexing", label: "Max file size (B)", kind: settingNumber, key: skTopoMaxFileSize, value: itoa(int(cfg.Topology.MaxFileSizeBytes))},
		{group: "Indexing", label: "Resync batch", kind: settingNumber, key: skTopoResyncBatch, value: itoa(cfg.Topology.ResyncBatch)},
		{group: "Indexing", label: "Resync pause (ms)", kind: settingNumber, key: skTopoResyncPauseMs, value: itoa(cfg.Topology.ResyncPauseMs)},
		{group: "Indexing", label: "Resync interval (min)", kind: settingNumber, key: skTopoResyncIntervalMin, value: itoa(cfg.Topology.ResyncIntervalMinutes)},
		{group: "Indexing", label: "Exclude patterns", kind: settingList, key: skExcludePatterns, value: listSummary(cfg.Topology.ExcludePatterns), list: cfg.Topology.ExcludePatterns},

		{group: "Quality", label: "Quality analysis", kind: settingToggle, key: skQuality, value: onOff(cfg.Quality.Enabled)},
		{group: "Quality", label: "Mode", kind: settingCycle, key: skQualityMode, value: qualityModeValue(cfg.Quality.Mode), options: qualityModeOptions},
		{group: "Quality", label: "Timeout (ms)", kind: settingNumber, key: skQualityTimeoutMs, value: itoa(cfg.Quality.TimeoutMs)},
		{group: "Quality", label: "Max findings/file", kind: settingNumber, key: skQualityMaxFindings, value: itoa(cfg.Quality.MaxFindingsPerFile)},
		{group: "Quality", label: "Analysers", kind: settingList, key: skAnalysers, value: listSummary(cfg.Quality.Analysers), list: cfg.Quality.Analysers},

		{group: "Git", label: "Git allow writes", kind: settingToggle, key: skGitWrites, value: onOff(cfg.Git.AllowWrites)},
		{group: "Git", label: "Git allow destructive", kind: settingToggle, key: skGitDestructive, value: onOff(cfg.Git.AllowDestructive)},
		{group: "Git", label: "Git allow push", kind: settingToggle, key: skGitPush, value: onOff(cfg.Git.AllowPush)},
		{group: "Git", label: "Protected branches", kind: settingList, key: skProtectedBranches, value: listSummary(cfg.Git.ProtectedBranches), list: cfg.Git.ProtectedBranches},

		{group: "Session", label: "Idle threshold (min)", kind: settingNumber, key: skIdleThresholdMin, value: itoa(cfg.Session.IdleThresholdMinutes)},
		{group: "Session", label: "Eviction TTL (min)", kind: settingNumber, key: skEvictionTTLMin, value: itoa(cfg.Session.EvictionTTLMinutes)},
		{group: "Session", label: "Persist state", kind: settingToggle, key: skPersistState, value: onOff(cfg.Session.PersistState)},
		{group: "Session", label: "Persist state TTL (min)", kind: settingNumber, key: skPersistStateTTLMin, value: itoa(cfg.Session.PersistStateTTLMinutes)},

		{group: "Memory", label: "Memory index", kind: settingToggle, key: skMemoryEnabled, value: onOff(cfg.Memory.Enabled)},
		{group: "Memory", label: "Generated summaries", kind: settingToggle, key: skMemoryGeneratedSummaries, value: onOff(cfg.Memory.GeneratedSummaries)},
		{group: "Memory", label: "Inject hints", kind: settingToggle, key: skMemoryInjectHints, value: onOff(cfg.Memory.InjectHints)},
		{group: "Memory", label: "Hint budget (B)", kind: settingNumber, key: skMemoryHintBudgetBytes, value: itoa(cfg.Memory.HintBudgetBytes)},
		{group: "Memory", label: "Episodic budget (B)", kind: settingNumber, key: skMemoryEpisodicBudgetBytes, value: itoa(cfg.Memory.EpisodicBudgetBytes)},
		{group: "Memory", label: "Max hints", kind: settingNumber, key: skMemoryMaxHints, value: itoa(cfg.Memory.MaxHints)},
		{group: "Memory", label: "Idle summary (min)", kind: settingNumber, key: skMemoryIdleSummaryMin, value: itoa(cfg.Memory.IdleSummaryMinutes)},
		{group: "Memory", label: "Generated keep", kind: settingNumber, key: skMemoryGeneratedKeep, value: itoa(cfg.Memory.GeneratedMemoryKeep)},

		{group: "Collab", label: "Peer awareness", kind: settingToggle, key: skCollabPeerAwareness, value: onOff(cfg.Collab.PeerAwareness)},
		{group: "Collab", label: "Hint budget (B)", kind: settingNumber, key: skCollabHintBudgetBytes, value: itoa(cfg.Collab.HintBudgetBytes)},
		{group: "Collab", label: "Intents", kind: settingToggle, key: skCollabIntents, value: onOff(cfg.Collab.Intents)},
		{group: "Collab", label: "Mailbox", kind: settingToggle, key: skCollabMailbox, value: onOff(cfg.Collab.Mailbox)},
		{group: "Collab", label: "Knowledge handoff", kind: settingToggle, key: skCollabKnowledgeHandoff, value: onOff(cfg.Collab.KnowledgeHandoff)},
		{group: "Collab", label: "Intent TTL (min)", kind: settingNumber, key: skCollabIntentTTLMin, value: itoa(cfg.Collab.IntentTTLMinutes)},

		{group: "Workspace", label: "Auto attach", kind: settingToggle, key: skAutoAttach, value: onOff(cfg.Workspace.AutoAttach)},
		{group: "Workspace", label: "Auto attach persist", kind: settingToggle, key: skAutoAttachPersist, value: onOff(cfg.Workspace.AutoAttachPersist)},
		{group: "Workspace", label: "Allow dependency reads", kind: settingToggle, key: skAllowDependencyReads, value: onOff(cfg.Workspace.AllowDependencyReads)},
		{group: "Workspace", label: "Child scan depth", kind: settingNumber, key: skChildScanDepth, value: itoa(cfg.Workspace.ChildScanDepth)},
		{group: "Workspace", label: "Extra roots", kind: settingList, key: skExtraRoots, value: listSummary(cfg.Workspace.ExtraRoots), list: cfg.Workspace.ExtraRoots},
		{group: "Workspace", label: "Read roots", kind: settingList, key: skReadRoots, value: listSummary(cfg.Workspace.ReadRoots), list: cfg.Workspace.ReadRoots},

		{group: "Others", label: "Cache TTL", kind: settingCycle, key: skCacheTTL, value: durValue(cfg.Cache.TTL, cacheTTLOptions), options: cacheTTLOptions},
		{group: "Others", label: "Cache max size", kind: settingNumber, key: skCacheMaxSize, value: itoa(cfg.Cache.MaxSize)},
		{group: "Others", label: "LSP query timeout", kind: settingCycle, key: skLSPTimeout, value: durValue(cfg.LSPQuery.Timeout, lspTimeoutOptions), options: lspTimeoutOptions},
		{group: "Others", label: "Agent config writes", kind: settingToggle, key: skAgentConfigWrites, value: onOff(cfg.AgentConfigWrites)},
	}, lspSettingItems(cfg)...), semanticsSettingItems(cfg)...))
}

// lspSettingItems builds the per-language [lsp.<lang>] rows (enable + command +
// args + root_markers), one block per configured language in sorted order. Each
// row carries its language in lspLang; the field is identified by the key.
func lspSettingItems(cfg config.Config) []settingItem {
	langs := make([]string, 0, len(cfg.LSP))
	for l := range cfg.LSP {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	out := make([]settingItem, 0, len(langs)*4)
	for _, lang := range langs {
		e := cfg.LSP[lang]
		g := capFirst(lang)
		// Languages are enabled by default and activate automatically when their
		// server is installed. An enabled language whose binary is absent is
		// "dormant" — normal, not an error; we flag it only so the user knows why
		// it is inactive.
		dormant := e.Enabled && e.Command != "" && !lspOnPath(e.Command)
		// Non-dormant help comes from the registry (stampHelp); only the dynamic
		// dormant message is set here, so it survives the stamp.
		enabledHelp := ""
		enabledValue := onOff(e.Enabled)
		if dormant {
			enabledValue = "on (dormant)"
			enabledHelp = lang + " is enabled but its server is not installed, so it is dormant. Install " + e.Command + " to activate it, or set off to exclude it."
		}
		out = append(out,
			settingItem{
				group: g, label: "enabled", kind: settingToggle, key: skLSPEnabled, lspLang: lang, lspMissing: dormant,
				value: enabledValue, help: enabledHelp,
			},
			settingItem{
				group: g, label: "command", kind: settingText, key: skLSPCommand, lspLang: lang, lspMissing: dormant,
				value: pathOrDefault(e.Command),
			},
			settingItem{
				group: g, label: "args", kind: settingList, key: skLSPArgs, lspLang: lang, lspMissing: dormant,
				value: listSummary(e.Args), list: e.Args,
			},
			settingItem{
				group: g, label: "root markers", kind: settingList, key: skLSPRootMarkers, lspLang: lang, lspMissing: dormant,
				value: listSummary(e.RootMarkers), list: e.RootMarkers,
			},
		)
	}
	for i := range out {
		out[i].tab = settingsTabLSP
	}
	return out
}

// semanticsSettingItems builds the [semantics] rows (the Semantics tab): opt-in
// API-backed semantic re-rank for topology_search. The api key row is masked.
func semanticsSettingItems(cfg config.Config) []settingItem {
	sem := cfg.Semantics
	itoa := func(n int) string { return fmt.Sprintf("%d", n) }
	out := []settingItem{
		{group: "Semantics", label: "enabled", kind: settingToggle, key: skSemEnabled, value: onOff(sem.Enabled)},
		{group: "Semantics", label: "provider", kind: settingCycle, key: skSemProvider, value: semProviderValue(sem.Provider), options: config.SemanticsProviders},
		{group: "Semantics", label: "model", kind: settingText, key: skSemModel, value: pathOrDefault(sem.Model)},
		{group: "Semantics", label: "base url", kind: settingText, key: skSemBaseURL, value: pathOrDefault(sem.BaseURL)},
		{group: "Semantics", label: "api key env", kind: settingText, key: skSemAPIKeyEnv, value: semKeyEnvValue(sem)},
		{group: "Semantics", label: "api key", kind: settingText, key: skSemAPIKey, value: maskedKey(sem.APIKey)},
		{group: "Semantics", label: "rerank candidates", kind: settingNumber, key: skSemRerankCandidates, value: itoa(sem.RerankCandidates)},
		{group: "Semantics", label: "timeout", kind: settingCycle, key: skSemTimeout, value: durValue(sem.Timeout, lspTimeoutOptions), options: lspTimeoutOptions},
	}
	out = stampHelp(out)
	for i := range out {
		out[i].tab = settingsTabSemantics
	}
	return out
}

func semProviderValue(p string) string {
	if p == "" {
		return "openai"
	}
	return p
}

// maskedKey renders the api_key row: never the value, only whether one is set.
func maskedKey(k string) string {
	if k == "" {
		return "(unset — using env)"
	}
	return "•••• (set in config)"
}

// semKeyEnvValue shows the resolved key env var name and whether it is set.
func semKeyEnvValue(s config.SemanticsConfig) string {
	env := s.KeySourceEnv()
	if env == "" {
		return "(none)"
	}
	if os.Getenv(env) != "" {
		return env + " ✓"
	}
	return env + " ✗"
}

// lspOnPath reports whether cmd resolves to an executable on PATH (or via an
// absolute/relative path). Used to flag enabled-but-missing LSP servers.
func lspOnPath(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// pathOrDefault renders a path field, showing "(default)" when empty.
func pathOrDefault(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}

// qualityModeValue defaults an empty mode to "background".
func qualityModeValue(s string) string {
	if s == "" {
		return "background"
	}
	return s
}

// listSummary renders a []string setting's value column: the count and the
// joined entries (truncated by the column width), or "(none)" when empty.
func listSummary(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return fmt.Sprintf("(%d) %s", len(items), strings.Join(items, ", "))
}

// Settings rows-pane tabs. General holds every non-LSP setting; Commands manages
// the command allow-list + [commands] policy (its own two-pane view, not the flat
// settingItem rows); LSP holds the per-language [lsp.<lang>] server rows. tab /
// shift+tab cycle through the Scope column and these tabs (Scope → General →
// Commands → LSP → Semantics → Scope).
const (
	settingsTabGeneral = iota
	settingsTabCommands
	settingsTabLSP
	settingsTabSemantics
)

// settingsTabHeaderRows is the height reserved at the top of the rows pane for
// the tab bar plus the blank line beneath it.
const settingsTabHeaderRows = 2

var settingsTabNames = []string{"General", "Commands", "LSP", "Semantics"}

// filterSettingsByTab keeps only the rows whose tab matches the active one.
func filterSettingsByTab(items []settingItem, tab int) []settingItem {
	out := make([]settingItem, 0, len(items))
	for _, it := range items {
		if it.tab == tab {
			out = append(out, it)
		}
	}
	return out
}

// capFirst upper-cases the first rune of s, used for per-language LSP section
// headers ("go" → "Go"). Language ids are ASCII, so byte slicing is safe.
func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// settingsFooterRows is the number of body rows reserved at the bottom for the
// pinned footer: a blank separator, the key-hint bar, and the status bar.
const settingsFooterRows = 3
