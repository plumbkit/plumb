package tui

import (
	"fmt"
	"strings"

	"github.com/golimpio/plumb/internal/render"
	"github.com/golimpio/plumb/internal/session"
)

// sessionAdapterRow renders the active-LSP detail row: every language server
// the session is driving, joined (a multi-language root runs several, e.g.
// gopls + vscode-html-language-server), falling back to the legacy single
// Adapter, and pluralised to "Adapters" when more than one is live.
func sessionAdapterRow(s session.Info) (label, value string) {
	value = strings.Join(s.Adapters, ", ")
	if value == "" {
		value = s.Adapter
	}
	if value == "" {
		value = "—"
	}
	label = "Adapter"
	if len(s.Adapters) > 1 {
		label = "Adapters"
	}
	return label, value
}

func (m *Model) handleRightPanelClick(bodyRow int) {
	switch m.rightTab {
	case 1: // Tools tab
		if m.statsTableBodyRow >= 0 && len(m.toolStats) > 0 {
			idx := bodyRow - m.statsTableBodyRow
			if idx >= 0 && idx < len(m.toolStats) {
				m.toolStatsCursor, m.focusPanel = idx, focusToolStats
			}
		}
	case 2: // History tab
		if m.recentTableBodyRow >= 0 && len(m.recentCalls) > 0 {
			idx := bodyRow - m.recentTableBodyRow
			if idx >= 0 && idx < len(m.recentCalls) {
				m.statsCursor, m.focusPanel = idx, focusStats
			}
		}
	}
}

// rightTabFocusPanel returns the panelFocus that corresponds to the current rightTab.
func (m Model) rightTabFocusPanel() panelFocus {
	switch m.rightTab {
	case 1:
		return focusToolStats
	case 2:
		return focusStats
	case 3:
		return focusDiagnostics
	default:
		return focusDetails
	}
}

// rightTabBar renders the pill-style tab header row for the right panel.
// Active pill uses accent colour; inactive pills use muted text.
func (m Model) rightTabBar(_ int) string {
	rightFocused := m.focusPanel != focusSessions

	tabs := []string{"Details", "Tools", "History", "Diagnostics"}
	var sb strings.Builder
	sb.WriteString(" ")
	for i, name := range tabs {
		if i > 0 {
			sb.WriteString("  ")
		}
		if i == m.rightTab && rightFocused {
			sb.WriteString(RightTabActiveBracket.Render("[") + RightTabActiveLabel.Render(" "+name+" ") + RightTabActiveBracket.Render("]"))
		} else {
			sb.WriteString(RightTabMuted.Render("[") + RightTabInactive.Render(" "+name+" ") + RightTabMuted.Render("]"))
		}
	}
	return sb.String()
}

func (m *Model) rightLines(rw int) []string {
	if m.currentSection == 2 {
		return m.memoryRightLines(rw)
	}
	lines := []string{m.rightTabBar(rw), ""}
	if len(m.sessions) == 0 {
		lines = append(lines, "  "+MutedStyle.Render("Select a session to view details."))
		return lines
	}
	switch m.rightTab {
	case 1:
		lines = append(lines, m.rightLinesTools(rw)...)
	case 2:
		lines = append(lines, m.rightLinesHistory(rw)...)
	case 3:
		lines = append(lines, m.rightLinesDiagnostics(rw)...)
	default:
		lines = append(lines, m.rightLinesDetails(rw)...)
	}
	return lines
}

