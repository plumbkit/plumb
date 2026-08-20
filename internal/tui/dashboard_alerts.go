package tui

import "fmt"

// maxAlertLines caps how many wrapped lines a single alert may occupy in the
// Alerts widget. Without a cap, a HealthMessage embedding a long
// client-supplied path (issue #358) can hard-wrap into dozens of lines and
// push every other dashboard widget below the fold, even though each line
// individually fits the box (that corruption is wrapText's job, see
// model_utils.go).
const maxAlertLines = 8

// capAlertLines caps lines at maxAlertLines, eliding the MIDDLE rather than
// the tail: it keeps the first lines, one "…" line, and the last two. A prior
// attempt at this same problem (issue #358) truncated the tail, which is
// exactly where a boundary-violation message's remedy sentence sits — that
// attempt's cap fell squarely on the remedy and its own test could not catch
// it because it compared against the constant under test rather than a real
// message. Keeping the last two lines here is what a widget-level test can
// assert against: the final sentence of a long message must still appear.
func capAlertLines(lines []string) []string {
	if len(lines) <= maxAlertLines {
		return lines
	}
	const tail = 2
	head := maxAlertLines - tail - 1 // 1 line reserved for the "…" marker
	out := make([]string, 0, maxAlertLines)
	out = append(out, lines[:head]...)
	out = append(out, "…")
	out = append(out, lines[len(lines)-tail:]...)
	return out
}

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
			for j, line := range capAlertLines(wrapText(msg, textWidth)) {
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

func (m Model) dashboardWorkspaceStateAlert() string {
	for _, s := range m.sessions {
		if s.Synthetic {
			return "Workspace auto-attached; run plumb init to make it explicit"
		}
	}
	for _, s := range m.sessions {
		if s.Health == "blocked" {
			// Render the session's own remedy (issue #358): Health == "blocked"
			// now covers several causes (a boundary violation, a refused sticky
			// re-pin issue #182, a refused wide claim issue #318) with different
			// remedies, and HealthMessage already carries the right one — the
			// writers in internal/cli (markBoundaryViolation callers) are
			// responsible for naming it. Fall back to the fixed string only when
			// nothing was recorded.
			if s.HealthMessage != "" {
				return s.HealthMessage
			}
			return "Workspace boundary violation blocked; start a new MCP connection"
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
