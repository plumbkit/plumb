package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

func (m Model) handlePopupKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		if m.waitingForQuit {
			return m, tea.Quit
		}
		m.waitingForQuit = true
		m.quitMessageID++
		id := m.quitMessageID
		return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return clearQuitMessageMsg{id: id} })
	case "esc":
		m.showPopup = false
		m.popupCalls = nil
		m.popupDetailScroll = 0
		m.popupLeftScroll = 0
		m.popupRightFocus = false
	case "tab", "shift+tab":
		m.popupRightFocus = !m.popupRightFocus
		m.popupDetailScroll = 0
	case "up", "k":
		if m.popupRightFocus {
			if m.popupDetailScroll > 0 {
				m.popupDetailScroll--
			}
		} else if m.popupCallCursor > 0 {
			m.popupCallCursor--
			m.popupDetailScroll = 0
			m.popupDetail = popupDetailCache{}
			m.ensurePopupCursorVisible()
		}
	case "down", "j":
		if m.popupRightFocus {
			m.popupDetailScroll++
		} else if m.popupCallCursor < len(m.popupCalls)-1 {
			m.popupCallCursor++
			m.popupDetailScroll = 0
			m.popupDetail = popupDetailCache{}
			m.ensurePopupCursorVisible()
		}
	case "c":
		if len(m.popupCalls) > 0 {
			inputJSON, outputText := m.currentDetail()
			return m, copyToClipboard(inputJSON, outputText)
		}
	case "[":
		m.popupLeftWidth -= 2
		if m.popupLeftWidth < minPopupLeftWidth {
			m.popupLeftWidth = minPopupLeftWidth
		}
	case "]":
		m.popupLeftWidth += 2
		if maxPLeft := m.width / 2; m.popupLeftWidth > maxPLeft {
			m.popupLeftWidth = maxPLeft
		}
	case "pgdown":
		pageSize := max(m.height-7, 1)
		if m.popupRightFocus {
			m.popupDetailScroll += pageSize
		} else {
			m.popupCallCursor += pageSize
			if m.popupCallCursor >= len(m.popupCalls) {
				m.popupCallCursor = len(m.popupCalls) - 1
			}
			m.popupDetailScroll = 0
			m.popupDetail = popupDetailCache{}
			m.ensurePopupCursorVisible()
		}
	case "pgup":
		pageSize := max(m.height-7, 1)
		if m.popupRightFocus {
			m.popupDetailScroll -= pageSize
			if m.popupDetailScroll < 0 {
				m.popupDetailScroll = 0
			}
		} else {
			m.popupCallCursor -= pageSize
			if m.popupCallCursor < 0 {
				m.popupCallCursor = 0
			}
			m.popupDetailScroll = 0
			m.popupDetail = popupDetailCache{}
			m.ensurePopupCursorVisible()
		}
	}
	return m, nil
}

func (m Model) handleLogSectionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.logDetailOpen {
		switch msg.String() {
		case "ctrl+q", "ctrl+c":
			if m.waitingForQuit {
				return m, tea.Quit
			}
			m.waitingForQuit = true
			m.quitMessageID++
			id := m.quitMessageID
			return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return clearQuitMessageMsg{id: id} })
		case "esc":
			m.logDetailOpen = false
			m.logDetailScroll = 0
		case "c":
			if text := m.currentLogDetailText(); text != "" {
				m.logDetailCopied = true
				return m, tea.Batch(
					copyTextToClipboard(text),
					tea.Tick(3*time.Second, func(time.Time) tea.Msg { return logDetailCopyResetMsg{} }),
				)
			}
		case "up", "k":
			if m.logDetailScroll > 0 {
				m.logDetailScroll--
			}
		case "down", "j":
			m.logDetailScroll++
		case "pgup":
			pageSize := max(m.height-10, 1)
			m.logDetailScroll -= pageSize
			if m.logDetailScroll < 0 {
				m.logDetailScroll = 0
			}
		case "pgdown":
			pageSize := max(m.height-10, 1)
			m.logDetailScroll += pageSize
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+q", "ctrl+c":
		if m.waitingForQuit {
			return m, tea.Quit
		}
		m.waitingForQuit = true
		m.quitMessageID++
		id := m.quitMessageID
		return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return clearQuitMessageMsg{id: id} })
	case "esc":
		if m.logFilter != "" {
			m.logFilter = ""
			m.logScroll = 0
		} else {
			m.sectionMenuOpen = true
			m.sectionMenuCursor = m.currentSection
		}
	case "/":
		if m.sectionMenuOpen {
			m.sectionMenuOpen = false
		} else {
			m.sectionMenuOpen = true
			m.sectionMenuCursor = m.currentSection
		}
	case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "alt+1", "alt+2", "alt+3", "alt+4", "alt+5":
		m.selectSectionShortcut(msg.String())
	case "ctrl+h":
		m.showHelp = true
	case "up", "k":
		m.moveLogSelection(-1)
	case "down", "j":
		m.moveLogSelection(1)
	case "pgup":
		m.moveLogSelection(-m.logBodyHeight())
	case "pgdown":
		m.moveLogSelection(m.logBodyHeight())
	case "G":
		m.logFollow = true
	case "enter":
		if len(m.filteredLogEntries()) > 0 {
			m.logDetailOpen = true
			m.logDetailScroll = 0
		}
	case "backspace":
		if r := []rune(m.logFilter); len(r) > 0 {
			m.logFilter = string(r[:len(r)-1])
			m.logScroll = 0
			m.logCursor = 0
		}
	default:
		s := msg.String()
		if len(s) == 1 && s[0] >= 32 && s[0] < 127 {
			m.logFilter += s
			m.logScroll = 0
			m.logCursor = 0
		}
	}
	return m, nil
}