func (m Model) rightLinesDetails(rw int) []string {
	const kw = 14
	mv := max(rw-kw, 8)
	s := m.sessions[m.cursor]
	fld := s.Folder
	if fld == "" {
		fld = MutedStyle.Render("(resolving workspace…)")
	} else {
		fld = contractPath(fld, mv, m.settingsCfg.UI.PathStyle)
	}
	adapterLabel, adp := sessionAdapterRow(s)
	nm := s.Name
	if nm == "" {
		nm = MutedStyle.Render("—")
	}
	lang := sessionLangLabel(s)
	if lang == "" {
		lang = MutedStyle.Render("—")
	}
	lines := []string{
		detailRow("Name", nm),
		detailRow("ID", s.ID),
		detailRow("Language", lang),
		detailRow("Folder", fld),
		detailRow(adapterLabel, adp),
		detailRow("PID", fmt.Sprintf("%d", s.PID)),
	}
	if s.DaemonVersion != "" {
		lines = append(lines, detailRow("Daemon", s.DaemonVersion))
	}
	lines = append(lines, detailRow("Started", s.StartedAt.Format("2006-01-02 15:04:05")))
	cl := s.ClientName
	if s.ClientVersion != "" {
		cl += " " + s.ClientVersion
	}
	if cl == "" {
		cl = MutedStyle.Render("unknown")
	}
	lines = append(lines, detailRow("Client", cl))
	if s.Health == "blocked" {
		msg := s.HealthMessage
		if msg == "" {
			msg = "workspace boundary violation blocked"
		}
		for i, w := range wrapText(msg, max(rw-kw-2, 8)) {
			if i == 0 {
				lines = append(lines, detailRow("Health", w))
			} else {
				lines = append(lines, detailRow("", w))
			}
		}
	}

	lines = append(lines, "")
	var totalCalls int64
	for _, ts := range m.toolStats {
		totalCalls += ts.Calls
	}
	var issues int
	if m.lastDiagnosticsOutput != "" {
		_, _ = fmt.Sscanf(m.lastDiagnosticsOutput, "%d", &issues)
	}
	lines = append(lines, detailRow("Tools", fmt.Sprintf("%d", len(m.toolStats))))
	lines = append(lines, detailRow("Calls", fmt.Sprintf("%d", totalCalls)))
	lines = append(lines, detailRow("Issues", fmt.Sprintf("%d", issues)))
	if row, ok := m.topologyDetailRow(); ok {
		lines = append(lines, row)
	}

	return lines
}

// topologyDetailRow renders a one-line topology summary for the selected
// session's workspace, or ok=false when the workspace has no on-disk index
// (topology disabled, or not yet built). Counts come from a read-only snapshot;
// the live indexer state is not shown here because the TUI is a separate process.
func (m Model) topologyDetailRow() (string, bool) {
	if !m.topoStatusOK {
		return "", false
	}
	st := m.topoStatus
	info := fmt.Sprintf("%d nodes · %d files · %s", st.TotalNodes, st.IndexedFiles, byteSizeLabel(st.DBSizeBytes))
	if len(st.Languages) > 0 {
		info += " · " + strings.Join(st.Languages, ",")
	}
	return detailRow("Topology", info), true
}

