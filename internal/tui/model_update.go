package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type clearQuitMessageMsg struct{ id int }

func (m Model) Init() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	newM, cmd := m.updateInner(msg)
	newM.enforceScrollBounds()
	// Fill the memory-body cache off the render path: the render chain runs on a
	// throwaway value copy, so a disk read there would never persist and would
	// repeat every frame. newM is the model actually returned, so the cache set
	// here survives to the next render.
	newM.populateMemoryBody()
	return newM, cmd
}

func (m Model) updateInner(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pollMsg:
		return m.handlePollMsg()
	case clearQuitMessageMsg:
		return m.handleClearQuitMsg(msg), nil
	case logDetailCopyResetMsg:
		m.logDetailCopied = false
		return m, nil
	case settingsStatusMsg:
		m.settingsStatus = msg.text
		return m, nil
	case tea.WindowSizeMsg:
		m = m.handleWindowSizeMsg(msg)
	case tea.KeyPressMsg:
		return m.handleKeyMsg(msg)
	case tea.PasteMsg:
		return m.handlePasteMsg(msg), nil
	default:
		return m.handleMouseMsg(msg), nil
	}
	return m, nil
}

// handleMouseMsg routes the mouse message family; any other message falls
// through unchanged.
func (m Model) handleMouseMsg(msg tea.Msg) Model {
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		m.handleLeftMouseClick(msg.Mouse())
	case tea.MouseWheelMsg:
		mouse := msg.Mouse()
		switch mouse.Button {
		case tea.MouseWheelUp:
			m.handleMouseWheel(mouse, -3)
		case tea.MouseWheelDown:
			m.handleMouseWheel(mouse, 3)
		}
	case tea.MouseMotionMsg:
		mouse := msg.Mouse()
		if m.draggingDivider && mouse.Button == tea.MouseLeft {
			m.setLeftWidthFromMouse(mouse.X)
		}
	case tea.MouseReleaseMsg:
		m.draggingDivider = false
	}
	return m
}

// handlePasteMsg routes bracketed-paste text to whichever text input is
// active, in the same priority order as handleOverlayKey. Pasted text is
// flattened to a single line; with no input open the paste is dropped rather
// than spilling into keybindings.
func (m Model) handlePasteMsg(msg tea.PasteMsg) Model {
	text := sanitisePaste(msg.Content)
	if text == "" {
		return m
	}
	switch {
	case m.renameModal != nil:
		m.renameModal.paste(text)
	case m.settingsListEditor != nil:
		m.settingsListEditor.paste(text)
	case m.settingsTextEditor != nil:
		m.settingsTextEditor.paste(text)
	case m.showPopup || m.showThemePicker || m.showHelp || m.sectionMenuOpen:
		// no text input in these overlays
	case m.currentSection == 2 && m.memoryFilterActive:
		m.memoryFilter += text
		m.resetMemoryFilterView()
	case m.currentSection == 3 && !m.logDetailOpen:
		m.logFilter += text
		m.logScroll = 0
		m.logCursor = 0
	}
	return m
}

func (m Model) handlePollMsg() (Model, tea.Cmd) {
	m.refresh()
	if m.showPopup {
		m.refreshPopupCalls()
	}
	if m.currentSection == 3 && m.logInitd {
		newEntries, newOffset := readNewLogLines(m.logPath, m.logOffset)
		if len(newEntries) > 0 {
			m.logOffset = newOffset
			m.logEntries = append(m.logEntries, newEntries...)
			if len(m.logEntries) > maxLogEntries {
				m.logEntries = m.logEntries[len(m.logEntries)-maxLogEntries:]
			}
		}
	}
	return m, tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
}

func (m Model) handleClearQuitMsg(msg clearQuitMessageMsg) Model {
	if m.waitingForQuit && m.quitMessageID == msg.id {
		m.waitingForQuit = false
	}
	return m
}

func (m Model) handleWindowSizeMsg(msg tea.WindowSizeMsg) Model {
	m.width = msg.Width
	m.height = msg.Height
	if maxLeft := m.maxLeftWidth(); m.leftWidth > maxLeft {
		m.leftWidth = maxLeft
	}
	m.ready = true
	if newW := max(m.width-8, activityBuckets); newW != m.dashChartWidth {
		m.dashChartWidth = newW
		m.refreshDashboard()
	}
	return m
}

