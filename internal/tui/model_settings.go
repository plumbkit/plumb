package tui

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/config"
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
	return append([]settingItem{
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
	}, lspSettingItems(cfg)...)
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
		missing := e.Enabled && e.Command != "" && !lspOnPath(e.Command)
		enabledHelp := "Enable the " + lang + " language server (most non-Go/Python servers are opt-in)."
		if missing {
			enabledHelp = "(!) " + lang + " language server not found on PATH — install it or set the command."
		}
		out = append(out,
			settingItem{
				group: g, label: "enabled", kind: settingToggle, key: skLSPEnabled, lspLang: lang, lspMissing: missing,
				value: onOff(e.Enabled), help: enabledHelp,
			},
			settingItem{
				group: g, label: "command", kind: settingText, key: skLSPCommand, lspLang: lang, lspMissing: missing,
				value: pathOrDefault(e.Command), help: "Executable for the " + lang + " language server. Enter to edit.",
			},
			settingItem{
				group: g, label: "args", kind: settingList, key: skLSPArgs, lspLang: lang, lspMissing: missing,
				value: listSummary(e.Args), list: e.Args, help: "Command-line args passed to the " + lang + " server. Enter to edit.",
			},
			settingItem{
				group: g, label: "root markers", kind: settingList, key: skLSPRootMarkers, lspLang: lang, lspMissing: missing,
				value: listSummary(e.RootMarkers), list: e.RootMarkers, help: "Files that mark a " + lang + " project root. Enter to edit.",
			},
		)
	}
	return out
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
)

// settingsTabHeaderRows is the height reserved at the top of the rows pane for
// the tab bar plus the blank line beneath it.
const settingsTabHeaderRows = 2

var settingsTabNames = []string{"General", "LSP"}

