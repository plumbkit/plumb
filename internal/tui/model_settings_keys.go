package tui

import (
	"bufio"
	"fmt"
	"net"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/config"
)

// settingsStatusMsg carries the result of an asynchronous settings action
// (currently the live log-level push) back into the model.
type settingsStatusMsg struct{ text string }

// enterSettings loads a fresh global-config snapshot and rebuilds the settings
// rows. Called when the Settings section becomes active.
func (m *Model) enterSettings() {
	cfg, err := config.Load()
	if err != nil {
		cfg = config.Defaults()
		m.settingsStatus = "config unreadable; showing defaults"
	} else {
		m.settingsStatus = ""
	}
	m.settingsCfg = cfg
	m.settingsScopes = m.collectSettingsScopes()
	if m.settingsScopeCursor >= len(m.settingsScopes) {
		m.settingsScopeCursor = 0
	}
	m.settingsItems = m.buildScopeItems()
	m.settingsCursor = 0
	m.settingsScroll = 0
	m.settingsScopeFocus = false
	m.showThemePicker = false
	m.settingsListEditor = nil
	m.syncThemeCursor()
}

// settingsScrollHeight is the visible height of the scrollable settings list
// (the body minus the pinned footer rows).
func (m Model) settingsScrollHeight() int {
	return max(m.height-6-settingsFooterRows, 1)
}

func (m Model) settingsPageSize() int {
	return max(m.settingsScrollHeight()-1, 1)
}

// ensureSettingsCursorVisible scrolls so the focused row stays on screen.
func (m *Model) ensureSettingsCursorVisible() {
	lines := settingsLogicalLines(m.settingsItems)
	lineIdx := -1
	for i, ln := range lines {
		if ln.kind == slRow && ln.item == m.settingsCursor {
			lineIdx = i
			break
		}
	}
	if lineIdx < 0 {
		return
	}
	scrollH := m.settingsScrollHeight()
	if lineIdx < m.settingsScroll {
		m.settingsScroll = lineIdx
	} else if lineIdx >= m.settingsScroll+scrollH {
		m.settingsScroll = lineIdx - scrollH + 1
	}
}

// scrollSettings adjusts the settings scroll offset by delta (mouse wheel),
// clamped to the content bounds.
func (m *Model) scrollSettings(delta int) {
	m.settingsScroll += delta
	maxOff := max(len(settingsLogicalLines(m.settingsItems))-m.settingsScrollHeight(), 0)
	if m.settingsScroll > maxOff {
		m.settingsScroll = maxOff
	}
	if m.settingsScroll < 0 {
		m.settingsScroll = 0
	}
}

// selectSettingAtBodyRow moves the cursor to the row clicked at screen row y,
// when it maps to a selectable settings row.
func (m *Model) selectSettingAtBodyRow(y int) {
	row := y - bodyStartRow
	if row < 0 || row >= m.settingsScrollHeight() {
		return
	}
	lines := settingsLogicalLines(m.settingsItems)
	idx := m.settingsScroll + row
	if idx < 0 || idx >= len(lines) {
		return
	}
	if lines[idx].kind == slRow {
		m.settingsCursor = lines[idx].item
	}
}

// syncThemeCursor points themePickerCursor at the live ActiveThemeName, using
// the picker's grouped (dark-then-light) navigation order.
func (m *Model) syncThemeCursor() {
	for i, n := range themePickerOrder() {
		if n == ActiveThemeName {
			m.themePickerCursor = i
			return
		}
	}
}

func (m Model) handleSettingsSectionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		return m.mainKeyQuit()
	case "/":
		m.sectionMenuOpen = true
		m.sectionMenuCursor = m.currentSection
	case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "alt+1", "alt+2", "alt+3", "alt+4", "alt+5":
		m.selectSectionShortcut(msg.String())
	case "ctrl+h":
		m.showHelp = true
	case "esc":
		m.sectionMenuOpen = true
		m.sectionMenuCursor = m.currentSection
	default:
		return m.handleSettingsNavKey(msg.String())
	}
	return m, nil
}

