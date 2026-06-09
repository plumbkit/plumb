package tui

// model_settings_persist.go — config persistence, the best-effort daemon
// control-socket reload pushes (config / project / log level), and the theme
// picker key handling.

import (
	"bufio"
	"fmt"
	"net"

	tea "charm.land/bubbletea/v2"

	"github.com/plumbkit/plumb/internal/config"
)

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

func (m Model) setLogLevel(lvl string) (Model, tea.Cmd) {
	if !m.persist(func(c *config.Config) { c.LogLevel = lvl }) {
		return m, nil
	}
	m.settingsCfg.LogLevel = lvl
	m.refreshSettingsItems()
	m.settingsStatus = "log level → " + lvl
	m.pendingReload = false // log level applies live via set-level, not reload-config
	return m, m.applyLogLevelLive(lvl)
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
	m.refreshSettingsItems()
}