// filterSettingsByTab keeps only the rows for the active tab. LSP rows are keyed
// off a non-empty lspLang, so the split survives any change to the group label.
func filterSettingsByTab(items []settingItem, tab int) []settingItem {
	out := make([]settingItem, 0, len(items))
	for _, it := range items {
		if (it.lspLang != "") == (tab == settingsTabLSP) {
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

// settingsTabBar renders the rows-pane tab bar: [General] [LSP], the active tab
// highlighted (brightest when the rows pane itself holds focus).
func (m Model) settingsTabBar() string {
	rowsFocused := !m.settingsScopeFocus
	parts := make([]string, len(settingsTabNames))
	for i, n := range settingsTabNames {
		label := "[" + n + "]"
		switch {
		case i == m.settingsTab && rowsFocused:
			parts[i] = SelectedStyle.Render(label)
		case i == m.settingsTab:
			parts[i] = PanelHeaderStyle.Render(label)
		default:
			parts[i] = FadedStyle.Render(label)
		}
	}
	return " " + strings.Join(parts, "  ")
}

// settingsFooterRows is the number of body rows reserved at the bottom for the
// pinned footer: a blank separator, the key-hint bar, and the status bar.
const settingsFooterRows = 3

// settingsLineKind classifies a logical line in the scrollable settings list.
type settingsLineKind int

const (
	slBlank settingsLineKind = iota
	slHeader
	slRow
)

// settingsLine is a width-independent description of one scrollable line. It is
// shared by the renderer and the mouse-click row resolver.
type settingsLine struct {
	kind  settingsLineKind
	group string // slHeader
	item  int    // slRow: index into settingsItems
	cont  int    // slRow: 0 = the row itself; >0 = the Nth list-entry continuation line
}

// settingsLogicalLines describes the scrollable list: each group as a header
// followed by its rows, with a blank line between groups (no leading blank).
func settingsLogicalLines(items []settingItem) []settingsLine {
	out := []settingsLine{}
	last := ""
	for i, it := range items {
		if it.group != last {
			if last != "" {
				out = append(out, settingsLine{kind: slBlank})
			}
			out = append(out, settingsLine{kind: slHeader, group: it.group})
			last = it.group
		}
		out = append(out, settingsLine{kind: slRow, item: i})
		// A multi-entry list row stacks its remaining entries on continuation lines.
		if it.kind == settingList {
			for j := 1; j < len(it.list); j++ {
				out = append(out, settingsLine{kind: slRow, item: i, cont: j})
			}
		}
	}
	return out
}

// rowLabel is a row's display label: list rows get a trailing "(N)" count so the
// value column can stack one entry per line instead of a long joined string.
func rowLabel(it settingItem) string {
	if it.kind == settingList && len(it.list) > 0 {
		return fmt.Sprintf("%s (%d)", it.label, len(it.list))
	}
	return it.label
}

// rowValues is the value column as one or more lines: a list row yields one line
// per entry ("(none)" when empty), everything else a single line.
func rowValues(it settingItem) []string {
	if it.kind == settingList {
		if len(it.list) == 0 {
			return []string{"(none)"}
		}
		return it.list
	}
	return []string{it.value}
}

// settingsColumnWidths returns the label and value column widths (including a
// trailing gap) so every row aligns regardless of label/value lengths.
func settingsColumnWidths(items []settingItem) (labelW, valueW int) {
	for _, it := range items {
		if w := lipgloss.Width(rowLabel(it)); w > labelW {
			labelW = w
		}
		for _, v := range rowValues(it) {
			if w := lipgloss.Width(v); w > valueW {
				valueW = w
			}
		}
	}
	return labelW + 3, valueW + 4
}

// renderSettingsSection renders the full-width Settings section (section 4): a
// grouped, scrollable settings list with a pinned footer bar. Overlays (help,
// section menu, theme picker) are composited on top.
func (m Model) renderSettingsSection() string {
	isOverlay := m.showHelp || m.sectionMenuOpen || m.showThemePicker || m.settingsListEditor != nil || m.settingsTextEditor != nil
	bodyHeight := max(m.height-6, 1)
	innerW := m.width - 2
	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	var sb strings.Builder
	logoLines := strings.Split(LogoText, "\n")
	logoW := lipgloss.Width(logoLines[0])
	menu := m.renderTopMenu(m.width-logoW, isOverlay)
	for i := range 3 {
		sb.WriteString(menu[i] + sepStyle.Render(logoLines[i]) + "\n")
	}
	// Connect the scope/rows divider to the top border with a ┬ junction (top
	// only — the footer status bars span the full width below the divider).
	topFill := []rune(strings.Repeat("─", innerW))
	if sw := m.settingsScopeWidth(); sw < len(topFill) {
		topFill[sw] = '┬'
	}
	sb.WriteString(sepStyle.Render(overlayLogoBottom("╭"+string(topFill)+"╮", m.width)) + "\n")

	sb.WriteString(m.renderSettingsBody(innerW, bodyHeight, isOverlay))

	sb.WriteString(sepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯") + "\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	final := m.applyOverlays(sb.String())
	if m.settingsListEditor != nil {
		final = m.settingsListEditor.renderModal(final, m.width, m.height)
	}
	if m.settingsTextEditor != nil {
		final = m.settingsTextEditor.renderModal(final, m.width, m.height)
	}
	return final
}

// settingsScopeWidth is the width of the left Scope column: the default (longest
// scope label + 4) shifted by the user's [ / ] adjustment, clamped to bounds.
func (m Model) settingsScopeWidth() int {
	base, lo, hi := m.settingsScopeBounds()
	return clampWidth(base+m.settingsScopeWDelta, lo, hi)
}

// settingsScopeBounds returns the default scope-column width and the min/max it
// can be resized to. The default is the widest scope label plus 4 columns of
// breathing room, but capped at 30% of the screen so long workspace names do not
// crowd out the settings pane; [ / ] can still widen it up to hi.
func (m Model) settingsScopeBounds() (base, lo, hi int) {
	lo = 10
	hi = max(m.width-20, lo)
	base = min(scopeLabelWidth(m.settingsScopes)+5, max(m.width*30/100, lo))
	return base, lo, hi
}

// adjustScopeWidth widens (dir>0) or narrows (dir<0) the Scope column by storing
// the delta from the default, clamped so the resulting width stays in bounds.
func (m Model) adjustScopeWidth(dir int) Model {
	base, lo, hi := m.settingsScopeBounds()
	w := clampWidth(base+m.settingsScopeWDelta+dir*2, lo, hi)
	m.settingsScopeWDelta = w - base
	return m
}

// scopeLabelWidth returns the display width of the widest scope label (including
// the "Scope" title), used to size the column to its content.
func scopeLabelWidth(scopes []settingScope) int {
	maxW := lipgloss.Width("Scope")
	for _, sc := range scopes {
		if n := lipgloss.Width(sc.label); n > maxW {
			maxW = n
		}
	}
	return maxW
}

// renderSettingsBody renders the two-pane Settings layout: the Scope column
// (Global + workspaces) on the left, the settings rows for the selected scope on
// the right, and the pinned footer (hint + status/help) spanning both below.
func (m Model) renderSettingsBody(innerW, bodyHeight int, isOverlay bool) string {
	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}
	scrollH := max(bodyHeight-settingsFooterRows, 1)
	scopeW := m.settingsScopeWidth()
	rowsW := max(innerW-1-scopeW, 10)

	// Rows pane: a pinned 2-line header (tab bar + blank) above the scrollable
	// settings rows, so the [General] [LSP] tabs stay visible while scrolling.
	contentLines := m.settingsDisplayLines(rowsW)
	contentVisH := max(scrollH-settingsTabHeaderRows, 1)
	rowOff := clampOffset(m.settingsScroll, len(contentLines), contentVisH)
	contentBar := scrollbarCol(len(contentLines), contentVisH, rowOff, isOverlay)
	rowVis := append([]string{m.settingsTabBar(), ""}, contentLines[rowOff:]...)
	var rowBar []string
	if contentBar != nil {
		rowBar = append([]string{SepStyle.Render("│"), SepStyle.Render("│")}, contentBar...)
	}

	scopeLines := m.settingsScopeLines(scopeW)
	scopeOff := clampOffset(m.settingsScopeScroll, len(scopeLines), scrollH)
	scopeVis := scopeLines[scopeOff:]
	scopeBar := scrollbarCol(len(scopeLines), scrollH, scopeOff, isOverlay)

	var sb strings.Builder
	for i := range bodyHeight {
		if i >= scrollH {
			footerIdx := i - scrollH
			footer := m.settingsFooterRow(footerIdx, innerW, isOverlay)
			if footerIdx == 0 {
				// Extend the scope/rows divider one row into the blank separator so the
				// vertical line reaches the footer instead of stopping a row short.
				footer = settingsBlankDividerRow(scopeW, innerW, isOverlay || m.settingsScopeFocus)
			}
			sb.WriteString(sepStyle.Render("│") + footer + sepStyle.Render("│") + "\n")
			continue
		}
		scope, _ := bodyColumn(scopeVis, scopeBar, i)
		row, rightEdge := bodyColumn(rowVis, rowBar, i)
		div := SepStyle.Render("┆")
		if scopeBar != nil && i < len(scopeBar) {
			div = scopeBar[i]
		}
		scopeCell := lipgloss.NewStyle().Width(scopeW).Render(ansi.Truncate(scope, scopeW-1, "…") + " ")
		rowCell := lipgloss.NewStyle().Width(rowsW).Render(row)
		if isOverlay {
			scopeCell = InactiveStyle.Render(ansi.Strip(scopeCell))
			rowCell = InactiveStyle.Render(ansi.Strip(rowCell))
			div = SepInactiveStyle.Render("┆")
		} else if m.settingsScopeFocus {
			// Scope column has focus — dim the rows pane so the active pane stands out.
			rowCell = InactiveStyle.Render(ansi.Strip(rowCell))
			div = SepInactiveStyle.Render("┆")
		}
		sb.WriteString(sepStyle.Render("│") + scopeCell + div + rowCell + rightEdge + "\n")
	}
	return sb.String()
}

// settingsBlankDividerRow renders a blank separator row that still carries the
// scope/rows divider, so the vertical line extends one row below the last
// settings row to meet the footer (no gap). The divider dims when the rows pane
// is inactive (overlay or scope-focused), matching the body divider.
func settingsBlankDividerRow(scopeW, innerW int, dim bool) string {
	div := SepStyle.Render("┆")
	if dim {
		div = SepInactiveStyle.Render("┆")
	}
	left := lipgloss.NewStyle().Width(scopeW).Render("")
	right := lipgloss.NewStyle().Width(max(innerW-scopeW-1, 0)).Render("")
	return left + div + right
}

// clampOffset bounds a scroll offset to [0, total-visible].
func clampOffset(off, total, visible int) int {
	if maxOff := max(total-visible, 0); off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	return off
}

// settingsScopeLines renders the left Scope column: Global first (filled dot),
// then one row per active workspace. The selected scope drives which config the
// rows on the right edit.
func (m Model) settingsScopeLines(w int) []string {
	focused := m.settingsScopeFocus
	titleStyle := PanelHeaderFadedStyle
	if focused {
		titleStyle = PanelHeaderStyle
	}
	lines := []string{titleStyle.Render(" Scope"), ""}
	for i, sc := range m.settingsScopes {
		selected := i == m.settingsScopeCursor
		// One first-column marker: the cursor (❯) when selected, otherwise a
		// muted bullet (∙).
		marker := "∙"
		if selected {
			marker = "❯"
		}
		label := sc.label
		avail := max(w-4, 4)
		if r := []rune(label); len(r) > avail {
			label = string(r[:avail-1]) + "…"
		}
		line := " " + marker + " " + label
		switch {
		case selected:
			lines = append(lines, SelectedStyle.Render(line))
		case focused:
			lines = append(lines, ItemStyle.Render(line))
		default:
			lines = append(lines, FadedStyle.Render(line))
		}
	}
	return lines
}

// settingsDisplayLines renders the scrollable logical lines to display strings
// for the rows pane (width rowsW). In a workspace scope each row shows whether
// it is a workspace override or inherited; in Global scope it shows the reload
// tier.
func (m Model) settingsDisplayLines(rowsW int) []string {
	if len(m.settingsItems) == 0 {
		msg := "  (no settings in this tab)"
		if m.settingsTab == settingsTabLSP {
			msg = "  (no language servers configured — add [lsp.<lang>] to config)"
		}
		return []string{MutedStyle.Render(msg)}
	}
	labelW, valueW := settingsColumnWidths(m.settingsItems)
	logical := settingsLogicalLines(m.settingsItems)
	wsScope := !m.currentScope().global
	missing := map[string]bool{} // language groups with an enabled-but-missing server
	for _, it := range m.settingsItems {
		if it.lspMissing {
			missing[it.group] = true
		}
	}
	out := make([]string, len(logical))
	for i, ln := range logical {
		switch ln.kind {
		case slHeader:
			out[i] = settingsHeaderDisplay(ln.group, rowsW, missing[ln.group])
		case slRow:
			it := m.settingsItems[ln.item]
			if ln.cont > 0 {
				out[i] = settingsContLine(it, ln.cont, labelW, wsScope)
			} else {
				out[i] = settingsRowDisplay(it, ln.item == m.settingsCursor, wsScope, labelW, valueW)
			}
		default:
			out[i] = ""
		}
	}
	return out
}

// settingsContLine renders a list-entry continuation line, padded so the entry
// aligns under the value column of the row above. Missing-LSP rows render red.
func settingsContLine(it settingItem, idx, labelW int, wsScope bool) string {
	style := DetailStyle
	if wsScope && !it.overridden {
		style = FadedStyle
	}
	if it.lspMissing {
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	}
	entry := ""
	if idx < len(it.list) {
		entry = it.list[idx]
	}
	return strings.Repeat(" ", labelW+3) + style.Render(entry)
}

// settingsHeaderDisplay renders a group header as the name followed by a faded
// dotted rule that fills to the right gap (1 space from each border).
func settingsHeaderDisplay(group string, innerW int, warn bool) string {
	marker := ""
	if warn { // an enabled LSP server in this group is not on PATH
		marker = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("*")
	}
	used := 1 + lipgloss.Width(group) + lipgloss.Width(marker) + 1 // " " + name + marker + " "
	dots := max(innerW-1-used, 0)
	return " " + PanelHeaderFadedStyle.Render(group) + marker + " " + SepStyle.Render(strings.Repeat("╌", dots))
}

// settingsRowDisplay renders one aligned settings row: 1-space gap, cursor,
// fixed-width label and value columns, the control. In Global scope the
// reload-tier numeral sits right after the setting name (¹ live / ² next session
// / ³ restart — see settingsHintContent for the legend); in a workspace scope a
// superscript ⁴/⁵ after the numeral marks override vs inherited.
func settingsRowDisplay(it settingItem, focused, wsScope bool, labelW, valueW int) string {
	label := rowLabel(it)
	value := fmt.Sprintf("%-*s", valueW, rowValues(it)[0])
	ctrl := settingControl(it)

	numeral, numeralPlain := reloadNumeral(it.key)
	// Workspace scope: a superscript marker after the tier numeral flags the row
	// as an override (⁴) or inherited (⁵).
	mark, markPlain := "", ""
	if wsScope {
		mark, markPlain = workspaceMark(it.overridden)
	}
	markers := numeralPlain + markPlain
	pad := strings.Repeat(" ", max(labelW-lipgloss.Width(label)-lipgloss.Width(markers), 0))

	// Dim inherited rows so workspace overrides stand out; flag a missing LSP
	// server's whole block in red.
	labelStyle, valueStyle := ItemStyle, DetailStyle
	if wsScope && !it.overridden {
		labelStyle, valueStyle = FadedStyle, FadedStyle
	}
	if it.lspMissing {
		red := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
		labelStyle, valueStyle = red, red
	}

	var core string
	if focused {
		// One SelectedStyle pass, so the markers take the selection colour.
		core = SelectedStyle.Render("❯ " + label + markers + pad + value + ctrl)
	} else {
		core = "  " + labelStyle.Render(label) + numeral + mark + pad + valueStyle.Render(value) + MutedStyle.Render(ctrl)
	}
	return " " + core
}

// reloadNumeral returns the coloured reload-tier numeral and its plain rune (the
// plain form is used in the focused row's single SelectedStyle render).
func reloadNumeral(key settingKey) (coloured, plain string) {
	switch reloadTierFor(key) {
	case reloadNextSession:
		return WarnStyle.Render("²"), "²"
	case reloadRestart:
		return RestartStyle.Render("³"), "³"
	default:
		return OkStyle.Render("¹"), "¹"
	}
}

// workspaceMark returns the coloured + plain superscript that flags a workspace
// row as an override (⁴, green) or inherited (⁵, muted), shown right after the
// reload-tier numeral on the label.
func workspaceMark(overridden bool) (coloured, plain string) {
	if overridden {
		return OkStyle.Render("⁴"), "⁴"
	}
	return MutedStyle.Render("⁵"), "⁵"
}

// settingsFooterRow renders one of the three pinned footer rows: a blank
// separator (0), the key-hint bar (1), and the status bar (2).
func (m Model) settingsFooterRow(idx, innerW int, isOverlay bool) string {
	contentW := max(innerW-4, 0)
	switch idx {
	case 1:
		return statusBarLine(settingsHintContent(contentW, !m.currentScope().global), innerW, isOverlay)
	case 2:
		return statusBarLine(settingsStatusContent(m.settingsStatusOrHelp(), contentW), innerW, isOverlay)
	default:
		return lipgloss.NewStyle().Width(innerW).Render("")
	}
}

// settingsStatusOrHelp returns the transient action status when one is set,
// otherwise the focused row's one-line help — so the second status-bar line
// describes the highlighted setting whenever the user is just navigating.
func (m Model) settingsStatusOrHelp() string {
	if m.settingsStatus != "" {
		return m.settingsStatus
	}
	if m.settingsCursor >= 0 && m.settingsCursor < len(m.settingsItems) {
		return m.settingsItems[m.settingsCursor].help
	}
	return ""
}

// statusBarLine frames footer content on a subtle background bar: a 1-space
// plain gap from each border, then the background — within which the content is
// inset one further space on each side, so text begins one column into the
// background. content must already be exactly innerW-4 wide and styled.
func statusBarLine(content string, innerW int, isOverlay bool) string {
	if isOverlay {
		return lipgloss.NewStyle().Width(innerW).Render("  " + ansi.Strip(content))
	}
	return " " + SettingsBarStyle.Render(" ") + content + SettingsBarStyle.Render(" ") + " "
}

// settingsHintContent builds the hint bar: a legend on the left (the reload
// tiers in Global scope, the inherit/override key in a workspace scope) and the
// navigation shortcuts (brighter keys) on the right.
func settingsHintContent(contentW int, wsScope bool) string {
	legend := settingsLegend(wsScope)
	shortcut := SettingsBarKeyStyle.Render("↑↓") + SettingsBarStyle.Render(" move  ·  ") +
		SettingsBarKeyStyle.Render("←→") + SettingsBarStyle.Render(" change  ·  ") +
		SettingsBarKeyStyle.Render("tab") + SettingsBarStyle.Render(" panes  ·  ") +
		SettingsBarKeyStyle.Render("[ ]") + SettingsBarStyle.Render(" width")
	shortcutW := lipgloss.Width("↑↓ move  ·  ←→ change  ·  tab panes  ·  [ ] width")
	gap := max(contentW-lipgloss.Width(legend)-shortcutW, 1)
	return legend + SettingsBarStyle.Render(strings.Repeat(" ", gap)) + shortcut
}

// settingsLegend renders the left-hand legend on the status bar. Global scope
// explains the reload-tier numerals with matching colours (¹ green, ² yellow,
// ³ purple); a workspace scope explains the override/inherit marks. All segments
// carry the bar background.
func settingsLegend(wsScope bool) string {
	ok := SettingsBarStyle.Foreground(ActiveTheme.Success)
	warn := SettingsBarStyle.Foreground(ActiveTheme.Warning)
	restart := SettingsBarStyle.Foreground(lipgloss.Color("#9D7CD8"))
	muted := SettingsBarStyle.Foreground(ActiveTheme.TextMuted)
	legend := ok.Render("¹") + SettingsBarStyle.Render(" immediate  ·  ") +
		warn.Render("²") + SettingsBarStyle.Render(" new sessions  ·  ") +
		restart.Render("³") + SettingsBarStyle.Render(" daemon restart")
	if wsScope {
		legend += SettingsBarStyle.Render("  ·  ") +
			ok.Render("⁴") + SettingsBarStyle.Render(" override  ·  ") +
			muted.Render("⁵") + SettingsBarStyle.Render(" inherited")
	}
	return legend
}

// settingsStatusContent left-aligns the status message on the bar, padded with
// the background colour to the full content width.
func settingsStatusContent(text string, contentW int) string {
	if lipgloss.Width(text) > contentW {
		text = truncate(text, contentW)
	}
	pad := max(contentW-lipgloss.Width(text), 0)
	return SettingsBarMsgStyle.Render(text) + SettingsBarStyle.Render(strings.Repeat(" ", pad))
}

// settingControl renders the interactive control affordance for a row. Cycle
// rows expose their full option set so the choices are discoverable; the
// current value lives in the row's value column.
func settingControl(it settingItem) string {
	switch it.kind {
	case settingPopup:
		return "›"
	case settingToggle:
		return "[ " + it.value + " ]"
	case settingCycle:
		return "‹ " + strings.Join(it.options, "·") + " ›"
	case settingNumber:
		return "‹ -/+ ›"
	case settingList, settingText:
		return "‹ edit ›"
	default:
		return ""
	}
}

// pickerLine is one rendered row of the theme picker: raw text plus the style
// to apply. A zero value renders as a blank padding line. When header is set,
// the text is rendered as a section name followed by a faded dotted rule.
type pickerLine struct {
	text   string
	style  lipgloss.Style
	header bool
}

// themeGroups splits the sorted theme names into dark and light buckets.
func themeGroups() (dark, light []string) {
	for _, n := range ThemeNames() {
		if isLightTheme(AvailableThemes[n]) {
			light = append(light, n)
		} else {
			dark = append(dark, n)
		}
	}
	return dark, light
}

// themePickerOrder is the cursor navigation order: dark themes first, then
// light, matching the grouped layout in renderThemePicker.
func themePickerOrder() []string {
	dark, light := themeGroups()
	return append(dark, light...)
}

// renderThemePicker draws the centred theme-picker modal over a dimmed
// background. Moving the cursor applies and saves the theme live, so the dimmed
// background re-renders in the highlighted theme — that is the preview.
func (m Model) renderThemePicker(bg string) string {
	dark, light := themeGroups()
	rows := m.themePickerRows(dark, light)

	contentW := lipgloss.Width("↑↓ apply + save  ·  esc close")
	for _, ln := range rows {
		if !ln.header {
			if w := lipgloss.Width(ln.text); w > contentW {
				contentW = w
			}
		}
	}
	const pad = 3 // blank columns on each side of the row content
	innerW := contentW + pad*2

	var b strings.Builder
	b.WriteString(themePickerTop(innerW) + "\n")
	for _, ln := range rows {
		b.WriteString(themePickerBodyLine(ln, innerW, pad, contentW) + "\n")
	}
	// Footer status bar (same treatment as the Settings status bar).
	b.WriteString(SepStyle.Render("│") + statusBarLine(themePickerFooterContent(innerW-4), innerW, false) + SepStyle.Render("│") + "\n")
	b.WriteString(SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯"))

	return spliceOverlay(bg, b.String(), m.width, m.height)
}

// themePickerRows builds the grouped body of the picker: a blank line at the
// top, a Dark and a Light section (dotted header + rows), and a blank line
// before the footer (rendered separately).
func (m Model) themePickerRows(dark, light []string) []pickerLine {
	var lines []pickerLine
	blank := func() { lines = append(lines, pickerLine{}) }
	flat := 0
	group := func(title string, names []string) {
		lines = append(lines, pickerLine{text: title, header: true})
		for _, n := range names {
			sel := flat == m.themePickerCursor
			st := ItemStyle
			if sel {
				st = SelectedStyle
			}
			lines = append(lines, pickerLine{text: themePickerRow(n, sel), style: st})
			flat++
		}
	}

	blank()
	if len(dark) > 0 {
		group("Dark", dark)
	}
	if len(light) > 0 {
		if len(dark) > 0 {
			blank()
		}
		group("Light", light)
	}
	blank()
	return lines
}

// themePickerRow formats one theme row: a cursor (❯) when focused and a ✓ after
// the name when it is the active theme.
func themePickerRow(name string, cursor bool) string {
	c := "  "
	if cursor {
		c = "❯ "
	}
	row := c + name
	if name == ActiveThemeName {
		row += " ✓"
	}
	return row
}

func themePickerTop(innerW int) string {
	title := " Theme "
	dashes := max(innerW-1-lipgloss.Width(title), 0)
	return SepStyle.Render("╭─") + PanelHeaderStyle.Render(title) + SepStyle.Render(strings.Repeat("─", dashes)+"╮")
}

func themePickerBodyLine(ln pickerLine, innerW, pad, contentW int) string {
	var styled string
	var vis int
	if ln.header {
		dots := max(contentW-lipgloss.Width(ln.text)-1, 0)
		styled = PanelHeaderFadedStyle.Render(ln.text) + " " + SepStyle.Render(strings.Repeat("╌", dots))
		vis = lipgloss.Width(ln.text) + 1 + dots
	} else {
		styled = ln.style.Render(ln.text)
		vis = lipgloss.Width(ln.text)
	}
	rpad := max(innerW-pad-vis, 0)
	body := strings.Repeat(" ", pad) + styled + strings.Repeat(" ", rpad)
	return SepStyle.Render("│") + body + SepStyle.Render("│")
}

// themePickerFooterContent centres the apply/close hint (brighter keys) within
// the footer content width.
func themePickerFooterContent(contentW int) string {
	hint := SettingsBarKeyStyle.Render("↑↓") + SettingsBarStyle.Render(" apply + save  ·  ") +
		SettingsBarKeyStyle.Render("esc") + SettingsBarStyle.Render(" close")
	w := lipgloss.Width("↑↓ apply + save  ·  esc close")
	left := max((contentW-w)/2, 0)
	right := max(contentW-w-left, 0)
	return SettingsBarStyle.Render(strings.Repeat(" ", left)) + hint + SettingsBarStyle.Render(strings.Repeat(" ", right))
}

// isLightTheme heuristically identifies a light theme by checking whether
// the SelectionBackground is brighter than a mid-point luminance threshold.
// The result is used only for the "dark / light" badge in the theme list.
func isLightTheme(th Theme) bool {
	if th.SelectionBackground == nil {
		return false
	}
	r, g, b, _ := th.SelectionBackground.RGBA()
	// RGBA() returns 16-bit values (0–65535); scale to 0–255.
	lum := (float64(r>>8)*299 + float64(g>>8)*587 + float64(b>>8)*114) / 1000
	return lum > 127
}