// handleSettingsNavKey handles the in-list navigation and edit keys, kept
// separate from the section/global keys to keep each handler simple. When the
// Scope column is focused it routes to handleScopeNavKey instead.
func (m Model) handleSettingsNavKey(key string) (Model, tea.Cmd) {
	if m.settingsScopeFocus {
		return m.handleScopeNavKey(key)
	}
	switch key {
	case "tab":
		m.settingsScopeFocus = true
		return m, nil
	case "up", "k":
		if m.settingsCursor > 0 {
			m.settingsCursor--
			m.settingsStatus = "" // show the focused row's help, not a stale confirmation
			m.ensureSettingsCursorVisible()
		}
	case "down", "j":
		if m.settingsCursor < len(m.settingsItems)-1 {
			m.settingsCursor++
			m.settingsStatus = ""
			m.ensureSettingsCursorVisible()
		}
	case "pgup":
		m.settingsCursor = max(m.settingsCursor-m.settingsPageSize(), 0)
		m.settingsStatus = ""
		m.ensureSettingsCursorVisible()
	case "pgdown":
		m.settingsCursor = min(m.settingsCursor+m.settingsPageSize(), len(m.settingsItems)-1)
		m.settingsStatus = ""
		m.ensureSettingsCursorVisible()
	case "enter", " ":
		return m.afterSettingChange(m.activateSetting(), nil)
	case "left", "-":
		return m.afterSettingChange(m.adjustSetting(-1))
	case "right", "+", "=":
		return m.afterSettingChange(m.adjustSetting(1))
	case "backspace", "delete":
		// In a workspace scope, reset the focused row to inherit (no-op in Global).
		return m.afterSettingChange(m.resetToInherit(), nil)
	}
	return m, nil
}

// handleScopeNavKey drives the Scope column: up/down pick a scope (reloading the
// rows for it), tab/right/enter return focus to the rows pane.
func (m Model) handleScopeNavKey(key string) (Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.settingsScopeCursor > 0 {
			m.settingsScopeCursor--
			m.selectScope()
		}
	case "down", "j":
		if m.settingsScopeCursor < len(m.settingsScopes)-1 {
			m.settingsScopeCursor++
			m.selectScope()
		}
	case "tab", "right", "l", "enter", " ":
		m.settingsScopeFocus = false
	}
	return m, nil
}

// selectScope reloads the rows for the newly-selected scope and resets the row
// cursor/scroll so navigation starts at the top of the new scope's settings.
func (m *Model) selectScope() {
	m.settingsStatus = ""
	m.settingsCursor = 0
	m.settingsScroll = 0
	m.settingsScopeScroll = clampOffset(m.settingsScopeScroll, len(m.settingsScopes)+2, m.settingsScrollHeight())
	m.settingsItems = m.buildScopeItems()
}

// activateSetting handles enter/space: opens the theme picker for the popup row
// and flips toggles; numeric and cycle rows are changed with ←→ instead.
func (m Model) activateSetting() Model {
	if m.settingsCursor < 0 || m.settingsCursor >= len(m.settingsItems) {
		return m
	}
	it := m.settingsItems[m.settingsCursor]
	switch it.kind {
	case settingPopup:
		if it.key == skLogFile {
			m.settingsStatus = "log file path is edited directly in config.toml"
			return m
		}
		m.showThemePicker = true
		m.syncThemeCursor()
		return m
	case settingToggle:
		return m.toggleBool(it.key, it.value == "on")
	case settingList:
		return m.openListEditor(it.key)
	default:
		return m
	}
}

// openListEditor opens the list-value editor for a settingList row, seeded with
// the effective list for the current scope.
func (m Model) openListEditor(key settingKey) Model {
	m.settingsListEditor = newListEditor(key, listLabel(key), m.effectiveList(key))
	return m
}

// handleListEditorKey routes a key to the open list editor. On commit it persists
// the entries to the current scope and pushes the appropriate reload.
func (m Model) handleListEditorKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.settingsListEditor == nil {
		return m, nil
	}
	if msg.String() == "ctrl+c" {
		return m.mainKeyQuit()
	}
	done, save := m.settingsListEditor.Update(msg)
	if !done {
		return m, nil
	}
	if save {
		return m.afterSettingChange(m.commitListEditor(), nil)
	}
	m.settingsListEditor = nil // esc — cancel, discard the edits
	return m, nil
}

// commitListEditor writes the editor's entries to the active scope and closes it.
func (m Model) commitListEditor() Model {
	ed := m.settingsListEditor
	m.settingsListEditor = nil
	if ed == nil {
		return m
	}
	entries := append([]string(nil), ed.entries...)
	apply := func(c *config.Config) {
		if p := listField(c, ed.key); p != nil {
			*p = entries
		}
	}
	if m.applyScopedSetting(ed.key, entries, apply) {
		m.settingsStatus = m.scopedStatus(ed.key, fmt.Sprintf("%s → %d entr%s", listLabel(ed.key), len(entries), plural(len(entries))))
	}
	return m
}

