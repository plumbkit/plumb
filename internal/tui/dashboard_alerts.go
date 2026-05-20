package tui

import "fmt"

func (m Model) dashAlertsWidget(width int) []string {
	inner := width - 2
	alerts := m.dashboardAlerts()

	var content []string
	if len(alerts) == 0 {
		content = []string{"   " + OkStyle.Render("✓") + " " + MutedStyle.Render("No issues detected") + "   "}
	} else {
		for _, msg := range alerts {
			content = append(content, "   "+WarnStyle.Render("✗")+" "+WarnStyle.Render(msg)+"   ")
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
			return fmt.Sprintf("Daemon version mismatch: running %s, TUI %s; run plumb stop", s.DaemonVersion, Version)
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