func (m *Model) handleLeftMouseClick(mouse tea.Mouse) {
	if mouse.Button != tea.MouseLeft {
		return
	}
	if m.logDetailOpen {
		return
	}
	if m.sectionMenuOpen {
		if mouse.X >= 0 && mouse.X < sectionMenuWidth {
			m.selectSectionMenuAtRow(mouse.Y)
		} else {
			m.sectionMenuOpen = false
		}
		return
	}
	if m.onSectionSelector(mouse.X, mouse.Y) {
		m.sectionMenuOpen = true
		m.sectionMenuCursor = m.currentSection
		return
	}
	if m.currentSection == 3 && !m.showHelp {
		m.selectLogAtBodyRow(mouse.Y - bodyStartRow)
		return
	}
	if m.currentSection == 4 {
		m.handleSettingsClick(mouse)
		return
	}
	if m.onDivider(mouse.X) {
		m.draggingDivider = true
		m.setLeftWidthFromMouse(mouse.X)
		return
	}
	m.handleBodyAreaClick(mouse.X, mouse.Y)
}

// handleSettingsClick routes a click in the Settings section: a click in the
// left Scope column (X within [1, scopeW], past the left border) selects a
// scope; anywhere to its right selects a settings row. No-op while an overlay
// or editor popup is open.
func (m *Model) handleSettingsClick(mouse tea.Mouse) {
	if m.showHelp || m.showThemePicker || m.settingsListEditor != nil || m.settingsTextEditor != nil {
		return
	}
	if mouse.X >= 1 && mouse.X <= m.settingsScopeWidth() {
		m.selectSettingsScopeAtBodyRow(mouse.Y)
		return
	}
	m.settingsScopeFocus = false
	m.selectSettingAtBodyRow(mouse.Y)
}

func (m *Model) handleBodyAreaClick(x, y int) {
	if m.currentSection == 2 {
		m.handleMemoryClick(x, y)
		return
	}
	if m.onSessionsPanel(x, y) {
		m.selectSessionAtBodyRow(y - bodyStartRow)
		return
	}
	if y == bodyStartRow && x > m.leftWidth+1 {
		m.handleTabBarClick(x)
		return
	}
	if y > bodyStartRow && x > m.leftWidth+1 {
		m.handleRightPanelClick(y - bodyStartRow + m.rightScroll)
	}
}

// handleMemoryClick routes a body-area click in the three-pane Memory section to
// the column under the cursor: Workspaces, Memories, or Detail.
func (m *Model) handleMemoryClick(x, y int) {
	if y < bodyStartRow {
		return
	}
	wsW, memW, _ := m.memoryColumnWidths()
	row := y - bodyStartRow
	switch {
	case x >= 1 && x <= wsW:
		m.selectWorkspaceAtBodyRow(row)
	case x >= wsW+2 && x <= wsW+1+memW:
		m.selectMemoryAtBodyRow(row)
	case x >= wsW+3+memW:
		m.focusPanel = focusDetails
	}
}

func (m *Model) handleTabBarClick(x int) {
	relX := x - m.leftWidth - 3
	if relX < 0 {
		return
	}
	if relX < 13 {
		m.rightTab = 0
		m.focusPanel = focusDetails
	} else if relX < 23 {
		m.rightTab = 1
		m.focusPanel = focusToolStats
	} else if relX < 35 {
		m.rightTab = 2
		m.focusPanel = focusStats
	} else if relX < 51 {
		m.rightTab = 3
		m.focusPanel = focusDiagnostics
	}
}

func (m Model) handleKeyMsg(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	key := msg.String()
	if m.waitingForQuit && key != "ctrl+c" && key != "ctrl+q" {
		m.waitingForQuit = false
	}
	if updated, cmd, handled := m.handleOverlayKey(msg); handled {
		return updated, cmd
	}
	if key == "ctrl+t" {
		return m.maybeOpenThemePicker()
	}
	if updated, cmd, handled := m.handleDashboardKey(msg); handled {
		return updated, cmd
	}
	if m.currentSection == 3 && !m.sectionMenuOpen && !m.showHelp {
		return m.handleLogSectionKey(msg)
	}
	if m.currentSection == 4 && !m.sectionMenuOpen && !m.showHelp {
		return m.handleSettingsSectionKey(msg)
	}
	return m.handleMainKey(msg)
}