// byteSizeLabel formats a byte count compactly for the detail panel.
func byteSizeLabel(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func (m *Model) rightLinesTools(rw int) []string {
	const (
		c2w, c3w, c4w = 8, 10, 6
	)
	s3 := "   "
	c1w := max(rw-2-c2w-c3w-c4w-12, 10)
	sln := "  " + SepStyle.Render(strings.Repeat("─", rw-3))
	roww := rw - 2

	if len(m.toolStats) == 0 {
		m.statsTableBodyRow = -1
		return []string{
			"  " + MutedStyle.Render("No calls recorded yet."),
		}
	}
	lc := render.PadRight(HintStyle.Render("Tool"), c1w)
	h := "  " + lc + s3 + render.PadLeft(HintStyle.Render("Calls"), c2w) + s3 + render.PadLeft(HintStyle.Render("Avg"), c3w) + s3 + HintStyle.Render("Errors")
	lines := []string{h, sln}
	m.statsTableBodyRow = 2 // tab bar + blank = 2 rows before this content
	for i, ts := range m.toolStats {
		sel := m.focusPanel == focusToolStats && i == m.toolStatsCursor
		tn := render.PadRight(truncate(ts.Tool, c1w-2), c1w-2)
		if sel {
			pc, pa, pe := render.PadLeft(fmt.Sprintf("%d", ts.Calls), c2w), render.PadLeft(fmt.Sprintf("%.0fms", ts.AvgMs), c3w), render.PadLeft("", c4w)
			if ts.Errors > 0 {
				pe = render.PadLeft(fmt.Sprintf("%d", ts.Errors), c4w)
			}
			lines = append(lines, SelectedStyle.Width(roww).Render("  > "+tn+s3+pc+s3+pa+s3+pe+s3))
		} else {
			c2, c3, c4 := render.PadLeft(OkStyle.Render(fmt.Sprintf("%d", ts.Calls)), c2w), render.PadLeft(MutedStyle.Render(fmt.Sprintf("%.0fms", ts.AvgMs)), c3w), render.PadLeft("", c4w)
			if ts.Errors > 0 {
				c4 = render.PadLeft(WarnStyle.Render(fmt.Sprintf("%d", ts.Errors)), c4w)
			}
			lines = append(lines, "  ∙ "+tn+s3+c2+s3+c3+s3+c4+s3)
		}
	}
	return lines
}

func (m *Model) rightLinesHistory(rw int) []string {
	const (
		c2w, c3w, c4w, c5w = 8, 10, 6, 12
	)
	s3 := "   "
	c1w := max(rw-2-c2w-c3w-c4w-12, 10)
	rc1w := max(c1w-c5w-3, 8)
	sln := "  " + SepStyle.Render(strings.Repeat("─", rw-3))
	roww := rw - 2

	if len(m.recentCalls) == 0 {
		m.recentTableBodyRow = -1
		return []string{
			"  " + MutedStyle.Render("No calls in this session yet."),
		}
	}
	rlc := render.PadRight(HintStyle.Render("Tool"), rc1w)
	h := "  " + rlc + s3 + render.PadLeft(HintStyle.Render("Dur"), c2w) + s3 + render.PadLeft(HintStyle.Render("When"), c3w) + s3 + render.PadLeft(HintStyle.Render("Err"), c4w) + s3 + HintStyle.Render("Session")
	lines := []string{h, sln}
	m.recentTableBodyRow = 2 // tab bar + blank = 2 rows before this content
	for i, c := range m.recentCalls {
		sel := m.focusPanel == focusStats && i == m.statsCursor
		tn := render.PadRight(truncate(c.Tool, rc1w-2), rc1w-2)
		sn := render.PadRight(truncate(c.SessionName, c5w), c5w)
		if sel {
			pd, pw, pe := render.PadLeft(fmt.Sprintf("%dms", c.DurationMs), c2w), render.PadLeft(render.HumanAge(c.CalledAt), c3w), render.PadLeft("", c4w)
			if !c.Success {
				pe = render.PadLeft("✗", c4w)
			}
			lines = append(lines, SelectedStyle.Width(roww).Render("  > "+tn+s3+pd+s3+pw+s3+pe+s3+sn))
		} else {
			mk := OkStyle.Render("✓") + " "
			if !c.Success {
				mk = WarnStyle.Render("✗") + " "
			}
			c2, c3, c4 := render.PadLeft(MutedStyle.Render(fmt.Sprintf("%dms", c.DurationMs)), c2w), render.PadLeft(MutedStyle.Render(render.HumanAge(c.CalledAt)), c3w), render.PadLeft("", c4w)
			if !c.Success {
				c4 = render.PadLeft(WarnStyle.Render("✗"), c4w)
			}
			c5 := render.PadRight(MutedStyle.Render(truncate(c.SessionName, c5w)), c5w)
			lines = append(lines, "  "+mk+tn+s3+c2+s3+c3+s3+c4+s3+c5)
		}
	}
	return lines
}

func (m Model) rightLinesDiagnostics(_ int) []string {
	if m.lastDiagnosticsOutput == "" {
		return []string{
			"  " + MutedStyle.Render("No diagnostics recorded yet."),
			"  " + MutedStyle.Render("Run the `diagnostics` tool in this session to populate this tab."),
		}
	}
	var lines []string
	for line := range strings.SplitSeq(m.lastDiagnosticsOutput, "\n") {
		if line == "" {
			lines = append(lines, "")
		} else {
			lines = append(lines, "  "+DetailStyle.Render(line))
		}
	}
	return lines
}
