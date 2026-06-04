package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

func (m Model) handlePopupKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		return m.mainKeyQuit()
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
		m = m.popupKeyUp()
	case "down", "j":
		m = m.popupKeyDown()
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
		m = m.popupKeyPageDown()
	case "pgup":
		m = m.popupKeyPageUp()
	}
	return m, nil
}

func (m Model) popupKeyUp() Model {
	if m.popupRightFocus {
		if m.popupDetailScroll > 0 {
			m.popupDetailScroll--
		}
		return m
	}
	if m.popupCallCursor > 0 {
		m.popupCallCursor--
		m.popupDetailScroll = 0
		m.popupDetail = popupDetailCache{}
		m.ensurePopupCursorVisible()
	}
	return m
}

func (m Model) popupKeyDown() Model {
	if m.popupRightFocus {
		m.popupDetailScroll++
		return m
	}
	if m.popupCallCursor < len(m.popupCalls)-1 {
		m.popupCallCursor++
		m.popupDetailScroll = 0
		m.popupDetail = popupDetailCache{}
		m.ensurePopupCursorVisible()
	}
	return m
}

func (m Model) popupKeyPageDown() Model {
	pageSize := max(m.height-7, 1)
	if m.popupRightFocus {
		m.popupDetailScroll += pageSize
		return m
	}
	m.popupCallCursor += pageSize
	if m.popupCallCursor >= len(m.popupCalls) {
		m.popupCallCursor = len(m.popupCalls) - 1
	}
	m.popupDetailScroll = 0
	m.popupDetail = popupDetailCache{}
	m.ensurePopupCursorVisible()
	return m
}

func (m Model) popupKeyPageUp() Model {
	pageSize := max(m.height-7, 1)
	if m.popupRightFocus {
		m.popupDetailScroll -= pageSize
		if m.popupDetailScroll < 0 {
			m.popupDetailScroll = 0
		}
		return m
	}
	m.popupCallCursor -= pageSize
	if m.popupCallCursor < 0 {
		m.popupCallCursor = 0
	}
	m.popupDetailScroll = 0
	m.popupDetail = popupDetailCache{}
	m.ensurePopupCursorVisible()
	return m
}

func (m Model) handleLogSectionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.logDetailOpen {
		return m.handleLogDetailKey(msg)
	}
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		return m.mainKeyQuit()
	case "esc":
		m = m.handleLogSectionEscKey()
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
		m = m.handleLogEnterKey()
	default:
		m = m.handleLogFilterInput(msg.String())
	}
	return m, nil
}

func (m Model) handleLogDetailKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		return m.mainKeyQuit()
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

func (m Model) handleLogSectionEscKey() Model {
	if m.logFilter != "" {
		m.logFilter = ""
		m.logScroll = 0
	} else {
		m.sectionMenuOpen = true
		m.sectionMenuCursor = m.currentSection
	}
	return m
}

func (m Model) handleLogEnterKey() Model {
	if len(m.filteredLogEntries()) > 0 {
		m.logDetailOpen = true
		m.logDetailScroll = 0
	}
	return m
}

func (m Model) handleLogFilterInput(s string) Model {
	if s == "backspace" {
		if r := []rune(m.logFilter); len(r) > 0 {
			m.logFilter = string(r[:len(r)-1])
			m.logScroll = 0
			m.logCursor = 0
		}
		return m
	}
	if len(s) == 1 && s[0] >= 32 && s[0] < 127 {
		m.logFilter += s
		m.logScroll = 0
		m.logCursor = 0
	}
	return m
}

func (m Model) handleMainKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		return m.mainKeyQuit()
	case "enter":
		m = m.mainKeyEnter()
	case "tab":
		m = m.mainKeyTab()
	case "shift+tab":
		m = m.mainKeyShiftTab()
	case "up", "k":
		m = m.mainKeyUp()
	case "down", "j":
		m = m.mainKeyDown()
	case "pgdown":
		m = m.mainKeyPageDown()
	case "pgup":
		m = m.mainKeyPageUp()
	default:
		m = m.handleMainKeySimple(msg.String())
	}
	return m, nil
}

func (m Model) mainKeyQuit() (Model, tea.Cmd) {
	if m.waitingForQuit {
		return m, tea.Quit
	}
	m.waitingForQuit = true
	m.quitMessageID++
	id := m.quitMessageID
	return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return clearQuitMessageMsg{id: id} })
}

