package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/monitor"
)

func (m Model) View() tea.View {
	content := "Loading…"
	if m.ready {
		content = m.render()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m Model) render() string {
	// Dashboard and Logs sections each use a dedicated full-width renderer.
	if m.currentSection == 0 && !m.showPopup {
		return m.renderDashboard()
	}
	if m.currentSection == 3 && !m.showPopup {
		return m.renderLogsSection()
	}
	if m.currentSection == 4 && !m.showPopup {
		return m.renderSettingsSection()
	}

	rightWidth := max(m.width-m.leftWidth-3, 10)
	bodyHeight := max(m.height-6, 1)

	var sb strings.Builder
	isOverlay := m.showPopup || m.showHelp || m.sectionMenuOpen || m.showThemePicker

	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	// Header: 4-line Logo
	logoLines := strings.Split(LogoText, "\n")

	// Ensure logo starts at exactly right edge
	logoW := lipgloss.Width(logoLines[0])
	tabSpaceW := m.width - logoW

	menu := m.renderTopMenu(tabSpaceW, isOverlay)

	for i := range 3 {
		sb.WriteString(menu[i] + sepStyle.Render(logoLines[i]) + "\n")
	}
	sb.WriteString(m.renderTopBorder(rightWidth, isOverlay) + "\n")

	allLeftLines := m.leftLines()
	allRightLines := (&m).rightLines(rightWidth)
	sb.WriteString(m.renderBodySection(allLeftLines, allRightLines, bodyHeight, rightWidth, isOverlay))

	sb.WriteString(m.renderBottomBorder(rightWidth, isOverlay) + "\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	final := sb.String()
	if m.showPopup {
		final = m.renderPopup(final, bodyHeight-1)
	}
	final = m.applyOverlays(final)
	if m.renameModal != nil {
		final = m.renameModal.renderModal(final, m.width, m.height)
	}
	return final
}

// applyOverlays composites the help, section-menu, and theme-picker overlays
// (in that order) onto an already-rendered section. Shared by every full-width
// section renderer so the theme picker (global ^t) appears over all of them.
func (m Model) applyOverlays(final string) string {
	if m.showHelp {
		final = m.renderHelp(final)
	}
	if m.sectionMenuOpen {
		final = m.renderSectionMenuOverlay(final)
	}
	if m.showThemePicker {
		final = m.renderThemePicker(final)
	}
	return final
}

func (m Model) renderBodySection(allLeftLines, allRightLines []string, bodyHeight, rightWidth int, isOverlay bool) string {
	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	// Clamp scroll offsets
	maxLeftScroll := max(len(allLeftLines)-bodyHeight, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxLeft = maxLeftScroll
	}
	if m.leftScroll > maxLeftScroll {
		m.leftScroll = maxLeftScroll
	}
	maxRightScroll := max(len(allRightLines)-bodyHeight, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxRight = maxRightScroll
	}
	if m.rightScroll > maxRightScroll {
		m.rightScroll = maxRightScroll
	}

	leftLines := allLeftLines[m.leftScroll:]
	rightLines := allRightLines[m.rightScroll:]

	leftScrollbar := scrollbarCol(len(allLeftLines), bodyHeight, m.leftScroll, isOverlay)
	rightScrollbar := scrollbarCol(len(allRightLines), bodyHeight, m.rightScroll, isOverlay)

	var sb strings.Builder
	for i := range bodyHeight {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}

		leftCell := lipgloss.NewStyle().Width(m.leftWidth).Render(ansi.Truncate(l, m.leftWidth-1, "…") + " ")
		rightCell := lipgloss.NewStyle().Width(rightWidth).Render(ansi.Truncate(r, rightWidth, "…"))

		lBar := SepStyle.Render("│")
		if leftScrollbar != nil && i < len(leftScrollbar) {
			lBar = leftScrollbar[i]
		}
		rBar := SepStyle.Render("│")
		if rightScrollbar != nil && i < len(rightScrollbar) {
			rBar = rightScrollbar[i]
		}

		if isOverlay {
			lDim := InactiveStyle.Render(ansi.Strip(leftCell))
			rDim := InactiveStyle.Render(ansi.Strip(rightCell))
			line := sepStyle.Render("│") + lDim + sepStyle.Render("┆") + rDim + sepStyle.Render("│")
			sb.WriteString(line + "\n")
		} else {
			sb.WriteString(lBar + leftCell + SepStyle.Render("┆") + rightCell + rBar + "\n")
		}
	}
	return sb.String()
}

func (m Model) renderMainStatusBar(dimmed bool) string {
	style := StatusStyle
	keyStyle := StatusKeyStyle
	if dimmed {
		style = InactiveStyle
		keyStyle = InactiveStyle
	}
	rightFooterPlain := "/ menu  ·  ^q quit  ·  ^h help "

	if m.waitingForQuit {
		msg := " Press ctrl+c again to quit "
		msgStyle := WarningMsgStyle
		if dimmed {
			msgStyle = InactiveStyle
		}

		leftFooter := msgStyle.Render(msg)
		// fill until 3 columns before the right footer
		fillW := max(m.width-lipgloss.Width(msg)-lipgloss.Width(rightFooterPlain)-3, 0)

		rightFooter := keyStyle.Render("/") + style.Render(" menu  ·  ") +
			keyStyle.Render("^q") + style.Render(" quit  ·  ") +
			keyStyle.Render("^h") + style.Render(" help ")

		return leftFooter + strings.Repeat(" ", fillW) + "   " + rightFooter
	}

	vStr := Version
	if vStr == "" {
		vStr = "dev"
	}
	sessCount := int64(len(m.sessions))
	memStr := "n/a"
	if m.daemonMetricsOK && m.daemonMetrics.RSSAvailable {
		memStr = monitor.FormatBytes(m.daemonMetrics.RSSBytes)
	}
	leftFooter := fmt.Sprintf(
		" plumb %s  ·  %s  ·  daemon mem: %s",
		vStr,
		formatSessionCount(sessCount),
		memStr,
	)
	footerGap := max(m.width-lipgloss.Width(leftFooter)-lipgloss.Width(rightFooterPlain), 1)
	rightFooter := keyStyle.Render("/") + style.Render(" menu  ·  ") +
		keyStyle.Render("^q") + style.Render(" quit  ·  ") +
		keyStyle.Render("^h") + style.Render(" help ")
	return style.Render(leftFooter) + strings.Repeat(" ", footerGap) + rightFooter
}

func (m Model) renderTopBorder(rightWidth int, dimmed bool) string {
	sepStyle := SepStyle
	if dimmed {
		sepStyle = SepInactiveStyle
	}

	// The body divider ┆ is at index m.leftWidth + 1.
	// We want ┬ to be at the same index.
	// Total width before the logo should match the body's content width.
	contentW := m.leftWidth + rightWidth + 1
	filler := []rune(strings.Repeat("─", contentW))
	if m.leftWidth < len(filler) {
		filler[m.leftWidth] = '┬'
	}

	line := "╭" + string(filler) + "╮"
	return sepStyle.Render(overlayLogoBottom(line, m.width))
}

func (m Model) renderBottomBorder(rightWidth int, dimmed bool) string {
	sepStyle := SepStyle
	if dimmed {
		sepStyle = SepInactiveStyle
	}
	contentW := m.leftWidth + rightWidth + 1
	filler := []rune(strings.Repeat("─", contentW))
	if m.leftWidth < len(filler) {
		filler[m.leftWidth] = '┴'
	}
	return sepStyle.Render("╰" + string(filler) + "╯")
}

func (m Model) renderPopup(bg string, bodyHeight int) string {
	if m.popupLeftWidth == 0 {
		m.popupLeftWidth = minPopupLeftWidth
	}
	pLW, pRW := m.popupLeftWidth, m.width-m.popupLeftWidth-3
	if pRW < 10 {
		pRW = 10
	}

	lines := make([]string, 0, 2+bodyHeight)
	lines = append(lines, m.renderTopBorderPopup(pLW, pRW))
	allLeft := m.popupLeftLines()
	maxPL := max(len(allLeft)-bodyHeight, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxPopupLeft = maxPL
	}
	if m.popupLeftScroll > maxPL {
		m.popupLeftScroll = maxPL
	}
	visibleLeft := allLeft[m.popupLeftScroll:]
	leftScrollbar := scrollbarCol(len(allLeft), bodyHeight, m.popupLeftScroll, false)
	allRight := m.popupRightAll(pRW - 2)

	rightScrollH := max(bodyHeight-2, 0)

	maxDS := max(len(allRight)-rightScrollH, 0)
	if m.scrollBounds != nil {
		m.scrollBounds.maxPopupDetail = maxDS
	}
	if m.popupDetailScroll > maxDS {
		m.popupDetailScroll = maxDS
	}
	visibleRight := allRight[m.popupDetailScroll:]
	scrollbar := scrollbarCol(len(allRight), rightScrollH, m.popupDetailScroll, false)

	lines = append(lines, m.renderPopupBody(visibleLeft, visibleRight, bodyHeight, pLW, pRW, leftScrollbar, scrollbar, rightScrollH)...)
	lines = append(lines, m.renderBottomBorderPopup(pLW, pRW))

	overlayText := strings.Join(lines, "\n")

	// The overlay should start on row 4 (line 5 visually)
	// and end 1 row above the status bar (m.height - 2).

	ovLines := strings.Split(overlayText, "\n")
	ovW := 0
	for _, l := range ovLines {
		if lw := lipgloss.Width(l); lw > ovW {
			ovW = lw
		}
	}
	sx := (m.width - ovW) / 2
	sy := 4

	return spliceOverlayAt(bg, overlayText, sx, sy)
}

func (m Model) renderPopupBody(visibleLeft, visibleRight []string, bodyHeight, pLW, pRW int, leftScrollbar, scrollbar []string, rightScrollH int) []string {
	lines := make([]string, 0, bodyHeight)
	for i := range bodyHeight {
		var lCell string
		if i < len(visibleLeft) && visibleLeft[i] != "" {
			lCell = lipgloss.NewStyle().Width(pLW).Render(visibleLeft[i])
		} else {
			lCell = lipgloss.NewStyle().Width(pLW).Render("")
		}
		var rStr string
		if i >= bodyHeight-2 {
			if i == bodyHeight-1 {
				sep := StatusStyle.Render("  ·  ")
				if m.popupRightFocus {
					left := StatusKeyStyle.Render("c") + StatusStyle.Render(" copy")
					mid := StatusKeyStyle.Render("tab") + StatusStyle.Render(" back")
					right := StatusKeyStyle.Render("esc") + StatusStyle.Render(" close")
					rStr = "  " + left + sep + mid + sep + right
				} else {
					mid := StatusKeyStyle.Render("tab") + StatusStyle.Render(" detail")
					right := StatusKeyStyle.Render("esc") + StatusStyle.Render(" close")
					rStr = "  " + mid + sep + right
				}
			}
		} else if i < len(visibleRight) {
			rStr = visibleRight[i]
		}
		rCell := lipgloss.NewStyle().Width(pRW).Render(rStr)

		lb := SepStyle.Render("│")
		if leftScrollbar != nil && i < len(leftScrollbar) {
			lb = leftScrollbar[i]
		}
		rb := SepStyle.Render("│")
		if scrollbar != nil && i < len(scrollbar) && i < rightScrollH {
			rb = scrollbar[i]
		}

		lines = append(lines, lb+lCell+SepStyle.Render("┆")+rCell+rb)
	}
	return lines
}

func (m Model) renderHelp(bg string) string {
	type helpItem struct {
		key  string
		desc string
	}
	type helpGroup struct {
		title string
		items []helpItem
	}
	groups := []helpGroup{
		{
			title: "Navigation",
			items: []helpItem{
				{key: "↑/↓  j/k", desc: "Move through lists and scroll details"},
				{key: "pgup/pgdown", desc: "Page through lists"},
			},
		},
		{
			title: "Sections",
			items: []helpItem{
				{key: "/", desc: "Open section selector"},
				{key: "ctrl+1-5, alt+1-5", desc: "Jump to Dashboard, Sessions, Memory, Logs, Settings"},
			},
		},
		{
			title: "Panels",
			items: []helpItem{
				{key: "tab / shift+tab", desc: "Switch focus: sessions, details, tools, recent"},
				{key: "[  ]", desc: "Resize columns"},
			},
		},
		{
			title: "Settings",
			items: []helpItem{
				{key: "↑/↓  ←/→", desc: "Move between / change settings"},
				{key: "enter / space", desc: "Toggle, or open the theme picker"},
				{key: "theme: ↑/↓", desc: "Apply + save live; esc closes"},
			},
		},
		{
			title: "Actions",
			items: []helpItem{
				{key: "enter", desc: "Open detail or select menu item"},
				{key: "esc", desc: "Close popup or menu"},
				{key: "ctrl+t", desc: "Open the theme picker (anywhere)"},
				{key: "ctrl+h", desc: "Open help"},
				{key: "ctrl+q", desc: "Quit"},
			},
		},
	}

	boxW := 84
	innerW := boxW - 2
	topLabel := " Help & Navigation "

	// Calculate dashes for the top border correctly
	labelW := lipgloss.Width(topLabel)
	leftDashes := 1
	rightDashes := max(innerW-labelW-leftDashes, 0)

	top := SepStyle.Render("╭─") + PanelHeaderStyle.Render(topLabel) + SepStyle.Render(strings.Repeat("─", rightDashes)+"╮")

	bodyLines := make([]string, 0, 18)
	// Empty row top
	bodyLines = append(bodyLines, SepStyle.Render("│")+strings.Repeat(" ", innerW)+SepStyle.Render("│"))

	for gi, group := range groups {
		if gi > 0 {
			bodyLines = append(bodyLines, SepStyle.Render("│")+strings.Repeat(" ", innerW)+SepStyle.Render("│"))
		}
		bodyLines = append(bodyLines, renderHelpContentLine(innerW, "   "+PanelHeaderFadedStyle.Render(group.title)))
		for _, item := range group.items {
			bodyLines = append(bodyLines, renderHelpRow(innerW, item.key, item.desc))
		}
	}

	// Empty row bottom
	bodyLines = append(bodyLines, SepStyle.Render("│")+strings.Repeat(" ", innerW)+SepStyle.Render("│"))

	bottom := SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯")

	popup := top + "\n" + strings.Join(bodyLines, "\n") + "\n" + bottom
	return spliceOverlay(bg, popup, m.width, m.height)
}

func renderHelpRow(innerW int, key, desc string) string {
	keyW := 17
	content := "   " +
		SelectedStyle.Width(keyW).Render(key) +
		"  " +
		DetailStyle.Render(desc)
	return renderHelpContentLine(innerW, content)
}

func renderHelpContentLine(innerW int, content string) string {
	pad := max(innerW-lipgloss.Width(content)-3, 0)
	return SepStyle.Render("│") + content + strings.Repeat(" ", pad) + "   " + SepStyle.Render("│")
}

func (m Model) renderSectionMenuOverlay(bg string) string {
	border := SepStyle
	textStyle := DetailStyle
	selectedStyle := SelectedStyle

	innerW := sectionMenuWidth - 2
	lines := make([]string, 0, 2+len(sectionMenuItems))
	title := " Section "
	topFill := max(sectionMenuWidth-lipgloss.Width("╭─")-lipgloss.Width(title)-lipgloss.Width("╮"), 0)
	lines = append(lines, border.Render("╭─")+PanelHeaderStyle.Render(title)+border.Render(strings.Repeat("─", topFill)+"╮"))
	for i, item := range sectionMenuItems {
		marker := " "
		style := textStyle
		if i == m.sectionMenuCursor {
			marker = "❯"
			style = selectedStyle
		}
		content := style.Render(fmt.Sprintf(" %s %d. %-11s  ", marker, i+1, item))
		pad := max(innerW-lipgloss.Width(content), 0)
		lines = append(lines, border.Render("│")+content+strings.Repeat(" ", pad)+border.Render("│"))
	}
	lines = append(lines, border.Render("╰"+strings.Repeat("─", innerW)+"╯"))

	return spliceOverlayAt(bg, strings.Join(lines, "\n"), 0, 0)
}

func (m Model) renderTopBorderPopup(pLW, pRW int) string {
	lt, rt := " Timestamp ", " Call Detail "
	var lts, rts lipgloss.Style
	if m.popupRightFocus {
		lts, rts = PanelHeaderFadedStyle, PanelHeaderStyle
	} else {
		lts, rts = PanelHeaderStyle, PanelHeaderFadedStyle
	}
	lf := max(pLW-1-len(lt), 0)
	rf := max(pRW-1-len(rt), 0)
	return SepStyle.Render("╭─") + lts.Render(lt) + SepStyle.Render(strings.Repeat("─", lf)+"┬─") + rts.Render(rt) + SepStyle.Render(strings.Repeat("─", rf)+"╮")
}

func (m Model) renderBottomBorderPopup(pLW, pRW int) string {
	b := []rune("╰" + strings.Repeat("─", pLW+pRW+1) + "╯")
	b[pLW+1] = '┴'
	return SepStyle.Render(string(b))
}
