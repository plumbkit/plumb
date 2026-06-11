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
	skRateLimit
	skPostWriteDiagMs
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
	skExtraRoots
	skReadRoots
	skProtectedBranches
	skExcludePatterns
	skAnalysers
	skIdleThresholdMin
	skEvictionTTLMin
	skPathStyle
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
)

// reloadTier classifies when a change to a setting takes effect. It mirrors
// config.RestartSensitiveEqual — the daemon's single source of truth — under
// which only log format and cache need a restart; edits/git/topology hot-reload
// into running sessions (the TUI pushes reload-config on every change), and
// quality/auto_attach/lsp_query apply on the next workspace attach.
type reloadTier int

const (
	reloadLive        reloadTier = iota // applies to running sessions immediately
	reloadNextSession                   // applies on next attach / new session
	reloadRestart                       // needs a daemon restart
)

// reloadTierFor maps a settings row to when its change takes effect. Single
// source of truth shared by the row marker and the status line, so the two can
// never disagree. The reloadRestart set must stay in lock-step with the fields
// config.RestartSensitiveEqual compares (log format + cache).
func reloadTierFor(key settingKey) reloadTier {
	switch key {
	case skLogFormat, skLogFile, skCacheTTL, skCacheMaxSize:
		return reloadRestart
	case skQuality, skQualityMode, skQualityTimeoutMs, skQualityMaxFindings, skAnalysers,
		skAutoAttach, skAutoAttachPersist, skAllowDependencyReads, skExtraRoots, skReadRoots, skLSPTimeout,
		skTopoResyncOnAttach, skTopoWatch, skTopoMaxFileSize,
		skTopoResyncBatch, skTopoResyncPauseMs, skTopoResyncIntervalMin, skExcludePatterns,
		skLSPEnabled, skLSPCommand, skLSPArgs, skLSPRootMarkers:
		return reloadNextSession
	default: // theme, path style, log level, edits, walk, topology enable, git, session
		return reloadLive
	}
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
	return append(append([]settingItem{
		{
			group: "Appearance", label: "Theme", kind: settingPopup, key: skTheme, value: ActiveThemeName,
			help: "Colour theme for the TUI and `plumb config show` syntax highlighting.",
		},
		{
			group: "Appearance", label: "Path style", kind: settingCycle, key: skPathStyle, value: pathStyleValue(cfg.UI.PathStyle), options: pathStyleOptions,
			help: "How workspace folder paths are abbreviated in the Sessions sidebar.",
		},

		{
			group: "Logging", label: "Log level", kind: settingCycle, key: skLogLevel, value: cfg.LogLevel, options: logLevelOptions,
			help: "Daemon log verbosity. Applies live via the control socket.",
		},
		{
			group: "Logging", label: "Log format", kind: settingCycle, key: skLogFormat, value: cfg.LogFormat, options: logFormatOptions,
			help: "Daemon log encoding: human-readable text or structured JSON.",
		},
		{
			group: "Logging", label: "Log file", kind: settingPopup, key: skLogFile, value: pathOrDefault(cfg.LogFile),
			help: "Path the daemon writes logs to. Enter to edit; blank uses the default cache dir.",
		},

		{
			group: "Editing", label: "Strict edits", kind: settingToggle, key: skStrict, value: onOff(cfg.Edits.Strict),
			help: "Require a prior read_file (matching mtime) before edit_file.",
		},
		{
			group: "Editing", label: "Show write diff", kind: settingToggle, key: skShowWriteDiff, value: onOff(cfg.Edits.ShowWriteDiff),
			help: "Append a unified diff to edit_file/write_file responses.",
		},
		{
			group: "Editing", label: "Rate limit / min", kind: settingNumber, key: skRateLimit, value: rateLimitValue(cfg.Edits.RateLimitPerMinute),
			help: "Max write ops per session per minute. 0 disables limiting.",
		},
		{
			group: "Editing", label: "Post-write diag (ms)", kind: settingNumber, key: skPostWriteDiagMs, value: itoa(cfg.Edits.PostWriteDiagnosticsMs),
			help: "How long write tools wait for the LSP to re-publish diagnostics. 0 disables.",
		},
		{
			group: "Editing", label: "Concurrent skew (ms)", kind: settingNumber, key: skConcurrentSkewMs, value: itoa(cfg.Edits.ConcurrentWriteSkewMs),
			help: "Clock-skew allowance for edit_file's concurrent-write detector. Raise on network mounts.",
		},
		{
			group: "Editing", label: "Refuse home roots", kind: settingToggle, key: skRefuseHomeRoots, value: onOff(cfg.Walk.RefuseHomeRoots),
			help: "Refuse walks rooted at $HOME or a protected dir (macOS TCC prompt guard).",
		},

		{
			group: "Indexing", label: "Topology", kind: settingToggle, key: skTopology, value: onOff(cfg.Topology.Enabled),
			help: "Enable the SQLite/FTS5 semantic index at <ws>/.plumb/topology.db.",
		},
		{
			group: "Indexing", label: "Resync on attach", kind: settingToggle, key: skTopoResyncOnAttach, value: onOff(cfg.Topology.ResyncOnAttach),
			help: "Trigger a full topology resync each time the workspace attaches.",
		},
		{
			group: "Indexing", label: "Watch files", kind: settingToggle, key: skTopoWatch, value: onOff(cfg.Topology.Watch),
			help: "OS-level file watching: re-index a file the moment it changes on disk.",
		},
		{
			group: "Indexing", label: "Max file size (B)", kind: settingNumber, key: skTopoMaxFileSize, value: itoa(int(cfg.Topology.MaxFileSizeBytes)),
			help: "Cap on file size considered for extraction (bytes). 0 uses the 512 KiB default.",
		},
		{
			group: "Indexing", label: "Resync batch", kind: settingNumber, key: skTopoResyncBatch, value: itoa(cfg.Topology.ResyncBatch),
			help: "Files a full resync extracts before pausing. 0 disables pacing.",
		},
		{
			group: "Indexing", label: "Resync pause (ms)", kind: settingNumber, key: skTopoResyncPauseMs, value: itoa(cfg.Topology.ResyncPauseMs),
			help: "Pause inserted after each resync batch (ms). 0 disables pacing.",
		},
		{
			group: "Indexing", label: "Resync interval (min)", kind: settingNumber, key: skTopoResyncIntervalMin, value: itoa(cfg.Topology.ResyncIntervalMinutes),
			help: "Periodic full-resync fallback interval. Suppressed while watching; 0 disables.",
		},
		{
			group: "Indexing", label: "Exclude patterns", kind: settingList, key: skExcludePatterns, value: listSummary(cfg.Topology.ExcludePatterns), list: cfg.Topology.ExcludePatterns,
			help: "Path globs to skip during indexing. Enter to edit the list.",
		},

		{
			group: "Quality", label: "Quality analysis", kind: settingToggle, key: skQuality, value: onOff(cfg.Quality.Enabled),
			help: "Run offline post-write analysers (golangci-lint, …) on changed files.",
		},
		{
			group: "Quality", label: "Mode", kind: settingCycle, key: skQualityMode, value: qualityModeValue(cfg.Quality.Mode), options: qualityModeOptions,
			help: "background: findings on next request. sync: block and append inline.",
		},
		{
			group: "Quality", label: "Timeout (ms)", kind: settingNumber, key: skQualityTimeoutMs, value: itoa(cfg.Quality.TimeoutMs),
			help: "Per-analyser run timeout in milliseconds.",
		},
		{
			group: "Quality", label: "Max findings/file", kind: settingNumber, key: skQualityMaxFindings, value: itoa(cfg.Quality.MaxFindingsPerFile),
			help: "Cap on findings appended per file to keep responses bounded.",
		},
		{
			group: "Quality", label: "Analysers", kind: settingList, key: skAnalysers, value: listSummary(cfg.Quality.Analysers), list: cfg.Quality.Analysers,
			help: "Which analysers to run (e.g. golangci-lint). Enter to edit the list.",
		},

		{
			group: "Git", label: "Git allow writes", kind: settingToggle, key: skGitWrites, value: onOff(cfg.Git.AllowWrites),
			help: "Gate the safe-write tier (add, commit, switch, branch/tag create, stash).",
		},
		{
			group: "Git", label: "Git allow destructive", kind: settingToggle, key: skGitDestructive, value: onOff(cfg.Git.AllowDestructive),
			help: "Gate reset/clean/checkout/restore/rebase/revert (each call also needs confirm).",
		},
		{
			group: "Git", label: "Git allow push", kind: settingToggle, key: skGitPush, value: onOff(cfg.Git.AllowPush),
			help: "Gate push/fetch/pull (each call also needs confirm). Protected branches stay safe.",
		},
		{
			group: "Git", label: "Protected branches", kind: settingList, key: skProtectedBranches, value: listSummary(cfg.Git.ProtectedBranches), list: cfg.Git.ProtectedBranches,
			help: "Branches that may never be force-pushed, even with allow_push. Enter to edit.",
		},

		{
			group: "Session", label: "Idle threshold (min)", kind: settingNumber, key: skIdleThresholdMin, value: itoa(cfg.Session.IdleThresholdMinutes),
			help: "Minutes with no tool call before a session shows the idle marker (cosmetic).",
		},
		{
			group: "Session", label: "Eviction TTL (min)", kind: settingNumber, key: skEvictionTTLMin, value: itoa(cfg.Session.EvictionTTLMinutes),
			help: "Minutes idle before the daemon force-closes a connection. 0 disables eviction.",
		},

		{
			group: "Workspace", label: "Auto attach", kind: settingToggle, key: skAutoAttach, value: onOff(cfg.Workspace.AutoAttach),
			help: "Fall back to the nearest .git/ or seed dir when no marker is found (LSP unavailable).",
		},
		{
			group: "Workspace", label: "Auto attach persist", kind: settingToggle, key: skAutoAttachPersist, value: onOff(cfg.Workspace.AutoAttachPersist),
			help: "Create .plumb/ at the synthetic root on first auto-attach (implies auto_attach).",
		},
		{
			group: "Workspace", label: "Allow dependency reads", kind: settingToggle, key: skAllowDependencyReads, value: onOff(cfg.Workspace.AllowDependencyReads),
			help: "Let read/search tools reach the Go module cache + GOROOT read-only.",
		},
		{
			group: "Workspace", label: "Extra roots", kind: settingList, key: skExtraRoots, value: listSummary(cfg.Workspace.ExtraRoots), list: cfg.Workspace.ExtraRoots,
			help: "Extra dirs read+write tools may reach beyond the workspace. Enter to edit.",
		},
		{
			group: "Workspace", label: "Read roots", kind: settingList, key: skReadRoots, value: listSummary(cfg.Workspace.ReadRoots), list: cfg.Workspace.ReadRoots,
			help: "Extra read-only dirs (compare another project). Enter to edit the list.",
		},

		{
			group: "Others", label: "Cache TTL", kind: settingCycle, key: skCacheTTL, value: durValue(cfg.Cache.TTL, cacheTTLOptions), options: cacheTTLOptions,
			help: "Session symbol-cache time-to-live. Needs a daemon restart.",
		},
		{
			group: "Others", label: "Cache max size", kind: settingNumber, key: skCacheMaxSize, value: itoa(cfg.Cache.MaxSize),
			help: "Max entries in the session symbol cache. Needs a daemon restart.",
		},
		{
			group: "Others", label: "LSP query timeout", kind: settingCycle, key: skLSPTimeout, value: durValue(cfg.LSPQuery.Timeout, lspTimeoutOptions), options: lspTimeoutOptions,
			help: "Cap on a single LSP tool call when the caller carries no deadline. 0 disables.",
		},
	}, lspSettingItems(cfg)...), semanticsSettingItems(cfg)...)
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
		enabledHelp := "Enabled languages activate automatically once their server is installed. Set off to exclude " + lang + " even when installed."
		if dormant {
			enabledHelp = lang + " is enabled but its server is not installed, so it is dormant. Install " + e.Command + " to activate it, or set off to exclude it."
		}
		enabledValue := onOff(e.Enabled)
		if dormant {
			enabledValue = "on (dormant)"
		}
		out = append(out,
			settingItem{
				group: g, label: "enabled", kind: settingToggle, key: skLSPEnabled, lspLang: lang, lspMissing: dormant,
				value: enabledValue, help: enabledHelp,
			},
			settingItem{
				group: g, label: "command", kind: settingText, key: skLSPCommand, lspLang: lang, lspMissing: dormant,
				value: pathOrDefault(e.Command), help: "Executable for the " + lang + " language server. Enter to edit.",
			},
			settingItem{
				group: g, label: "args", kind: settingList, key: skLSPArgs, lspLang: lang, lspMissing: dormant,
				value: listSummary(e.Args), list: e.Args, help: "Command-line args passed to the " + lang + " server. Enter to edit.",
			},
			settingItem{
				group: g, label: "root markers", kind: settingList, key: skLSPRootMarkers, lspLang: lang, lspMissing: dormant,
				value: listSummary(e.RootMarkers), list: e.RootMarkers, help: "Files that mark a " + lang + " project root. Enter to edit.",
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
		{
			group: "Semantics", label: "enabled", kind: settingToggle, key: skSemEnabled, value: onOff(sem.Enabled),
			help: "Re-rank topology_search results by meaning, via your chosen embedding API. Off by default.",
		},
		{
			group: "Semantics", label: "provider", kind: settingCycle, key: skSemProvider, value: semProviderValue(sem.Provider), options: config.SemanticsProviders,
			help: "openai | voyage (code) | jina | mistral | cohere | custom (your own OpenAI-compatible endpoint).",
		},
		{
			group: "Semantics", label: "model", kind: settingText, key: skSemModel, value: pathOrDefault(sem.Model),
			help: "Embedding model id. Blank uses the provider's default (e.g. voyage-code-3).",
		},
		{
			group: "Semantics", label: "base url", kind: settingText, key: skSemBaseURL, value: pathOrDefault(sem.BaseURL),
			help: "API base URL. Blank uses the provider preset; required for custom (e.g. http://localhost:11434/v1).",
		},
		{
			group: "Semantics", label: "api key env", kind: settingText, key: skSemAPIKeyEnv, value: semKeyEnvValue(sem),
			help: "Name of the env var holding the API key (used when 'api key' is blank). ✓ = the var is set.",
		},
		{
			group: "Semantics", label: "api key", kind: settingText, key: skSemAPIKey, value: maskedKey(sem.APIKey),
			help: "Key stored in config; takes precedence over the env var. Prefer the env var to keep secrets out of files.",
		},
		{
			group: "Semantics", label: "rerank candidates", kind: settingNumber, key: skSemRerankCandidates, value: itoa(sem.RerankCandidates),
			help: "How many FTS5 hits to re-rank semantically. Default 50.",
		},
		{
			group: "Semantics", label: "timeout", kind: settingCycle, key: skSemTimeout, value: durValue(sem.Timeout, lspTimeoutOptions), options: lspTimeoutOptions,
			help: "Cap on a single embedding API call. Default 10s.",
		},
	}
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

// Settings rows-pane tabs. General holds every non-LSP setting; LSP holds the
// per-language [lsp.<lang>] server rows. tab / shift+tab cycle through the Scope
// column and these two tabs (Scope → General → LSP → Scope).
const (
	settingsTabGeneral = iota
	settingsTabLSP
	settingsTabSemantics
)

// settingsTabHeaderRows is the height reserved at the top of the rows pane for
// the tab bar plus the blank line beneath it.
const settingsTabHeaderRows = 2

var settingsTabNames = []string{"General", "LSP", "Semantics"}

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