func (m Model) mainKeyEnter() Model {
	if m.sectionMenuOpen {
		m.selectSection(m.sectionMenuCursor)
		return m
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
	return m
}

func (m Model) mainKeyTab() Model {
	if m.currentSection == 2 {
		switch m.focusPanel {
		case focusWorkspaces:
			m.focusPanel = focusSessions
		case focusSessions:
			m.focusPanel = focusDetails
		default:
			m.focusPanel = focusWorkspaces
		}
		m.rightScroll = 0
		return m
	}
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
	return m
}

func (m Model) mainKeyShiftTab() Model {
	if m.currentSection == 2 {
		switch m.focusPanel {
		case focusWorkspaces:
			m.focusPanel = focusDetails
		case focusDetails:
			m.focusPanel = focusSessions
		default:
			m.focusPanel = focusWorkspaces
		}
		m.rightScroll = 0
		return m
	}
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
	return m
}

func (m Model) mainKeyUp() Model {
	if m.sectionMenuOpen {
		if m.sectionMenuCursor > 0 {
			m.sectionMenuCursor--
		}
		return m
	}
	switch m.focusPanel {
	case focusWorkspaces:
		if m.workspaceCursor > 0 {
			m.selectWorkspace(m.workspaceCursor - 1)
			m.ensureWorkspaceCursorVisible()
		}
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
		if m.currentSection == 2 {
			if m.memoryCursor > 0 {
				m.memoryCursor--
				m.rightScroll = 0
				m.memoryBodyCache = ""
				m.memoryBodyCacheName = ""
				m.ensureLeftCursorVisible()
			}
		} else if m.cursor > 0 {
			m.cursor--
			m.rightScroll = 0
			m.refreshStats()
			m.ensureLeftCursorVisible()
		}
	}
	return m
}

func (m Model) mainKeyDown() Model {
	if m.sectionMenuOpen {
		if m.sectionMenuCursor < len(sectionMenuItems)-1 {
			m.sectionMenuCursor++
		}
		return m
	}
	switch m.focusPanel {
	case focusWorkspaces:
		if m.workspaceCursor < len(m.memoryWorkspaces)-1 {
			m.selectWorkspace(m.workspaceCursor + 1)
			m.ensureWorkspaceCursorVisible()
		}
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
		if m.currentSection == 2 {
			if m.memoryCursor < len(m.memories)-1 {
				m.memoryCursor++
				m.rightScroll = 0
				m.memoryBodyCache = ""
				m.memoryBodyCacheName = ""
				m.ensureLeftCursorVisible()
			}
		} else if m.cursor < len(m.sessions)-1 {
			m.cursor++
			m.rightScroll = 0
			m.refreshStats()
			m.ensureLeftCursorVisible()
		}
	}
	return m
}

func (m Model) mainKeyPageDown() Model {
	pageSize := max(m.height-6, 1)
	switch m.focusPanel {
	case focusWorkspaces:
		m.selectWorkspace(min(m.workspaceCursor+pageSize, max(len(m.memoryWorkspaces)-1, 0)))
		m.ensureWorkspaceCursorVisible()
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
		if m.currentSection == 2 {
			m.memoryCursor += pageSize
			if m.memoryCursor >= len(m.memories) {
				m.memoryCursor = max(len(m.memories)-1, 0)
			}
			m.rightScroll = 0
			m.memoryBodyCache = ""
			m.memoryBodyCacheName = ""
			m.ensureLeftCursorVisible()
		} else {
			m.cursor += pageSize
			if m.cursor >= len(m.sessions) {
				m.cursor = len(m.sessions) - 1
			}
			m.rightScroll = 0
			m.refreshStats()
			m.ensureLeftCursorVisible()
		}
	}
	return m
}

func (m Model) mainKeyPageUp() Model {
	pageSize := max(m.height-6, 1)
	switch m.focusPanel {
	case focusWorkspaces:
		m.selectWorkspace(max(m.workspaceCursor-pageSize, 0))
		m.ensureWorkspaceCursorVisible()
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
		if m.currentSection == 2 {
			m.memoryCursor -= pageSize
			if m.memoryCursor < 0 {
				m.memoryCursor = 0
			}
			m.rightScroll = 0
			m.memoryBodyCache = ""
			m.memoryBodyCacheName = ""
			m.ensureLeftCursorVisible()
		} else {
			m.cursor -= pageSize
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.refreshStats()
			m.ensureLeftCursorVisible()
		}
	}
	return m
}

func (m Model) handleMainKeySimple(key string) Model {
	switch key {
	case "/":
		if m.sectionMenuOpen {
			m.sectionMenuOpen = false
		} else {
			m.sectionMenuOpen = true
			m.sectionMenuCursor = m.currentSection
		}
	case "ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "alt+1", "alt+2", "alt+3", "alt+4", "alt+5":
		m.selectSectionShortcut(key)
	case "ctrl+h":
		m.sectionMenuOpen = false
		m.showHelp = true
	case "esc":
		m.sectionMenuOpen = false
		m.showHelp = false
	case "1", "2", "3", "4", "5":
		if m.sectionMenuOpen {
			m.selectSection(int(key[0] - '1'))
		}
	case "r":
		m = m.handleRenameSessionKey()
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
	}
	return m
}

func (m Model) handleRenameSessionKey() Model {
	// Only allow rename when sessions are focused and we have sessions to rename
	if m.focusPanel == focusSessions && m.currentSection != 2 && len(m.sessions) > 0 {
		currentSession := m.sessions[m.cursor]
		modal := newRenameSessionModal(currentSession.Name)
		m.renameModal = &modal
	}
	return m
}
