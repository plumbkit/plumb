package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// terminalSafeEntry scrubs every string an entry renders. Paths, tool names,
// session and agent labels come from history.db, and a repository can name a
// file with an escape sequence in it (see textfmt.TerminalSafe).
func terminalSafeEntry(e *history.Entry) {
	e.Path = textfmt.TerminalSafeLine(e.Path)
	e.From = textfmt.TerminalSafeLine(e.From)
	e.Tool = textfmt.TerminalSafeLine(e.Tool)
	e.SessionName = textfmt.TerminalSafeLine(e.SessionName)
	e.SessionID = textfmt.TerminalSafeLine(e.SessionID)
	e.LogicalAgent = textfmt.TerminalSafeLine(e.LogicalAgent)
	e.ClientName = textfmt.TerminalSafeLine(e.ClientName)
	e.Reason = textfmt.TerminalSafeLine(e.Reason)
	e.CallID = textfmt.TerminalSafeLine(e.CallID) // plumb-minted, so a lookup never changes
}

// filteredDiffEntries returns write-diff history records matching diffFilter.
func (m Model) filteredDiffEntries() []history.Entry {
	if m.diffFilter == "" {
		return m.diffEntries
	}
	q := strings.ToLower(m.diffFilter)
	var out []history.Entry
	for _, e := range m.diffEntries {
		if strings.Contains(strings.ToLower(e.Tool), q) ||
			strings.Contains(strings.ToLower(e.SessionName), q) ||
			strings.Contains(strings.ToLower(e.SessionID), q) ||
			strings.Contains(strings.ToLower(e.LogicalAgent), q) ||
			strings.Contains(strings.ToLower(e.Path), q) ||
			strings.Contains(strings.ToLower(e.From), q) ||
			strings.Contains(strings.ToLower(string(e.Op)), q) {
			out = append(out, e)
		}
	}
	return out
}

// diffEntryLine returns the visual line index in rightLines for entry targetIdx.
// Line 0 is rightTabBar, Line 1 is empty, Line 2 is table header, Line 3 is separator,
// Line 4 is the first entry. Each entry with GapBefore takes 2 lines, others take 1.
func (m Model) diffEntryLine(targetIdx int) int {
	entries := m.filteredDiffEntries()
	if targetIdx < 0 || targetIdx >= len(entries) {
		return -1
	}
	line := 4
	for i, e := range entries {
		if i == targetIdx {
			return line
		}
		line++
		if e.GapBefore {
			line++
		}
	}
	return -1
}

// diffEntryAtLine returns the entry index for the given line in rightLines,
// or -1 if the line is a header, separator, gap row, or past the entries.
func (m Model) diffEntryAtLine(bodyRow int) int {
	entries := m.filteredDiffEntries()
	if len(entries) == 0 || bodyRow < 4 {
		return -1
	}
	line := 4
	for i, e := range entries {
		if line == bodyRow {
			return i
		}
		line++
		if e.GapBefore {
			if line == bodyRow {
				return -1 // Gap row is non-selecting.
			}
			line++
		}
	}
	return -1
}

// ensureDiffCursorVisible scrolls rightScroll so that the selected entry
// (and its gap row, if any) is within the visible right-panel viewport.
func (m *Model) ensureDiffCursorVisible() {
	top := m.diffEntryLine(m.diffCursor)
	if top < 0 {
		return
	}
	entries := m.filteredDiffEntries()
	bottom := top
	if m.diffCursor < len(entries) && entries[m.diffCursor].GapBefore {
		bottom++
	}
	if m.diffCursor == 0 {
		top = 0 // Reveal headers when at top.
	}

	bodyHeight := max(m.height-6, 1)
	rightFooterLen := 0
	if m.currentSection == 1 {
		rightFooterLen = len(m.sessionsRightFooter())
	}
	rightViewH := max(bodyHeight-rightFooterLen, 1)

	if bottom >= m.rightScroll+rightViewH {
		m.rightScroll = bottom - rightViewH + 1
	}
	if top < m.rightScroll {
		m.rightScroll = top
	}
	if m.rightScroll < 0 {
		m.rightScroll = 0
	}
}

