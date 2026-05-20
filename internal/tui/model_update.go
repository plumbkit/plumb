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
	case tea.WindowSizeMsg:
		m = m.handleWindowSizeMsg(msg)
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
	case tea.KeyPressMsg:
		return m.handleKeyMsg(msg)
	}
	return m, nil
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
	if m.onDivider(mouse.X) {
		m.draggingDivider = true
		m.setLeftWidthFromMouse(mouse.X)
		return
	}
	m.handleBodyAreaClick(mouse.X, mouse.Y)
}

func (m *Model) handleBodyAreaClick(x, y int) {
	if m.currentSection == 2 && m.onLeftPanel(x, y) {
		m.selectMemoryAtBodyRow(y - bodyStartRow)
		return
	}
	if m.currentSection != 2 && m.onSessionsPanel(x, y) {
		m.selectSessionAtBodyRow(y - bodyStartRow)
		return
	}
	if y == bodyStartRow && x > m.leftWidth+1 {
		if m.currentSection == 2 {
			m.focusPanel = focusDetails
		} else {
			m.handleTabBarClick(x)
		}
		return
	}
	if y > bodyStartRow && x > m.leftWidth+1 {
		m.handleRightPanelClick(y - bodyStartRow + m.rightScroll)
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
	if m.showPopup {
		return m.handlePopupKey(msg)
	}
	if updated, cmd, handled := m.handleDashboardKey(msg); handled {
		return updated, cmd
	}
	if m.currentSection == 3 && !m.sectionMenuOpen && !m.showHelp {
		return m.handleLogSectionKey(msg)
	}
	return m.handleMainKey(msg)
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
	return x == m.leftWidth+1
}

func (m Model) onSessionsPanel(x, y int) bool {
	if y < bodyStartRow || x <= 0 || x > m.leftWidth {
		return false
	}
	return len(m.sessions) > 0
}

func (m Model) onLeftPanel(x, y int) bool {
	return y >= bodyStartRow && x > 0 && x <= m.leftWidth
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
		m.memoryCursor = 0
		m.memoryBodyCache = ""
		m.memoryBodyCacheName = ""
		m.focusPanel = focusSessions
		m.rightScroll = 0
	}
	if m.currentSection == 3 && !m.logInitd {
		m.logEntries, m.logOffset = initLogTail(m.logPath)
		m.logInitd = true
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
		pLW := m.popupLeftWidth
		pRW := max(m.width-pLW-3, 10)
		ovW := pLW + pRW + 3
		sx := (m.width - ovW) / 2
		if mouse.X <= sx+pLW+1 {
			m.popupLeftScroll += delta
		} else {
			m.popupDetailScroll += delta
		}
		return
	}
	if m.handleMouseWheelDash(delta) {
		return
	}
	if m.currentSection == 3 && !m.sectionMenuOpen && !m.showHelp {
		if m.logDetailOpen {
			m.logDetailScroll += delta
			if m.logDetailScroll < 0 {
				m.logDetailScroll = 0
			}
			return
		}
		m.moveLogSelection(delta)
		return
	}
	bodyHeight := max(m.height-6, 1)
	if mouse.Y < bodyStartRow || mouse.Y >= bodyStartRow+bodyHeight {
		return
	}
	if mouse.X <= m.leftWidth+1 {
		m.leftScroll += delta
		if m.leftScroll < 0 {
			m.leftScroll = 0
		}
		return
	}
	m.rightScroll += delta
	if m.rightScroll < 0 {
		m.rightScroll = 0
	}
}
