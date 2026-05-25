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
			m.ensureSettingsCursorVisible()
		}
	case "down", "j":
		if m.settingsCursor < len(m.settingsItems)-1 {
			m.settingsCursor++
			m.ensureSettingsCursorVisible()
		}
	case "pgup":
		m.settingsCursor = max(m.settingsCursor-m.settingsPageSize(), 0)
		m.ensureSettingsCursorVisible()
	case "pgdown":
		m.settingsCursor = min(m.settingsCursor+m.settingsPageSize(), len(m.settingsItems)-1)
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
		m.showThemePicker = true
		m.syncThemeCursor()
		return m
	case settingToggle:
		return m.toggleBool(it.key)
	default:
		return m
	}
}

// adjustSetting changes the focused row's value by dir (−1 / +1).
func (m Model) adjustSetting(dir int) (Model, tea.Cmd) {
	if m.settingsCursor < 0 || m.settingsCursor >= len(m.settingsItems) {
		return m, nil
	}
	switch it := m.settingsItems[m.settingsCursor]; it.key {
	case skLogLevel:
		return m.setLogLevel(cycleOption(logLevelOptions, m.settingsCfg.LogLevel, dir))
	case skLogFormat:
		return m.setLogFormat(cycleOption(logFormatOptions, m.settingsCfg.LogFormat, dir)), nil
	case skRateLimit:
		return m.setRateLimit(m.settingsCfg.Edits.RateLimitPerMinute + dir*10), nil
	case skCacheMaxSize:
		return m.setCacheMaxSize(m.settingsCfg.Cache.MaxSize + dir*100), nil
	case skCacheTTL, skLSPTimeout:
		return m.setDuration(it.key, dir), nil
	case skPathStyle:
		return m.setPathStyle(cycleOption(pathStyleOptions, m.settingsCfg.UI.PathStyle, dir)), nil
	case skStrict, skShowWriteDiff, skTopology, skQuality,
		skGitWrites, skGitDestructive, skGitPush, skAutoAttach:
		return m.toggleBool(it.key), nil
	default:
		return m, nil
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
		m.settingsStatus = restartStatus("log format → " + format)
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
		m.settingsStatus = restartStatus("rate limit → " + rateLimitValue(n))
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
		m.settingsStatus = restartStatus(toggleLabel(key) + " " + onOff(v))
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
	case skQuality:
		return &c.Quality.Enabled
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
		m.settingsStatus = restartStatus(fmt.Sprintf("cache max size → %d", n))
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
		m.settingsStatus = restartStatus(durLabel(key) + " → " + next)
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
		m.settingsStatus = "path style → " + style
	}
	return m
}

func restartStatus(change string) string {
	return change + " · applies on next daemon start"
}

func toggleLabel(key settingKey) string {
	switch key {
	case skStrict:
		return "strict edits"
	case skShowWriteDiff:
		return "show write diff"
	case skTopology:
		return "topology"
	case skQuality:
		return "quality analysis"
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