func (m *Model) rightLinesDiffs(rw int) []string {
	if m.historyErr != "" {
		return []string{"  " + WarnStyle.Render("History error: "+m.historyErr)}
	}
	if m.historyReader == nil {
		return []string{"  " + MutedStyle.Render("No write history database found.")}
	}
	if m.diffNoWorkspace {
		return []string{"  " + MutedStyle.Render("No workspace selected: pick a session to see its write history.")}
	}

	entries := m.filteredDiffEntries()
	if len(entries) == 0 {
		if m.diffFilter != "" {
			return []string{"  " + MutedStyle.Render(fmt.Sprintf("No changes match filter %q.", m.diffFilter))}
		}
		return []string{"  " + MutedStyle.Render("No write changes recorded for this workspace yet.")}
	}

	const (
		cWhen, cOp, cTool, cWriter, cDiff = 12, 8, 14, 15, 22
	)
	s3 := "   "
	// A row is the 4-cell "  > " marker, six columns and five 3-cell
	// separators, and must fit roww (rw-2): leaving out the marker made the
	// selected row 4 cells too wide, so SelectedStyle wrapped it onto two lines.
	cPath := max(rw-2-4-cWhen-cOp-cTool-cWriter-cDiff-15, 12)
	sln := "  " + SepStyle.Render(strings.Repeat("─", rw-3))
	roww := rw - 2

	h := "  " + render.PadRight(HintStyle.Render("When"), cWhen) + s3 +
		render.PadRight(HintStyle.Render("Op"), cOp) + s3 +
		render.PadRight(HintStyle.Render("Tool"), cTool) + s3 +
		render.PadRight(HintStyle.Render("Writer"), cWriter) + s3 +
		render.PadRight(HintStyle.Render("Path"), cPath) + s3 +
		render.PadLeft(HintStyle.Render("Diff"), cDiff)

	lines := []string{h, sln}

	for i, e := range entries {
		sel := m.focusPanel == focusDiffs && i == m.diffCursor
		row := m.formatDiffRow(e, cWhen, cOp, cTool, cWriter, cPath, cDiff, s3, roww, sel)
		lines = append(lines, row)
		if e.GapBefore {
			lines = append(lines, m.formatDiffGap(e, rw))
		}
	}

	if m.diffFilterActive {
		lines = append(lines, "", "  "+MutedStyle.Render("⌕ ")+DetailStyle.Render(m.diffFilter)+SelectedStyle.Render("▎"))
	} else if m.diffFilter != "" {
		lines = append(lines, "", "  "+MutedStyle.Render("⌕ "+m.diffFilter+" (esc to clear)"))
	}

	return lines
}

func (m Model) formatDiffRow(e history.Entry, cWhen, cOp, cTool, cWriter, cPath, cDiff int, s3 string, roww int, sel bool) string {
	whenStr := e.At.Local().Format("01-02 15:04")
	opStr := string(e.Op)
	if e.Kind == history.KindDir {
		opStr += " dir"
	}
	who := stats.AgentLabel(e.SessionName, e.LogicalAgent)
	if who == "" {
		if len(e.SessionID) > 8 {
			who = e.SessionID[:8]
		} else {
			who = e.SessionID
		}
	}
	if who == "" {
		who = "-"
	}

	p := e.Path
	if e.From != "" {
		p = e.From + " → " + e.Path
	}

	diffStats := fmt.Sprintf("+%d -%d", e.Added, e.Removed)
	if e.Content != history.ContentDiff && e.Content != "" {
		marker := strings.TrimPrefix(string(e.Content), "withheld:")
		diffStats += " [" + marker + "]"
	}

	pw := render.PadRight(textfmt.Ellipsis(whenStr, cWhen), cWhen)
	po := render.PadRight(textfmt.Ellipsis(opStr, cOp), cOp)
	pt := render.PadRight(textfmt.Ellipsis(e.Tool, cTool), cTool)
	pwr := render.PadRight(textfmt.Ellipsis(who, cWriter), cWriter)
	pp := render.PadRight(textfmt.Ellipsis(p, cPath), cPath)
	pd := render.PadLeft(textfmt.Ellipsis(diffStats, cDiff), cDiff)

	if sel {
		return SelectedStyle.Width(roww).Render("  > " + pw + s3 + po + s3 + pt + s3 + pwr + s3 + pp + s3 + pd)
	}
	return "  ∙ " + MutedStyle.Render(pw) + s3 + DetailStyle.Render(po) + s3 + DetailStyle.Render(pt) + s3 +
		MutedStyle.Render(pwr) + s3 + DetailStyle.Render(pp) + s3 + OkStyle.Render(pd)
}

