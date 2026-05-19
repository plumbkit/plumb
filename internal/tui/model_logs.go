package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// filteredLogEntries returns log entries that match the current filter string
// (case-insensitive substring match on the raw line). Returns all entries when
// the filter is empty.
func (m Model) filteredLogEntries() []logEntry {
	if m.logFilter == "" {
		return m.logEntries
	}
	lower := strings.ToLower(m.logFilter)
	var out []logEntry
	for _, e := range m.logEntries {
		if strings.Contains(strings.ToLower(e.Raw), lower) {
			out = append(out, e)
		}
	}
	return out
}

// renderLogEntry formats a single log entry for display within width visible
// characters. Structured JSON entries are rendered with a timestamp, level
// badge, message, and key=val attributes; plain-text entries are shown as-is.
func (m Model) renderLogEntry(e logEntry, width int, selected bool) string {
	prefixMark := MutedStyle.Render("•")
	if selected {
		prefixMark = LogSelectedStyle.Render("❯")
	}
	if e.Msg == "" {
		// Plain text line — just show raw content.
		line := prefixMark + " " + MutedStyle.Render(truncate(e.Raw, width-2))
		if selected {
			line = LogSelectedStyle.Render(ansi.Strip(line))
		}
		return line
	}

	// Timestamp: "15:04:05" (8 chars) or blank.
	ts := "        "
	if !e.Time.IsZero() {
		ts = e.Time.Format("15:04:05")
	}

	// Level badge padded to 5 chars.
	const levelW = 5
	lvlText := padRight(e.Level, levelW)
	var lvlStyled string
	switch strings.ToUpper(strings.TrimSpace(e.Level)) {
	case "ERROR":
		lvlStyled = WarnStyle.Render(lvlText)
	case "WARN", "WARNING":
		lvlStyled = WarnStyle.Render(lvlText)
	case "DEBUG":
		lvlStyled = MutedStyle.Render(lvlText)
	default: // INFO and unknown
		lvlStyled = OkStyle.Render(lvlText)
	}

	// Attrs: key=val pairs, sorted for deterministic output.
	var attrParts []string
	if len(e.Attrs) > 0 {
		keys := make([]string, 0, len(e.Attrs))
		for k := range e.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrParts = append(attrParts, k+"="+e.Attrs[k])
		}
	}

	prefix := prefixMark + " " + MutedStyle.Render(ts) + " " + lvlStyled + "  "
	msg := DetailStyle.Render(e.Msg)
	attrs := ""
	if len(attrParts) > 0 {
		attrs = "  " + MutedStyle.Render(strings.Join(attrParts, " "))
	}

	line := prefix + msg + attrs
	// ANSI-aware truncation to keep within the cell boundary.
	if lipgloss.Width(line) > width-1 {
		line = ansi.Truncate(line, width-2, "…")
	}
	if selected {
		line = LogSelectedStyle.Render(ansi.Strip(line))
	}
	return line
}

// renderTopBorderLogs builds the plain top border for the Logs section.
func (m Model) renderTopBorderLogs(dimmed bool) string {
	sep := SepStyle
	if dimmed {
		sep = SepInactiveStyle
	}

	// Logs section is full-width, no divider.
	innerW := m.width - 2
	line := "╭" + strings.Repeat("─", innerW) + "╮"
	return sep.Render(overlayLogoBottom(line, m.width))
}

// logBodyScroll computes the clamped scroll offset for the log body given the
// total number of filtered entries and the available body height.
func (m Model) logBodyScroll(total, bodyHeight int) int {
	maxScroll := max(total-bodyHeight, 0)
	if m.logFollow {
		return maxScroll
	}
	s := m.logScroll
	if s > maxScroll {
		return maxScroll
	}
	if s < 0 {
		return 0
	}
	return s
}

func (m Model) selectedLogIndex(filteredLen int) int {
	if filteredLen == 0 {
		return 0
	}
	if m.logFollow {
		return filteredLen - 1
	}
	if m.logCursor < 0 {
		return 0
	}
	if m.logCursor >= filteredLen {
		return filteredLen - 1
	}
	return m.logCursor
}

func (m *Model) moveLogSelection(delta int) {
	filtered := m.filteredLogEntries()
	if len(filtered) == 0 {
		m.logCursor = 0
		m.logScroll = 0
		m.logFollow = false
		return
	}
	m.logCursor = max(m.selectedLogIndex(len(filtered))+delta, 0)
	if m.logCursor >= len(filtered) {
		m.logCursor = len(filtered) - 1
	}
	m.logFollow = false
	m.ensureLogCursorVisible(len(filtered))
}

func (m *Model) ensureLogCursorVisible(total int) {
	bodyHeight := m.logBodyHeight()
	maxScroll := max(total-bodyHeight, 0)
	if m.logCursor < m.logScroll {
		m.logScroll = m.logCursor
	}
	if m.logCursor >= m.logScroll+bodyHeight {
		m.logScroll = m.logCursor - bodyHeight + 1
	}
	if m.logScroll < 0 {
		m.logScroll = 0
	}
	if m.logScroll > maxScroll {
		m.logScroll = maxScroll
	}
}