// handleOverlayKey routes a key to an open overlay (rename modal, list editor,
// popup, theme picker), in priority order. handled is false when none is open.
func (m Model) handleOverlayKey(msg tea.KeyPressMsg) (Model, tea.Cmd, bool) {
	switch {
	case m.renameModal != nil:
		return m.handleRenameModalKey(msg), nil, true
	case m.settingsListEditor != nil:
		u, c := m.handleListEditorKey(msg)
		return u, c, true
	case m.settingsTextEditor != nil:
		u, c := m.handleTextEditorKey(msg)
		return u, c, true
	case m.showPopup:
		u, c := m.handlePopupKey(msg)
		return u, c, true
	case m.showThemePicker:
		u, c := m.handleThemePickerKey(msg)
		return u, c, true
	default:
		return m, nil, false
	}
}

func (m Model) handleRenameModalKey(msg tea.KeyPressMsg) Model {
	modal, confirmed := m.renameModal.Update(msg)
	m.renameModal = &modal
	if confirmed && m.renameSessionFn != nil && m.cursor < len(m.sessions) {
		newName, err := m.renameSessionFn(modal.input)
		if err == nil {
			m.sessions[m.cursor].Name = newName
			m.refreshStats()
		}
		m.renameModal = nil
	} else if msg.String() == "esc" {
		m.renameModal = nil
	}
	return m
}

func (m *Model) enforceScrollBounds() {
	if m.scrollBounds == nil {
		return
	}
	if m.dashScroll > m.scrollBounds.maxDash {
		m.dashScroll = m.scrollBounds.maxDash
	}
	if m.dashScroll < 0 {
		m.dashScroll = 0
	}
	if m.leftScroll > m.scrollBounds.maxLeft {
		m.leftScroll = m.scrollBounds.maxLeft
	}
	if m.leftScroll < 0 {
		m.leftScroll = 0
	}
	if m.rightScroll > m.scrollBounds.maxRight {
		m.rightScroll = m.scrollBounds.maxRight
	}
	if m.rightScroll < 0 {
		m.rightScroll = 0
	}
	if m.popupLeftScroll > m.scrollBounds.maxPopupLeft {
		m.popupLeftScroll = m.scrollBounds.maxPopupLeft
	}
	if m.popupLeftScroll < 0 {
		m.popupLeftScroll = 0
	}
	if m.popupDetailScroll > m.scrollBounds.maxPopupDetail {
		m.popupDetailScroll = m.scrollBounds.maxPopupDetail
	}
	if m.popupDetailScroll < 0 {
		m.popupDetailScroll = 0
	}
	if m.logDetailScroll > m.scrollBounds.maxLogDetail {
		m.logDetailScroll = m.scrollBounds.maxLogDetail
	}
	if m.logDetailScroll < 0 {
		m.logDetailScroll = 0
	}
}

func (m Model) logBodyHeight() int {
	return max(m.height-7, 1)
}

func (m Model) maxLeftWidth() int {
	maxLeft := m.width - 23
	if maxLeft < minLeftWidth {
		return minLeftWidth
	}
	return maxLeft
}

func (m Model) onDivider(x int) bool {
	// The Memory section is a three-pane layout with its own divider columns;
	// the single-divider drag does not apply there.
	return m.currentSection != 2 && x == m.leftWidth+1
}

func (m Model) onSessionsPanel(x, y int) bool {
	if y < bodyStartRow || x <= 0 || x > m.leftWidth {
		return false
	}
	return len(m.sessions) > 0
}

func (m *Model) setLeftWidthFromMouse(x int) {
	next := max(x-1, minLeftWidth)
	if maxLeft := m.maxLeftWidth(); next > maxLeft {
		next = maxLeft
	}
	m.leftWidth = next
}

func (m *Model) selectSessionAtBodyRow(row int) {
	if row < 1 {
		return
	}
	idx := (row - 1) / 3
	if idx < 0 || idx >= len(m.sessions) {
		return
	}
	m.cursor = idx
	m.focusPanel = focusSessions
	m.refreshStats()
}

