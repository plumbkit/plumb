package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

func (m Model) handleLogSectionKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.logDetailOpen {
		return m.handleLogDetailKey(msg)
	}
	switch m.keys.normalise(msg.String()) {
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
	switch m.keys.normalise(msg.String()) {
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