func (m *Model) selectLogAtBodyRow(row int) {
	bodyHeight := m.logBodyHeight()
	if row < 0 || row >= bodyHeight {
		return
	}
	filtered := m.filteredLogEntries()
	if len(filtered) == 0 {
		return
	}
	scroll := m.logBodyScroll(len(filtered), bodyHeight)
	idx := scroll + row
	if idx >= len(filtered) {
		return
	}
	m.logCursor = idx
	m.logScroll = scroll
	m.logFollow = false
}

// renderLogBodyLine renders one row of the log body, applying the isOverlay
// dim treatment when an overlay panel is open.
func (m Model) renderLogBodyLine(entry *logEntry, innerW int, selected bool, isOverlay bool, rBar string) string {
	var line string
	if entry != nil {
		line = m.renderLogEntry(*entry, innerW-2, selected)
	}
	if isOverlay {
		cell := lipgloss.NewStyle().Width(innerW - 2).Render(line)
		return SepInactiveStyle.Render("│") + " " + InactiveStyle.Render(ansi.Strip(cell)) + " " + rBar
	}
	cell := lipgloss.NewStyle().Width(innerW - 2).Render(line)
	return SepStyle.Render("│") + " " + cell + " " + rBar
}

// renderLogsSection renders the full terminal content for the Logs section.
// It reuses the standard top menu and logo header but replaces the two-panel
// body with a full-width, scrollable log viewer.
func (m Model) renderLogsSection() string {
	bodyHeight := m.logBodyHeight()
	innerW := m.width - 2 // visible content width inside │ borders

	var sb strings.Builder
	isOverlay := m.showHelp || m.sectionMenuOpen

	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	// Header: 3-line top menu + logo.
	logoLines := strings.Split(LogoText, "\n")
	logoW := lipgloss.Width(logoLines[0])
	menu := m.renderTopMenu(m.width-logoW, isOverlay)
	for i := range 3 {
		sb.WriteString(menu[i] + sepStyle.Render(logoLines[i]) + "\n")
	}
	sb.WriteString(m.renderTopBorderLogs(isOverlay) + "\n")

	// Body: filtered log entries with scroll.
	filtered := m.filteredLogEntries()
	scroll := m.logBodyScroll(len(filtered), bodyHeight)
	visible := filtered[scroll:]
	scrollbar := scrollbarCol(len(filtered), bodyHeight, scroll)
	selectedIdx := m.selectedLogIndex(len(filtered))

	for i := range bodyHeight {
		rBar := SepStyle.Render("│")
		if scrollbar != nil && i < len(scrollbar) {
			rBar = scrollbar[i]
		}
		var entry *logEntry
		if i < len(visible) {
			e := visible[i]
			entry = &e
		}
		sb.WriteString(m.renderLogBodyLine(entry, innerW, scroll+i == selectedIdx, isOverlay, rBar) + "\n")
	}

	// In-frame status bar and bottom border.
	sb.WriteString(m.renderLogStatusBar(filtered, innerW, isOverlay) + "\n")
	sb.WriteString(sepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯") + "\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	final := sb.String()
	if m.logDetailOpen {
		final = m.renderLogDetail(final, filtered)
	}
	if m.showHelp {
		final = m.renderHelp(final)
	}
	if m.sectionMenuOpen {
		final = m.renderSectionMenuOverlay(final)
	}
	return final
}

// renderLogStatusBar builds the in-frame status bar for the Logs section.
func (m Model) renderLogStatusBar(filtered []logEntry, innerW int, dimmed bool) string {
	left := "Type to filter"
	if m.logFilter != "" {
		left = "Filter: " + m.logFilter
		if len(filtered) == 0 {
			left += "  (no matches)"
		}
	}
	right := fmt.Sprintf("enter details  ·  %d/%d lines", len(filtered), len(m.logEntries))
	contentW := max(innerW-2, 1)
	gap := max(contentW-2-lipgloss.Width(left)-lipgloss.Width(right), 1)
	content := " " + left + strings.Repeat(" ", gap) + right + " "
	content = lipgloss.NewStyle().Width(contentW).Render(content)
	if dimmed {
		return SepInactiveStyle.Render("│") + " " + InactiveStyle.Render(ansi.Strip(content)) + " " + SepInactiveStyle.Render("│")
	}
	return SepStyle.Render("│") + " " + LogStatusStyle.Render(content) + " " + SepStyle.Render("│")
}

func (m Model) renderLogDetail(bg string, filtered []logEntry) string {
	if len(filtered) == 0 {
		return bg
	}
	entry := filtered[m.selectedLogIndex(len(filtered))]
	boxW := m.width
	if boxW < 42 {
		boxW = 42
	}
	innerW := boxW - 2
	scrollH := max(m.height-10, 3)

	all := logDetailLines(entry, innerW-4)
	maxScroll := max(len(all)-scrollH, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxLogDetail = maxScroll
	}
	scroll := min(m.logDetailScroll, maxScroll)
	if scroll < 0 {
		scroll = 0
	}
	visible := all[scroll:]
	scrollbar := scrollbarCol(len(all), scrollH, scroll)

	title := " Log Detail "
	fill := max(innerW-lipgloss.Width(title)-1, 0)
	lines := []string{
		SepStyle.Render("╭─") + PanelHeaderStyle.Render(title) + SepStyle.Render(strings.Repeat("─", fill)+"╮"),
	}
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
	lines = append(lines, m.renderLogDetailStatusBar(innerW))
	lines = append(lines, SepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯"))
	return spliceOverlayAt(dimAll(bg), strings.Join(lines, "\n"), 0, bodyStartRow)
}

func (m Model) renderLogDetailContentLine(text string, innerW int, rBar string) string {
	cell := lipgloss.NewStyle().Width(innerW - 4).Render(ansi.Truncate(text, innerW-4, ""))
	return SepStyle.Render("│") + "  " + cell + "  " + rBar
}

func (m Model) renderLogDetailStatusBar(innerW int) string {
	contentW := max(innerW-2, 1)
	if m.logDetailCopied {
		content := StatusStyle.Render(padRight("Copied to the clipboard", contentW))
		return SepStyle.Render("│") + " " + content + " " + SepStyle.Render("│")
	}
	left := StatusKeyStyle.Render("c") + StatusStyle.Render(" copy")
	right := StatusKeyStyle.Render("esc") + StatusStyle.Render(" close")
	sep := StatusStyle.Render("  ·  ")
	plainW := lipgloss.Width("c copy  ·  esc close")
	gap := max(contentW-plainW, 0)
	content := left + sep + right + strings.Repeat(" ", gap)
	return SepStyle.Render("│") + " " + content + " " + SepStyle.Render("│")
}

func (m Model) currentLogDetailText() string {
	filtered := m.filteredLogEntries()
	if len(filtered) == 0 {
		return ""
	}
	entry := filtered[m.selectedLogIndex(len(filtered))]
	return entry.Raw + "\n"
}

func logDetailLines(e logEntry, width int) []string {
	if e.Msg == "" {
		if parsed, ok := parseSlogTextFields(e.Raw); ok {
			e = parsed
		}
	}
	var lines []string
	if !e.Time.IsZero() {
		lines = append(lines, logDetailField("Time", e.Time.Format(time.RFC3339)))
	}
	if e.Level != "" {
		lines = append(lines, logDetailField("Level", e.Level))
	}
	if e.Msg != "" {
		lines = append(lines, logDetailField("Message", e.Msg))
	}
	if len(e.Attrs) > 0 {
		keys := make([]string, 0, len(e.Attrs))
		for k := range e.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		lines = append(lines, "", logDetailTitle("Attributes"))
		for _, k := range keys {
			lines = append(lines, logDetailGutterLines(k+"="+e.Attrs[k], width)...)
		}
	}
	lines = append(lines, "", logDetailTitle("Raw"))
	lines = append(lines, logDetailGutterLines(e.Raw, width)...)
	return lines
}

func logDetailField(label, value string) string {
	return LogDetailKeyStyle.Render(padRight(label, 9)) + value
}

func logDetailTitle(label string) string {
	return LogDetailKeyStyle.Render(label)
}

func logDetailGutterLine(value string) string {
	return LogDetailGutterStyle.Render("┊ ") + value
}

func logDetailGutterLines(value string, width int) []string {
	wrapWidth := max(width-2, 1)
	wrapped := wrapPlain(value, wrapWidth)
	out := make([]string, 0, len(wrapped))
	for _, line := range wrapped {
		out = append(out, logDetailGutterLine(line))
	}
	return out
}

func parseSlogTextFields(raw string) (logEntry, bool) {
	fields := splitSlogText(raw)
	if len(fields) == 0 {
		return logEntry{}, false
	}
	out := logEntry{Raw: raw, Attrs: make(map[string]string)}
	recognised := false
	for _, field := range fields {
		k, v, ok := strings.Cut(field, "=")
		if !ok || k == "" {
			continue
		}
		v = strings.Trim(v, `"`)
		switch k {
		case "time":
			recognised = true
			out.Time, _ = time.Parse(time.RFC3339Nano, v)
		case "level":
			recognised = true
			out.Level = v
		case "msg":
			recognised = true
			out.Msg = v
		default:
			out.Attrs[k] = v
		}
	}
	return out, recognised
}

func splitSlogText(raw string) []string {
	var fields []string
	var b strings.Builder
	inQuote := false
	escaped := false
	for _, r := range raw {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
			b.WriteRune(r)
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ' ' && !inQuote:
			if b.Len() > 0 {
				fields = append(fields, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		fields = append(fields, b.String())
	}
	return fields
}

func wrapPlain(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var out []string
	rest := s
	for lipgloss.Width(rest) > width {
		part := ansi.Truncate(rest, width, "")
		out = append(out, part)
		rest = ansi.TruncateLeft(rest, lipgloss.Width(part), "")
	}
	out = append(out, rest)
	return out
}