func (m Model) handleMainKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "/":
		if m.sectionMenuOpen {
			m.sectionMenuOpen = false
		} else {
			m.sectionMenuOpen = true
			m.sectionMenuCursor = m.currentSection
		}
	case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "alt+1", "alt+2", "alt+3", "alt+4", "alt+5":
		m.selectSectionShortcut(msg.String())
	case "ctrl+h":
		m.sectionMenuOpen = false
		m.showHelp = true
	case "ctrl+q", "ctrl+c":
		if m.waitingForQuit {
			return m, tea.Quit
		}
		m.waitingForQuit = true
		m.quitMessageID++
		id := m.quitMessageID
		return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return clearQuitMessageMsg{id: id} })
	case "esc":
		m.sectionMenuOpen = false
		m.showHelp = false
	case "enter":
		if m.sectionMenuOpen {
			m.selectSection(m.sectionMenuCursor)
			break
		}
		switch m.focusPanel {
		case focusToolStats:
			if len(m.toolStats) > 0 {
				m.openPopup(m.toolStats[m.toolStatsCursor].Tool, time.Time{})
			}
		case focusStats:
			if len(m.recentCalls) > 0 {
				rc := m.recentCalls[m.statsCursor]
				m.openPopup(rc.Tool, rc.CalledAt)
			}
		}
	case "tab":
		if m.focusPanel == focusSessions {
			m.focusPanel = m.rightTabFocusPanel()
		} else if m.rightTab < 3 {
			m.rightTab++
			m.focusPanel = m.rightTabFocusPanel()
			m.rightScroll = 0
		} else {
			m.rightTab = 0
			m.focusPanel = focusSessions
		}
	case "shift+tab":
		if m.focusPanel == focusSessions {
			m.rightTab = 3
			m.focusPanel = m.rightTabFocusPanel()
		} else if m.rightTab > 0 {
			m.rightTab--
			m.focusPanel = m.rightTabFocusPanel()
			m.rightScroll = 0
		} else {
			m.focusPanel = focusSessions
		}
	case "up", "k":
		if m.sectionMenuOpen {
			if m.sectionMenuCursor > 0 {
				m.sectionMenuCursor--
			}
			break
		}
		switch m.focusPanel {
		case focusToolStats:
			if m.toolStatsCursor > 0 {
				m.toolStatsCursor--
			}
		case focusStats:
			if m.statsCursor > 0 {
				m.statsCursor--
			}
		case focusDetails, focusDiagnostics:
			if m.rightScroll > 0 {
				m.rightScroll--
			}
		default:
			if m.cursor > 0 {
				m.cursor--
				m.leftScroll = 0
				m.rightScroll = 0
				m.refreshStats()
			}
		}
	case "down", "j":
		if m.sectionMenuOpen {
			if m.sectionMenuCursor < len(sectionMenuItems)-1 {
				m.sectionMenuCursor++
			}
			break
		}
		switch m.focusPanel {
		case focusToolStats:
			if m.toolStatsCursor < len(m.toolStats)-1 {
				m.toolStatsCursor++
			}
		case focusStats:
			if m.statsCursor < len(m.recentCalls)-1 {
				m.statsCursor++
			}
		case focusDetails, focusDiagnostics:
			m.rightScroll++
		default:
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
				m.leftScroll = 0
				m.rightScroll = 0
				m.refreshStats()
			}
		}
	case "1", "2", "3", "4", "5":
		if m.sectionMenuOpen {
			m.selectSection(int(msg.String()[0] - '1'))
		}
	case "a":
		m.refresh()
	case "[":
		m.leftWidth -= 2
		if m.leftWidth < minLeftWidth {
			m.leftWidth = minLeftWidth
		}
	case "]":
		m.leftWidth += 2
		if maxLeft := m.maxLeftWidth(); m.leftWidth > maxLeft {
			m.leftWidth = maxLeft
		}
	case "pgdown":
		pageSize := max(m.height-6, 1)
		switch m.focusPanel {
		case focusToolStats:
			m.toolStatsCursor += pageSize
			if m.toolStatsCursor >= len(m.toolStats) {
				m.toolStatsCursor = len(m.toolStats) - 1
			}
		case focusStats:
			m.statsCursor += pageSize
			if m.statsCursor >= len(m.recentCalls) {
				m.statsCursor = len(m.recentCalls) - 1
			}
		case focusDetails, focusDiagnostics:
			m.rightScroll += pageSize
		default:
			m.cursor += pageSize
			if m.cursor >= len(m.sessions) {
				m.cursor = len(m.sessions) - 1
			}
			m.leftScroll = 0
			m.rightScroll = 0
			m.refreshStats()
		}
	case "pgup":
		pageSize := max(m.height-6, 1)
		switch m.focusPanel {
		case focusToolStats:
			m.toolStatsCursor -= pageSize
			if m.toolStatsCursor < 0 {
				m.toolStatsCursor = 0
			}
		case focusStats:
			m.statsCursor -= pageSize
			if m.statsCursor < 0 {
				m.statsCursor = 0
			}
		case focusDetails, focusDiagnostics:
			m.rightScroll -= pageSize
			if m.rightScroll < 0 {
				m.rightScroll = 0
			}
		default:
			m.cursor -= pageSize
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.refreshStats()
		}
	}
	return m, nil
}
