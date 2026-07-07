package tui

// model_settings_keys.go — Settings navigation, scroll, mouse-row resolution,
// and the key-press dispatch. Row activation + editors live in
// model_settings_edit.go; value adjustment + field mappers in
// model_settings_values.go; persistence + daemon reload + theme keys in
// model_settings_persist.go.

import (
	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/config"
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
	m.settingsTab = settingsTabGeneral
	m.refreshSettingsItems()
	m.settingsCursor = 0
	m.settingsScroll = 0
	m.settingsScopeFocus = false
	m.showThemePicker = false
	m.settingsListEditor = nil
	m.settingsTextEditor = nil
	m.resetCommandsFocus()
	m.syncThemeCursor()
}

// settingsScrollHeight is the visible height of the scrollable settings list
// (the body minus the pinned footer rows).
func (m Model) settingsScrollHeight() int {
	return max(m.height-6-settingsFooterRows, 1)
}

func (m Model) settingsPageSize() int {
	return max(m.settingsContentHeight()-1, 1)
}

// settingsContentHeight is the visible height of the scrollable rows below the
// pinned tab header (the scroll area minus the tab bar + blank line).
func (m Model) settingsContentHeight() int {
	return max(m.settingsScrollHeight()-settingsTabHeaderRows, 1)
}

// refreshSettingsItems rebuilds the rows for the current scope, keeping only the
// active tab's settings. It also refreshes the Commands-tab data (which lives
// outside the flat settingItem list) so a scope change or a saved edit is
// reflected without a separate call site.
func (m *Model) refreshSettingsItems() {
	m.settingsItems = filterSettingsByTab(m.buildScopeItems(), m.settingsTab)
	m.refreshCommands()
}

// settingsCycleFocus moves focus forward (dir +1) or backward (dir -1) through
// the three stops: the Scope column, the General tab, and the LSP tab. Switching
// tab rebuilds the rows and resets the row cursor/scroll.
func (m Model) settingsCycleFocus(dir int) Model {
	n := len(settingsTabNames) + 1 // stops: Scope + one per tab
	pos := 0                       // 0 = Scope; 1+tab = that tab
	if !m.settingsScopeFocus {
		pos = 1 + m.settingsTab
	}
	pos = (pos + dir + n) % n
	if pos == 0 {
		m.settingsScopeFocus = true
		return m
	}
	m.settingsScopeFocus = false
	if newTab := pos - 1; newTab != m.settingsTab {
		m.settingsTab = newTab
		m.settingsCursor = 0
		m.settingsScroll = 0
		m.resetCommandsFocus()
		m.refreshSettingsItems()
	}
	return m
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
	scrollH := m.settingsContentHeight()
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
	maxOff := max(len(settingsLogicalLines(m.settingsItems))-m.settingsContentHeight(), 0)
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
	if row < settingsTabHeaderRows || row >= m.settingsScrollHeight() {
		return
	}
	lines := settingsLogicalLines(m.settingsItems)
	idx := m.settingsScroll + row - settingsTabHeaderRows
	if idx < 0 || idx >= len(lines) {
		return
	}
	if lines[idx].kind == slRow {
		m.settingsCursor = lines[idx].item
	}
}

// selectSettingsScopeAtBodyRow focuses the Scope column and selects the scope
// clicked at screen row y. The scope list renders as [title, blank, scope0,
// scope1, …], so the first scope sits two rows below the body top.
func (m *Model) selectSettingsScopeAtBodyRow(y int) {
	row := y - bodyStartRow
	if row < 0 || row >= m.settingsScrollHeight() {
		return
	}
	idx := m.settingsScopeScroll + row - 2 // header + blank spacer precede scope rows
	if idx < 0 || idx >= len(m.settingsScopes) {
		return
	}
	m.settingsScopeFocus = true
	m.settingsScopeCursor = idx
	m.selectScope()
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
		// Esc steps focus back to the Scope column; a no-op when already there.
		// (The section menu opens with "/", not Esc.)
		m.settingsScopeFocus = true
	default:
		return m.handleSettingsNavKey(msg.String())
	}
	return m, nil
}

// handleSettingsNavKey handles the in-list navigation and edit keys, kept
// separate from the section/global keys to keep each handler simple. When the
// Scope column is focused it routes to handleScopeNavKey instead.
func (m Model) handleSettingsNavKey(key string) (Model, tea.Cmd) {
	switch key { // pane controls work regardless of which pane is focused
	case "[":
		return m.adjustScopeWidth(-1), nil
	case "]":
		return m.adjustScopeWidth(1), nil
	case "tab":
		return m.settingsCycleFocus(1), nil
	case "shift+tab":
		return m.settingsCycleFocus(-1), nil
	}
	if m.settingsScopeFocus {
		return m.handleScopeNavKey(key)
	}
	if m.settingsTab == settingsTabCommands {
		return m.handleCommandsKey(key)
	}
	return m.handleSettingsRowKey(key)
}

// handleSettingsRowKey handles cursor movement and value-editing keys within the
// focused rows pane (the Scope column and pane controls are handled upstream).
func (m Model) handleSettingsRowKey(key string) (Model, tea.Cmd) {
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
	case "backspace", "delete":
		// In a workspace scope, reset the focused row to inherit (no-op in Global).
		return m.afterSettingChange(m.resetToInherit(), nil)
	}
	return m, nil
}

// handleScopeNavKey drives the Scope column: up/down pick a scope (reloading the
// rows for it), right/enter return focus to the rows pane. tab/shift+tab are
// handled upstream in handleSettingsNavKey.
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
	case "right", "l", "enter", " ":
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
	m.resetCommandsFocus()
	m.settingsScopeScroll = clampOffset(m.settingsScopeScroll, len(m.settingsScopes)+2, m.settingsScrollHeight())
	m.refreshSettingsItems()
}