func (m Model) onSectionSelector(x, y int) bool {
	return y >= 0 && y < 3 && x >= 0 && x < sectionMenuWidth
}

func (m *Model) selectSectionMenuAtRow(y int) {
	if y <= 0 || y > len(sectionMenuItems) {
		return
	}
	m.selectSection(y - 1)
}

func (m *Model) selectSectionShortcut(key string) {
	if (strings.HasPrefix(key, "ctrl+") || strings.HasPrefix(key, "alt+")) && len(key) >= 5 {
		idx := int(key[len(key)-1] - '1')
		if idx >= 0 && idx < len(sectionMenuItems) {
			m.selectSection(idx)
		}
	}
}

func (m *Model) selectSection(idx int) {
	if idx < 0 || idx >= len(sectionMenuItems) {
		return
	}
	prev := m.currentSection
	m.currentSection = idx
	m.sectionMenuCursor = idx
	m.sectionMenuOpen = false
	if m.currentSection == 2 && prev != 2 {
		m.memoryFilter = ""
		m.memoryFilterActive = false
		m.resetMemoryFilterView()
		m.focusPanel = focusSessions
	}
	if m.currentSection == 3 && !m.logInitd {
		m.logEntries, m.logOffset = initLogTail(m.logPath)
		m.logInitd = true
	}
	if m.currentSection == 4 && prev != 4 {
		m.enterSettings()
	}
}

func (m *Model) handleMouseWheelDash(delta int) bool {
	if m.currentSection != 0 || m.sectionMenuOpen || m.showHelp {
		return false
	}
	m.dashScroll += delta
	if m.dashScroll < 0 {
		m.dashScroll = 0
	}
	return true
}

func (m *Model) handleMouseWheel(mouse tea.Mouse, delta int) {
	if m.showPopup {
		m.handleMouseWheelPopup(mouse, delta)
		return
	}
	if m.handleMouseWheelDash(delta) {
		return
	}
	if m.currentSection == 3 && !m.sectionMenuOpen && !m.showHelp {
		m.handleMouseWheelLogs(delta)
		return
	}
	if m.currentSection == 4 && !m.sectionMenuOpen && !m.showHelp && !m.showThemePicker {
		m.scrollSettings(delta)
		return
	}
	m.handleMouseWheelBody(mouse, delta)
}

func (m *Model) handleMouseWheelPopup(mouse tea.Mouse, delta int) {
	pLW := m.popupLeftWidth
	pRW := max(m.width-pLW-3, 10)
	ovW := pLW + pRW + 3
	sx := (m.width - ovW) / 2
	if mouse.X <= sx+pLW+1 {
		m.popupLeftScroll += delta
	} else {
		m.popupDetailScroll += delta
	}
}

func (m *Model) handleMouseWheelLogs(delta int) {
	if m.logDetailOpen {
		m.logDetailScroll += delta
		if m.logDetailScroll < 0 {
			m.logDetailScroll = 0
		}
		return
	}
	m.moveLogSelection(delta)
}

func (m *Model) handleMouseWheelBody(mouse tea.Mouse, delta int) {
	bodyHeight := max(m.height-6, 1)
	if mouse.Y < bodyStartRow || mouse.Y >= bodyStartRow+bodyHeight {
		return
	}
	if m.currentSection == 2 {
		m.handleMemoryWheel(mouse.X, delta)
		return
	}
	if mouse.X <= m.leftWidth+1 {
		m.leftScroll = max(m.leftScroll+delta, 0)
		return
	}
	m.rightScroll = max(m.rightScroll+delta, 0)
}

// handleMemoryWheel scrolls the column under the cursor in the three-pane Memory
// section: Workspaces, Memories (m.leftScroll), or Detail (m.rightScroll).
func (m *Model) handleMemoryWheel(x, delta int) {
	wsW, memW, _ := m.memoryColumnWidths()
	switch {
	case x <= wsW+1:
		m.workspaceScroll = max(m.workspaceScroll+delta, 0)
	case x <= wsW+2+memW:
		m.leftScroll = max(m.leftScroll+delta, 0)
	default:
		m.rightScroll = max(m.rightScroll+delta, 0)
	}
}