// effectiveList returns the list value for key in the current scope: the merged
// project value in a workspace scope, the global snapshot in Global.
func (m Model) effectiveList(key settingKey) []string {
	cfg := m.settingsCfg
	if scope := m.currentScope(); !scope.global {
		if merged, err := config.LoadProject(m.settingsCfg, scope.folder); err == nil {
			cfg = merged
		}
	}
	if p := listField(&cfg, key); p != nil {
		return append([]string(nil), (*p)...)
	}
	return nil
}

// listField returns a pointer to the []string config field a list row edits.
func listField(c *config.Config, key settingKey) *[]string {
	switch key {
	case skExtraRoots:
		return &c.Workspace.ExtraRoots
	case skReadRoots:
		return &c.Workspace.ReadRoots
	case skProtectedBranches:
		return &c.Git.ProtectedBranches
	case skExcludePatterns:
		return &c.Topology.ExcludePatterns
	case skAnalysers:
		return &c.Quality.Analysers
	default:
		return nil
	}
}

// listLabel is the human label for a list setting (editor title + status line).
func listLabel(key settingKey) string {
	switch key {
	case skExtraRoots:
		return "extra_roots"
	case skReadRoots:
		return "read_roots"
	case skProtectedBranches:
		return "protected_branches"
	case skExcludePatterns:
		return "exclude_patterns"
	case skAnalysers:
		return "analysers"
	default:
		return ""
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// adjustSetting changes the focused row's value by dir (−1 / +1). Dispatch is
// kind-driven so adding a setting only extends the field mappers, not this
// switch (keeping every handler well under gocyclo 15).
func (m Model) adjustSetting(dir int) (Model, tea.Cmd) {
	if m.settingsCursor < 0 || m.settingsCursor >= len(m.settingsItems) {
		return m, nil
	}
	it := m.settingsItems[m.settingsCursor]
	switch it.kind {
	case settingToggle:
		return m.toggleBool(it.key, it.value == "on"), nil
	case settingNumber:
		return m.setNumber(it, dir), nil
	case settingCycle:
		return m.adjustCycle(it, dir)
	default:
		return m, nil
	}
}

// adjustCycle routes cycle rows. The global-only cycles (log level applies live,
// log format / path style, the cache/lsp durations) keep their bespoke setters
// and are only ever reached in Global scope; every project-overridable string
// cycle (quality mode) flows through the scope-aware setCycle.
func (m Model) adjustCycle(it settingItem, dir int) (Model, tea.Cmd) {
	switch it.key {
	case skLogLevel:
		return m.setLogLevel(cycleOption(logLevelOptions, m.settingsCfg.LogLevel, dir))
	case skLogFormat:
		return m.setLogFormat(cycleOption(logFormatOptions, m.settingsCfg.LogFormat, dir)), nil
	case skPathStyle:
		return m.setPathStyle(cycleOption(pathStyleOptions, m.settingsCfg.UI.PathStyle, dir)), nil
	case skCacheTTL, skLSPTimeout:
		return m.setDuration(it.key, dir), nil
	default:
		return m.setCycle(it, dir), nil
	}
}

// setNumber adjusts a numeric field by its per-field step and persists it in the
// current scope (global config or the workspace's project config). The current
// value is read from the focused row, so it reflects the merged effective value
// in a workspace scope, not the global snapshot.
func (m Model) setNumber(it settingItem, dir int) Model {
	step, label := numberMeta(it.key)
	if it.key == skTopoMaxFileSize { // the only int64 field
		var cur int64
		_, _ = fmt.Sscanf(it.value, "%d", &cur)
		n := cur + int64(dir*step)
		if n < 0 {
			n = 0
		}
		if m.applyScopedSetting(it.key, n, func(c *config.Config) { c.Topology.MaxFileSizeBytes = n }) {
			m.settingsStatus = m.scopedStatus(it.key, fmt.Sprintf("%s → %d", label, n))
		}
		return m
	}
	cur := 0
	if it.value != "off" { // rate limit renders 0 as "off"
		_, _ = fmt.Sscanf(it.value, "%d", &cur)
	}
	n := cur + dir*step
	if n < 0 {
		n = 0
	}
	apply := func(c *config.Config) {
		if p := intField(c, it.key); p != nil {
			*p = n
		}
	}
	if m.applyScopedSetting(it.key, n, apply) {
		m.settingsStatus = m.scopedStatus(it.key, fmt.Sprintf("%s → %d", label, n))
	}
	return m
}

// setCycle cycles a generic string-enum field from its current effective value
// and persists it in the current scope.
func (m Model) setCycle(it settingItem, dir int) Model {
	opts, set, label := cycleMeta(it.key)
	if set == nil {
		return m
	}
	next := cycleOption(opts, it.value, dir)
	if m.applyScopedSetting(it.key, next, func(c *config.Config) { set(c, next) }) {
		m.settingsStatus = m.scopedStatus(it.key, label+" → "+next)
	}
	return m
}

// numberMeta returns the adjust step and status label for a numeric setting.
func numberMeta(key settingKey) (int, string) {
	switch key {
	case skRateLimit:
		return 10, "rate limit"
	case skCacheMaxSize:
		return 100, "cache max_size"
	case skPostWriteDiagMs:
		return 50, "post-write diag (ms)"
	case skConcurrentSkewMs:
		return 25, "concurrent skew (ms)"
	case skTopoMaxFileSize:
		return 65536, "max file size (B)"
	case skTopoResyncBatch:
		return 25, "resync batch"
	case skTopoResyncPauseMs:
		return 5, "resync pause (ms)"
	case skTopoResyncIntervalMin:
		return 5, "resync interval (min)"
	case skQualityTimeoutMs:
		return 500, "quality timeout (ms)"
	case skQualityMaxFindings:
		return 1, "max findings/file"
	case skIdleThresholdMin:
		return 5, "idle threshold (min)"
	case skEvictionTTLMin:
		return 5, "eviction ttl (min)"
	default:
		return 1, ""
	}
}

// intField returns a pointer to the int config field a numeric row edits
// (excluding the two bespoke ones and the int64 topology cap).
func intField(c *config.Config, key settingKey) *int {
	switch key {
	case skRateLimit:
		return &c.Edits.RateLimitPerMinute
	case skCacheMaxSize:
		return &c.Cache.MaxSize
	case skPostWriteDiagMs:
		return &c.Edits.PostWriteDiagnosticsMs
	case skConcurrentSkewMs:
		return &c.Edits.ConcurrentWriteSkewMs
	case skTopoResyncBatch:
		return &c.Topology.ResyncBatch
	case skTopoResyncPauseMs:
		return &c.Topology.ResyncPauseMs
	case skTopoResyncIntervalMin:
		return &c.Topology.ResyncIntervalMinutes
	case skQualityTimeoutMs:
		return &c.Quality.TimeoutMs
	case skQualityMaxFindings:
		return &c.Quality.MaxFindingsPerFile
	case skIdleThresholdMin:
		return &c.Session.IdleThresholdMinutes
	case skEvictionTTLMin:
		return &c.Session.EvictionTTLMinutes
	default:
		return nil
	}
}

// cycleMeta returns the option set, setter, and label for a generic string-enum
// setting. The current value comes from the focused row, so this need not read
// any config snapshot (which would be the global one, wrong in a workspace scope).
func cycleMeta(key settingKey) ([]string, func(*config.Config, string), string) {
	switch key {
	case skQualityMode:
		return qualityModeOptions, func(c *config.Config, v string) { c.Quality.Mode = v }, "quality mode"
	default:
		return nil, nil, ""
	}
}

func (m Model) setLogLevel(lvl string) (Model, tea.Cmd) {
	if !m.persist(func(c *config.Config) { c.LogLevel = lvl }) {
		return m, nil
	}
	m.settingsCfg.LogLevel = lvl
	m.settingsItems = buildSettingItems(m.settingsCfg)
	m.settingsStatus = "log level → " + lvl
	m.pendingReload = false // log level applies live via set-level, not reload-config
	return m, m.applyLogLevelLive(lvl)
}

func (m Model) setLogFormat(format string) Model {
	if m.persist(func(c *config.Config) { c.LogFormat = format }) {
		m.settingsCfg.LogFormat = format
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(skLogFormat, "log format → "+format)
	}
	return m
}

// toggleBool flips a boolean setting (cur is the current effective value from
// the focused row) and persists it in the current scope.
func (m Model) toggleBool(key settingKey, cur bool) Model {
	v := !cur
	apply := func(c *config.Config) {
		if p := boolField(c, key); p != nil {
			*p = v
		}
	}
	if m.applyScopedSetting(key, v, apply) {
		m.settingsStatus = m.scopedStatus(key, toggleLabel(key)+" "+onOff(v))
	}
	return m
}

// boolField returns a pointer to the bool config field a toggle row edits.
func boolField(c *config.Config, key settingKey) *bool {
	switch key {
	case skStrict:
		return &c.Edits.Strict
	case skShowWriteDiff:
		return &c.Edits.ShowWriteDiff
	case skTopology:
		return &c.Topology.Enabled
	case skTopoResyncOnAttach:
		return &c.Topology.ResyncOnAttach
	case skTopoWatch:
		return &c.Topology.Watch
	case skQuality:
		return &c.Quality.Enabled
	case skRefuseHomeRoots:
		return &c.Walk.RefuseHomeRoots
	case skAutoAttachPersist:
		return &c.Workspace.AutoAttachPersist
	case skAllowDependencyReads:
		return &c.Workspace.AllowDependencyReads
	case skGitWrites:
		return &c.Git.AllowWrites
	case skGitDestructive:
		return &c.Git.AllowDestructive
	case skGitPush:
		return &c.Git.AllowPush
	case skAutoAttach:
		return &c.Workspace.AutoAttach
	default:
		return nil
	}
}

// durField returns the duration config field a cycle row edits and its presets.
func durField(c *config.Config, key settingKey) (*config.Duration, []string) {
	switch key {
	case skCacheTTL:
		return &c.Cache.TTL, cacheTTLOptions
	case skLSPTimeout:
		return &c.LSPQuery.Timeout, lspTimeoutOptions
	default:
		return nil, nil
	}
}

func (m Model) setDuration(key settingKey, dir int) Model {
	ptr, presets := durField(&m.settingsCfg, key)
	if ptr == nil {
		return m
	}
	next := cycleOption(presets, durValue(*ptr, presets), dir)
	d, err := time.ParseDuration(next)
	if err != nil {
		return m
	}
	if m.persist(func(c *config.Config) { p, _ := durField(c, key); p.Duration = d }) {
		ptr.Duration = d
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(key, durLabel(key)+" → "+next)
	}
	return m
}

func durLabel(key settingKey) string {
	switch key {
	case skCacheTTL:
		return "cache ttl"
	case skLSPTimeout:
		return "lsp query timeout"
	default:
		return ""
	}
}

// persist writes a change to the global config, recording a failure in the
// status line. Returns true on success.
func (m *Model) persist(apply func(*config.Config)) bool {
	if err := config.Save(apply); err != nil {
		m.settingsStatus = "save failed: " + err.Error()
		return false
	}
	m.pendingReload = true
	return true
}

// afterSettingChange appends a best-effort daemon config-reload push when a
// persisted setting changed, so live-reloadable settings take effect without a
// restart. Theme and log level use their own live paths and leave the flag clear.
func (m Model) afterSettingChange(next Model, cmd tea.Cmd) (Model, tea.Cmd) {
	if next.pendingProjectReload != "" {
		ws := next.pendingProjectReload
		next.pendingProjectReload = ""
		return next, tea.Batch(cmd, next.applyProjectReloadLive(ws))
	}
	if !next.pendingReload {
		return next, cmd
	}
	next.pendingReload = false
	return next, tea.Batch(cmd, next.applyConfigReloadLive())
}

// applyProjectReloadLive pushes a reload-project command to the running daemon
// so a just-saved per-workspace setting takes effect immediately for the
// sessions on that workspace (and only those). Best-effort, mirroring
// applyConfigReloadLive.
func (m Model) applyProjectReloadLive(ws string) tea.Cmd {
	ctrlPath := m.ctrlPath
	return func() tea.Msg {
		conn, err := net.Dial("unix", ctrlPath)
		if err != nil {
			return settingsStatusMsg{text: "saved (daemon not running)"}
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "reload-project %s\n", ws); err != nil {
			return settingsStatusMsg{text: "saved (daemon unreachable)"}
		}
		_, _ = bufio.NewReader(conn).ReadString('\n')
		return nil
	}
}

// applyConfigReloadLive pushes a reload-config command to the running daemon so
// a just-saved setting takes effect live. Best-effort: on success the per-setting
// status is left intact; only an unreachable daemon annotates the status.
func (m Model) applyConfigReloadLive() tea.Cmd {
	ctrlPath := m.ctrlPath
	return func() tea.Msg {
		conn, err := net.Dial("unix", ctrlPath)
		if err != nil {
			return settingsStatusMsg{text: "saved (daemon not running)"}
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "reload-config\n"); err != nil {
			return settingsStatusMsg{text: "saved (daemon unreachable)"}
		}
		_, _ = bufio.NewReader(conn).ReadString('\n')
		return nil
	}
}

// applyLogLevelLive pushes the new level to the running daemon via its control
// socket, mirroring `plumb log-level`. Best-effort: a missing daemon is not an
// error, the change is still persisted for the next start.
func (m Model) applyLogLevelLive(level string) tea.Cmd {
	ctrlPath := m.ctrlPath
	return func() tea.Msg {
		conn, err := net.Dial("unix", ctrlPath)
		if err != nil {
			return settingsStatusMsg{text: "log level → " + level + " saved (daemon not running)"}
		}
		defer conn.Close()
		if _, err := fmt.Fprintf(conn, "set-level %s\n", level); err != nil {
			return settingsStatusMsg{text: "log level → " + level + " saved (daemon unreachable)"}
		}
		_, _ = bufio.NewReader(conn).ReadString('\n')
		return settingsStatusMsg{text: "log level → " + level + " (applied live + saved)"}
	}
}

// maybeOpenThemePicker opens the theme picker for the global ^t shortcut unless
// another overlay (help or the section menu) is already showing.
func (m Model) maybeOpenThemePicker() (Model, tea.Cmd) {
	if m.showHelp || m.sectionMenuOpen {
		return m, nil
	}
	return m.openThemePicker()
}

// openThemePicker opens the theme-picker popup from anywhere (the global ^t
// shortcut). It reuses the same popup and key handler as the Settings Theme row.
func (m Model) openThemePicker() (Model, tea.Cmd) {
	m.showThemePicker = true
	m.syncThemeCursor()
	return m, nil
}

// handleThemePickerKey drives the theme-picker popup: moving the cursor applies
// and saves the theme live; esc/enter closes.
func (m Model) handleThemePickerKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	names := themePickerOrder()
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		return m.mainKeyQuit()
	case "esc", "enter", " ":
		m.showThemePicker = false
	case "up", "k":
		if m.themePickerCursor > 0 {
			m.themePickerCursor--
			m.applyTheme(names[m.themePickerCursor])
		}
	case "down", "j":
		if m.themePickerCursor < len(names)-1 {
			m.themePickerCursor++
			m.applyTheme(names[m.themePickerCursor])
		}
	}
	return m, nil
}

