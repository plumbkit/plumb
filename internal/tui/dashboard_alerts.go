package tui

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
)

func (m Model) dashAlertsWidget(width int) []string {
	inner := width - 2
	alerts := m.dashboardAlerts()

	const (
		lpad    = "   "    // 3-space left margin
		rpad    = "   "    // 3-space right margin
		contPad = "      " // 6-space continuation indent (lpad + mark + space + 1)
	)
	// Available text width: inner minus the widest prefix (contPad) and right margin.
	textWidth := inner - len(contPad) - len(rpad)

	var content []string
	if len(alerts) == 0 {
		content = []string{lpad + OkStyle.Render("✓") + " " + MutedStyle.Render("No issues detected") + rpad}
	} else {
		for i, msg := range alerts {
			if i > 0 {
				content = append(content, "") // blank line between alerts
			}
			for j, line := range wrapText(msg, textWidth) {
				if j == 0 {
					content = append(content, lpad+WarnStyle.Render("✗")+" "+WarnStyle.Render(line)+rpad)
				} else {
					content = append(content, contPad+WarnStyle.Render(line)+rpad)
				}
			}
		}
	}
	return dashBox(" Alerts ", inner, content)
}

func (m Model) dashboardAlerts() []string {
	var alerts []string
	if !daemonRunning() {
		alerts = append(alerts, "Daemon is not running; start it with: plumb daemon")
	} else if !m.daemonMetricsOK {
		alerts = append(alerts, "Daemon metrics unavailable; snapshot missing or stale")
	}
	if m.loadErr != "" {
		alerts = append(alerts, "Session load error: "+m.loadErr)
	}
	if m.statsErr != "" {
		alerts = append(alerts, "Stats database unavailable: "+m.statsErr)
	}
	if m.dashProjectFolder == "" {
		alerts = append(alerts, "No workspace resolved; run plumb init in this project")
	}
	if msg := m.dashboardDaemonVersionAlert(); msg != "" {
		alerts = append(alerts, msg)
	}
	if msg := m.dashboardWorkspaceStateAlert(); msg != "" {
		alerts = append(alerts, msg)
	}
	if msg := m.dashboardErrorSpikeAlert(); msg != "" {
		alerts = append(alerts, msg)
	}
	return alerts
}

func (m Model) dashboardDaemonVersionAlert() string {
	if Version == "" || Version == "dev" {
		return ""
	}
	for _, s := range m.sessions {
		if s.DaemonVersion != "" && s.DaemonVersion != Version {
			return fmt.Sprintf("Daemon version mismatch: running %s, TUI %s; run plumb restart", s.DaemonVersion, Version)
		}
	}
	return ""
}

// blockedAlert renders the one-line alert for a session marked blocked.
//
// It prefers the session's own HealthMessage, because "blocked" now covers
// causes with different remedies: a boundary violation, a refused sticky re-pin
// (issue #182), and a refused wide workspace claim (issue #318). The old fixed
// string prescribed "start a new MCP connection" for all of them, which for the
// #318 case is advice that LOOPS — a fresh connection replays the same pin and
// is refused again. Every writer of the mark supplies a message naming its own
// remedy; the fixed string is kept only for a mark that somehow carries none.
func blockedAlert(healthMessage string) string {
	if msg := strings.TrimSpace(healthMessage); msg != "" {
		return textfmt.Ellipsis(msg, maxBlockedAlertRunes)
	}
	return "Workspace blocked; see the session detail for why"
}

// maxBlockedAlertRunes keeps a health message from overrunning the single-line
// alert row; the untruncated text is in the session detail pane.
const maxBlockedAlertRunes = 160

func (m Model) dashboardWorkspaceStateAlert() string {
	for _, s := range m.sessions {
		if s.Synthetic {
			return "Workspace auto-attached; run plumb init to make it explicit"
		}
	}
	for _, s := range m.sessions {
		if s.Health == "blocked" {
			return blockedAlert(s.HealthMessage)
		}
	}
	for _, s := range m.sessions {
		if m.dashProjectFolder != "" && s.Folder != m.dashProjectFolder {
			continue
		}
		if s.Language == "" || s.Language == "none" {
			return "LSP unavailable for this workspace; filesystem tools still work"
		}
	}
	return ""
}

func (m Model) dashboardErrorSpikeAlert() string {
	var calls, errors int64
	for _, t := range m.dashUptimeTopTools {
		calls += t.Calls
		errors += t.Errors
	}
	if calls < 10 || errors < 3 || errors*100 < calls*20 {
		return ""
	}
	return fmt.Sprintf("Recent tool error spike: %d/%d uptime calls failed", errors, calls)
}
