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
		m.settingsStatus = "theme → " + ActiveThemeName
	}
	m.settingsCfg = cfg
	m.settingsItems = buildSettingItems(cfg)
	m.settingsCursor = 0
	m.settingsScroll = 0
	m.showThemePicker = false
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
// separate from the section/global keys to keep each handler simple.
func (m Model) handleSettingsNavKey(key string) (Model, tea.Cmd) {
	switch key {
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
	}
	return m, nil
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
		return m.toggleBool(it.key)
	default:
		return m
	}
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
		return m.toggleBool(it.key), nil
	case settingNumber:
		return m.adjustNumber(it.key, dir)
	case settingCycle:
		return m.adjustCycle(it.key, dir)
	default:
		return m, nil
	}
}

// adjustNumber routes numeric rows: the two original rows keep their bespoke
// setters (preserving the rate-limit "off" wording and the cache step), every
// other int field flows through the generic setNumber.
func (m Model) adjustNumber(key settingKey, dir int) (Model, tea.Cmd) {
	switch key {
	case skRateLimit:
		return m.setRateLimit(m.settingsCfg.Edits.RateLimitPerMinute + dir*10), nil
	case skCacheMaxSize:
		return m.setCacheMaxSize(m.settingsCfg.Cache.MaxSize + dir*100), nil
	default:
		return m.setNumber(key, dir), nil
	}
}

// adjustCycle routes cycle rows: log level applies live, durations use the
// duration setter, the original string cycles keep their setters, and any new
// string cycle (e.g. quality mode) flows through the generic setCycle.
func (m Model) adjustCycle(key settingKey, dir int) (Model, tea.Cmd) {
	switch key {
	case skLogLevel:
		return m.setLogLevel(cycleOption(logLevelOptions, m.settingsCfg.LogLevel, dir))
	case skLogFormat:
		return m.setLogFormat(cycleOption(logFormatOptions, m.settingsCfg.LogFormat, dir)), nil
	case skPathStyle:
		return m.setPathStyle(cycleOption(pathStyleOptions, m.settingsCfg.UI.PathStyle, dir)), nil
	case skCacheTTL, skLSPTimeout:
		return m.setDuration(key, dir), nil
	default:
		return m.setCycle(key, dir), nil
	}
}

// setNumber adjusts a generic integer field by its per-field step and persists.
func (m Model) setNumber(key settingKey, dir int) Model {
	step, label := numberMeta(key)
	if key == skTopoMaxFileSize { // the only int64 field
		n := m.settingsCfg.Topology.MaxFileSizeBytes + int64(dir*step)
		if n < 0 {
			n = 0
		}
		if m.persist(func(c *config.Config) { c.Topology.MaxFileSizeBytes = n }) {
			m.settingsCfg.Topology.MaxFileSizeBytes = n
			m.settingsItems = buildSettingItems(m.settingsCfg)
			m.settingsStatus = settingStatus(key, fmt.Sprintf("%s → %d", label, n))
		}
		return m
	}
	ptr := intField(&m.settingsCfg, key)
	if ptr == nil {
		return m
	}
	n := *ptr + dir*step
	if n < 0 {
		n = 0
	}
	if m.persist(func(c *config.Config) { *intField(c, key) = n }) {
		*intField(&m.settingsCfg, key) = n
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(key, fmt.Sprintf("%s → %d", label, n))
	}
	return m
}

// setCycle cycles a generic string-enum field and persists.
func (m Model) setCycle(key settingKey, dir int) Model {
	cur, opts, set, label := cycleMeta(&m.settingsCfg, key)
	if set == nil {
		return m
	}
	next := cycleOption(opts, cur, dir)
	if m.persist(func(c *config.Config) { set(c, next) }) {
		set(&m.settingsCfg, next)
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(key, label+" → "+next)
	}
	return m
}

// numberMeta returns the adjust step and status label for a numeric setting.
func numberMeta(key settingKey) (int, string) {
	switch key {
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

// cycleMeta returns the current value, option set, setter, and label for a
// generic string-enum setting.
func cycleMeta(c *config.Config, key settingKey) (string, []string, func(*config.Config, string), string) {
	switch key {
	case skQualityMode:
		return qualityModeValue(c.Quality.Mode), qualityModeOptions,
			func(c *config.Config, v string) { c.Quality.Mode = v }, "quality mode"
	default:
		return "", nil, nil, ""
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

func (m Model) setRateLimit(n int) Model {
	if n < 0 {
		n = 0
	}
	if m.persist(func(c *config.Config) { c.Edits.RateLimitPerMinute = n }) {
		m.settingsCfg.Edits.RateLimitPerMinute = n
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(skRateLimit, "rate limit → "+rateLimitValue(n))
	}
	return m
}

// toggleBool flips one of the boolean settings, persists it, and refreshes the
// rows. boolField centralises the key→field mapping so this stays small.
func (m Model) toggleBool(key settingKey) Model {
	cur := boolField(&m.settingsCfg, key)
	if cur == nil {
		return m
	}
	v := !*cur
	if m.persist(func(c *config.Config) { *boolField(c, key) = v }) {
		*boolField(&m.settingsCfg, key) = v
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(key, toggleLabel(key)+" "+onOff(v))
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

func (m Model) setCacheMaxSize(n int) Model {
	if n < 0 {
		n = 0
	}
	if m.persist(func(c *config.Config) { c.Cache.MaxSize = n }) {
		m.settingsCfg.Cache.MaxSize = n
		m.settingsItems = buildSettingItems(m.settingsCfg)
		m.settingsStatus = settingStatus(skCacheMaxSize, fmt.Sprintf("cache max size → %d", n))
	}
	return m
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
	if !next.pendingReload {
		return next, cmd
	}
	next.pendingReload = false
	return next, tea.Batch(cmd, next.applyConfigReloadLive())
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