// applyTheme switches the active theme, rebuilds styles, and persists it.
func (m *Model) applyTheme(name string) {
	ActiveTheme = AvailableThemes[name]
	ActiveThemeName = name
	RebuildStyles()
	if err := config.SaveTheme(name); err != nil {
		m.settingsStatus = "save failed: " + err.Error()
	} else {
		m.settingsStatus = "theme → " + name
	}
	m.settingsItems = buildSettingItems(m.settingsCfg)
}

// cycleOption returns the option dir steps away from cur, wrapping around.
func cycleOption(opts []string, cur string, dir int) string {
	idx := 0
	for i, o := range opts {
		if o == cur {
			idx = i
			break
		}
	}
	idx = (idx + dir) % len(opts)
	if idx < 0 {
		idx += len(opts)
	}
	return opts[idx]
}

func (m Model) setPathStyle(style string) Model {
	if m.persist(func(c *config.Config) { c.UI.PathStyle = style }) {
		m.settingsCfg.UI.PathStyle = style
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(skPathStyle, "path style → "+style)
	}
	return m
}

// settingStatus formats the transient status line for a changed setting,
// reflecting when the change takes effect. Driven by reloadTierFor so the
// wording always matches the row's reload-tier marker.
func settingStatus(key settingKey, change string) string {
	switch reloadTierFor(key) {
	case reloadNextSession:
		return change + " · applies to new sessions"
	case reloadRestart:
		return change + " · applies on next daemon start"
	default:
		return change + " · applied live"
	}
}

func toggleLabel(key settingKey) string {
	switch key {
	case skStrict:
		return "strict edits"
	case skShowWriteDiff:
		return "show write diff"
	case skTopology:
		return "topology"
	case skTopoResyncOnAttach:
		return "resync on attach"
	case skTopoWatch:
		return "watch files"
	case skQuality:
		return "quality analysis"
	case skRefuseHomeRoots:
		return "refuse home roots"
	case skAutoAttachPersist:
		return "auto_attach_persist"
	case skAllowDependencyReads:
		return "allow_dependency_reads"
	case skGitWrites:
		return "git allow writes"
	case skGitDestructive:
		return "git allow destructive"
	case skGitPush:
		return "git allow push"
	case skAutoAttach:
		return "workspace auto-attach"
	default:
		return ""
	}
}