func (m Model) formatDiffGap(e history.Entry, _ int) string {
	gap := "    ⋯ unrecorded change (outside plumb's write tools)"
	if e.GapDropped {
		gap += "; history dropped rows in this interval"
	}
	return WarnStyle.Render(gap)
}

func (m *Model) openDiffDetail(e history.Entry) {
	m.diffDetailOpen = true
	m.diffDetailScroll = 0
	m.diffDetailEntry = e
	m.diffDetailText = ""
	m.diffDetailErr = ""

	if m.historyReader != nil && e.Content == history.ContentDiff {
		_, diff, err := m.historyReader.Get(e.Seq)
		if err == nil {
			// The diff is file content: untrusted, so scrubbed before it is
			// rendered or copied. Tabs are expanded so line widths hold.
			m.diffDetailText = strings.ReplaceAll(textfmt.TerminalSafe(diff), "\t", "    ")
		} else {
			m.diffDetailErr = textfmt.TerminalSafeLine(err.Error())
		}
	}

	if m.globalDB == nil {
		m.ensureGlobalDB()
	}
	if m.globalDB != nil && e.CallID != "" {
		call, ok, _ := m.globalDB.CallByID(e.CallID)
		call.Tool = textfmt.TerminalSafeLine(call.Tool)
		call.ErrorMsg = textfmt.TerminalSafeLine(call.ErrorMsg)
		call.SessionName = textfmt.TerminalSafeLine(call.SessionName)
		m.diffDetailCall = call
		m.diffDetailHaveCall = ok
	} else {
		m.diffDetailHaveCall = false
	}
}

func (m Model) diffDetailMaxScroll() int {
	boxW := max(m.width, 60)
	innerW := boxW - 2
	scrollH := max(m.height-10, 4)
	allLines := m.diffDetailContentLines(innerW - 4)
	return max(len(allLines)-scrollH, 0)
}

func (m Model) renderDiffDetail(bg string) string {
	boxW := max(m.width, 60)
	innerW := boxW - 2
	scrollH := max(m.height-10, 4)

	allLines := m.diffDetailContentLines(innerW - 4)
	maxScroll := max(len(allLines)-scrollH, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxDiffDetail = maxScroll
	}
	scroll := max(min(m.diffDetailScroll, maxScroll), 0)
	visible := allLines[scroll:]
	scrollbar := scrollbarCol(len(allLines), scrollH, scroll, false)

	title := fmt.Sprintf(" Diff Detail: #%d %s ", m.diffDetailEntry.Seq, m.diffDetailEntry.Path)
	fill := max(innerW-lipgloss.Width(title)-1, 0)
	lines := make([]string, 0, scrollH+4)
	lines = append(lines,
		SepStyle.Render("╭─")+PanelHeaderStyle.Render(title)+SepStyle.Render(strings.Repeat("─", fill)+"╮"),
	)
	lines = append(lines, m.renderLogDetailContentLine("", innerW, SepStyle.Render("│")))

	for i := range scrollH {
		text := ""
		if i < len(visible) {
			text = visible[i]
		}
		rBar := SepStyle.Render("│")
		if scrollbar != nil && i < len(scrollbar) {
			rBar = scrollbar[i]
		}
		lines = append(lines, m.renderLogDetailContentLine(text, innerW, rBar))
	}

	lines = append(lines, m.renderLogDetailContentLine("", innerW, SepStyle.Render("│")))
	lines = append(lines, m.renderDiffDetailStatusBar(innerW))
	lines = append(lines, SepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯"))

	return spliceOverlayAt(dimAll(bg), strings.Join(lines, "\n"), 0, bodyStartRow)
}

func (m Model) diffDetailCallHeader() string {
	if m.diffDetailHaveCall {
		status := "✓ success"
		if !m.diffDetailCall.Success {
			status = "✗ failed: " + m.diffDetailCall.ErrorMsg
		}
		sess := m.diffDetailCall.SessionName
		if sess == "" {
			sess = "-"
		}
		callHdr := fmt.Sprintf("Tool Call: %s (%dms, %s)  session %s  at %s",
			m.diffDetailCall.Tool, m.diffDetailCall.DurationMs, status, sess,
			m.diffDetailCall.CalledAt.Local().Format("15:04:05.000"))
		return PanelHeaderStyle.Render(callHdr)
	}
	if m.diffDetailEntry.CallID != "" {
		return MutedStyle.Render("(tool call metadata unavailable in stats.db for call_id " + m.diffDetailEntry.CallID + ")")
	}
	return ""
}

func (m Model) diffDetailChangeLines() []string {
	e := m.diffDetailEntry
	who := stats.AgentLabel(e.SessionName, e.LogicalAgent)
	chgHdr := fmt.Sprintf("Change: seq #%d  op: %s  tool: %s  writer: %s  diff: +%d -%d",
		e.Seq, e.Op, e.Tool, who, e.Added, e.Removed)
	lines := []string{DetailStyle.Render(chgHdr)}
	if e.Redactions > 0 {
		lines = append(lines, WarnStyle.Render(fmt.Sprintf("Redactions: %d tokens redacted", e.Redactions)))
	}
	if e.Reason != "" {
		lines = append(lines, DetailStyle.Render("Reason: "+e.Reason))
	}
	return lines
}

func (m Model) diffDetailBodyLines() []string {
	e := m.diffDetailEntry
	fromPath := e.Path
	if e.From != "" {
		fromPath = e.From
	}
	lines := []string{HintStyle.Render("--- a/" + fromPath), HintStyle.Render("+++ b/" + e.Path)}
	if e.Content != history.ContentDiff && e.Content != "" {
		return append(lines, "", WarnStyle.Render(fmt.Sprintf("[%s] - diff content was withheld", e.Content)))
	}
	if m.diffDetailErr != "" {
		return append(lines, "", WarnStyle.Render("error reading diff: "+m.diffDetailErr))
	}
	if m.diffDetailText == "" {
		return append(lines, MutedStyle.Render("(empty diff)"))
	}
	for _, line := range strings.Split(m.diffDetailText, "\n") {
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			lines = append(lines, OkStyle.Render(line))
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			lines = append(lines, WarnStyle.Render(line))
		case strings.HasPrefix(line, "@@"):
			lines = append(lines, PanelHeaderStyle.Render(line))
		default:
			lines = append(lines, DetailStyle.Render(line))
		}
	}
	return lines
}

func (m Model) diffDetailContentLines(_ int) []string {
	var lines []string
	if hdr := m.diffDetailCallHeader(); hdr != "" {
		lines = append(lines, hdr)
	}
	lines = append(lines, m.diffDetailChangeLines()...)
	lines = append(lines, "")
	lines = append(lines, m.diffDetailBodyLines()...)
	return lines
}

func (m Model) renderDiffDetailStatusBar(innerW int) string {
	left := "esc/enter/q close  ·  j/k scroll  ·  c copy diff"
	if m.copyStatus.text != "" {
		left = m.copyStatus.text
	}
	contentW := max(innerW-2, 1)
	gap := max(contentW-2-lipgloss.Width(left), 1)
	content := " " + left + strings.Repeat(" ", gap) + " "
	content = lipgloss.NewStyle().Width(contentW).Render(content)
	return SepStyle.Render("│") + " " + LogStatusStyle.Render(content) + " " + SepStyle.Render("│")
}

func (m Model) handleDiffDetailKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	key := msg.String()
	switch key {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		return m.mainKeyQuit()
	case "esc", "q", "enter":
		m.diffDetailOpen = false
		return m, nil
	case "j", "down":
		maxScroll := m.diffDetailMaxScroll()
		if m.diffDetailScroll < maxScroll {
			m.diffDetailScroll++
		}
		return m, nil
	case "k", "up":
		if m.diffDetailScroll > 0 {
			m.diffDetailScroll--
		}
		return m, nil
	case "g":
		m.diffDetailScroll = 0
		return m, nil
	case "G":
		m.diffDetailScroll = m.diffDetailMaxScroll()
		return m, nil
	case "c", "y":
		return m, copyTextToClipboard(m.diffDetailText)
	default:
		return m, nil
	}
}

func (m Model) handleDiffFilterKey(s string) (Model, bool) {
	switch s {
	case "esc":
		m.diffFilter = ""
		m.diffFilterActive = false
		m.diffCursor = 0
		return m, true
	case "enter":
		m.diffFilterActive = false
		return m, true
	case "backspace":
		r := []rune(m.diffFilter)
		if len(r) > 0 {
			m.diffFilter = string(r[:len(r)-1])
			m.diffCursor = 0
		}
		return m, true
	default:
		if len(s) == 1 && s >= " " {
			m.diffFilter += s
			m.diffCursor = 0
			return m, true
		}
		return m, false
	}
}
