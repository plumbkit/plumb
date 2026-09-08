package tui

// model_settings_render.go — Settings section layout: the two-pane (Scope /
// rows) body, scope-column sizing, the tab bar, and the logical-line model.

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// settingsTabBar renders the rows-pane tab bar: [General] [LSP], the active tab
// highlighted (brightest when the rows pane itself holds focus).
func (m Model) settingsTabBar() string {
	rowsFocused := !m.settingsScopeFocus
	parts := make([]string, len(settingsTabNames))
	for i, n := range settingsTabNames {
		label := "[" + n + "]"
		switch {
		case i == m.settingsTab && rowsFocused:
			parts[i] = SelectedStyle.Render(label)
		case i == m.settingsTab:
			parts[i] = PanelHeaderStyle.Render(label)
		default:
			parts[i] = FadedStyle.Render(label)
		}
	}
	return " " + strings.Join(parts, "  ")
}

// settingsLineKind classifies a logical line in the scrollable settings list.
type settingsLineKind int

const (
	slBlank settingsLineKind = iota
	slHeader
	slRow
)

// settingsLine is a width-independent description of one scrollable line. It is
// shared by the renderer and the mouse-click row resolver.
type settingsLine struct {
	kind  settingsLineKind
	group string // slHeader
	item  int    // slRow: index into settingsItems
	cont  int    // slRow: 0 = the row itself; >0 = the Nth list-entry continuation line
}

// settingsLogicalLines describes the scrollable list: each group as a header
// followed by its rows, with a blank line between groups (no leading blank).
func settingsLogicalLines(items []settingItem) []settingsLine {
	out := []settingsLine{}
	last := ""
	for i, it := range items {
		if it.group != last {
			if last != "" {
				out = append(out, settingsLine{kind: slBlank})
			}
			out = append(out, settingsLine{kind: slHeader, group: it.group})
			last = it.group
		}
		out = append(out, settingsLine{kind: slRow, item: i})
		// A multi-entry list row stacks its remaining entries on continuation lines.
		if it.kind == settingList {
			for j := 1; j < rowValueCount(it); j++ {
				out = append(out, settingsLine{kind: slRow, item: i, cont: j})
			}
		}
	}
	return out
}

// rowLabel is a row's display label: list rows get a trailing "(N)" count so the
// value column can stack one entry per line instead of a long joined string.
func rowLabel(it settingItem) string {
	if n := rowValueCount(it); it.kind == settingList && n > 0 {
		return fmt.Sprintf("%s (%d)", it.label, n)
	}
	return it.label
}

// rowValueCount is how many entries a list row has. It reads entries when the
// row carries them and falls back to list otherwise, so the "(N)" count, the
// continuation lines and the rendered cells are all driven by one number — a row
// whose two sources disagreed would stack entries the label had not counted.
func rowValueCount(it settingItem) int {
	if len(it.entries) > 0 {
		return len(it.entries)
	}
	return len(it.list)
}

// rowValue returns the idx'th value cell of a row, as text plus an optional
// trailing badge (see listEntry).
func rowValue(it settingItem, idx int) listEntry {
	if it.kind != settingList {
		return listEntry{text: maybeDisplayPath(it, it.value)}
	}
	if idx < len(it.entries) {
		return it.entries[idx]
	}
	if idx < len(it.list) {
		return listEntry{text: maybeDisplayPath(it, it.list[idx])}
	}
	return listEntry{text: "(none)"}
}

// maybeDisplayPath contracts a value only on rows that hold a path, so an
// ordinary setting that happens to contain a slash is left exactly as stored.
func maybeDisplayPath(it settingItem, s string) string {
	if !it.path {
		return s
	}
	return displayPath(s)
}

// rowValues is the value column as one or more plain lines, for width
// measurement: a list row yields one line per entry ("(none)" when empty),
// everything else a single line.
func rowValues(it settingItem) []string {
	n := rowValueCount(it)
	if it.kind != settingList {
		n = 1
	}
	if n == 0 {
		return []string{"(none)"}
	}
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, rowValue(it, i).display())
	}
	return out
}

// settingsColumnWidths returns the label and value column widths (including a
// trailing gap) so every row aligns regardless of label/value lengths.
func settingsColumnWidths(items []settingItem) (labelW, valueW int) {
	for _, it := range items {
		if w := lipgloss.Width(rowLabel(it)); w > labelW {
			labelW = w
		}
		for _, v := range rowValues(it) {
			if w := lipgloss.Width(v); w > valueW {
				valueW = w
			}
		}
	}
	return labelW + 3, valueW + 4
}

// renderSettingsSection renders the full-width Settings section (section 4): a
// grouped, scrollable settings list with a pinned footer bar. Overlays (help,
// section menu, theme picker) are composited on top.
func (m Model) renderSettingsSection() string {
	isOverlay := m.showHelp || m.sectionMenuOpen || m.showThemePicker || m.settingsListEditor != nil || m.settingsTextEditor != nil
	bodyHeight := max(m.height-6, 1)
	innerW := m.width - 2
	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}

	var sb strings.Builder
	logoLines := strings.Split(LogoText, "\n")
	logoW := lipgloss.Width(logoLines[0])
	menu := m.renderTopMenu(m.width-logoW, isOverlay)
	for i := range 3 {
		sb.WriteString(menu[i])
		sb.WriteString(sepStyle.Render(logoLines[i]))
		sb.WriteString("\n")
	}
	// Connect the scope/rows divider to the top border with a ┬ junction (top
	// only — the footer status bars span the full width below the divider).
	topFill := []rune(strings.Repeat("─", innerW))
	if sw := m.settingsScopeWidth(); sw < len(topFill) {
		topFill[sw] = '┬'
	}
	sb.WriteString(sepStyle.Render(overlayLogoBottom("╭"+string(topFill)+"╮", m.width)))
	sb.WriteString("\n")

	sb.WriteString(m.renderSettingsBody(innerW, bodyHeight, isOverlay))

	sb.WriteString(sepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯"))
	sb.WriteString("\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	final := m.applyOverlays(sb.String())
	if m.settingsListEditor != nil {
		final = m.settingsListEditor.renderModal(final, m.width, m.height)
	}
	if m.settingsTextEditor != nil {
		final = m.settingsTextEditor.renderModal(final, m.width, m.height)
	}
	return final
}

// settingsScopeWidth is the width of the left Scope column: the default (longest
// scope label + 4) shifted by the user's [ / ] adjustment, clamped to bounds.
func (m Model) settingsScopeWidth() int {
	base, lo, hi := m.settingsScopeBounds()
	return clampWidth(base+m.settingsScopeWDelta, lo, hi)
}

// settingsScopeBounds returns the default scope-column width and the min/max it
// can be resized to. The default is the widest scope label plus 4 columns of
// breathing room, but capped at 30% of the screen so long workspace names do not
// crowd out the settings pane; [ / ] can still widen it up to hi.
func (m Model) settingsScopeBounds() (base, lo, hi int) {
	lo = 10
	hi = max(m.width-20, lo)
	base = min(scopeLabelWidth(m.settingsScopes)+5, max(m.width*30/100, lo))
	return base, lo, hi
}

// adjustScopeWidth widens (dir>0) or narrows (dir<0) the Scope column by storing
// the delta from the default, clamped so the resulting width stays in bounds.
func (m Model) adjustScopeWidth(dir int) Model {
	base, lo, hi := m.settingsScopeBounds()
	w := clampWidth(base+m.settingsScopeWDelta+dir*2, lo, hi)
	m.settingsScopeWDelta = w - base
	return m
}

// scopeLabelWidth returns the display width of the widest scope label (including
// the "Scope" title), used to size the column to its content.
func scopeLabelWidth(scopes []settingScope) int {
	maxW := lipgloss.Width("Scope")
	for _, sc := range scopes {
		if n := lipgloss.Width(sc.label); n > maxW {
			maxW = n
		}
	}
	return maxW
}

// renderSettingsBody renders the two-pane Settings layout: the Scope column
// (Global + workspaces) on the left, the settings rows for the selected scope on
// the right, and the pinned footer (hint + status/help) spanning both below.
func (m Model) renderSettingsBody(innerW, bodyHeight int, isOverlay bool) string {
	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}
	scrollH := max(bodyHeight-settingsFooterRows, 1)
	scopeW := m.settingsScopeWidth()
	rowsW := max(innerW-1-scopeW, 10)

	// Rows pane: a pinned 2-line header (tab bar + blank) above the scrollable
	// settings rows, so the tabs stay visible while scrolling. The Commands tab
	// renders its own two-pane view instead of the flat settingItem rows.
	contentLines := m.settingsRowsLines(rowsW)
	contentVisH := max(scrollH-settingsTabHeaderRows, 1)
	rowOff := clampOffset(m.settingsScroll, len(contentLines), contentVisH)
	contentBar := scrollbarCol(len(contentLines), contentVisH, rowOff, isOverlay)
	rowVis := append([]string{m.settingsTabBar(), ""}, contentLines[rowOff:]...)
	var rowBar []string
	if contentBar != nil {
		rowBar = append([]string{SepStyle.Render("│"), SepStyle.Render("│")}, contentBar...)
	}

	scopeLines := m.settingsScopeLines(scopeW)
	scopeOff := clampOffset(m.settingsScopeScroll, len(scopeLines), scrollH)
	scopeVis := scopeLines[scopeOff:]
	scopeBar := scrollbarCol(len(scopeLines), scrollH, scopeOff, isOverlay)

	var sb strings.Builder
	for i := range bodyHeight {
		if i >= scrollH {
			footerIdx := i - scrollH
			footer := m.settingsFooterRow(footerIdx, innerW, isOverlay)
			if footerIdx == 0 {
				// Extend the scope/rows divider one row into the blank separator so the
				// vertical line reaches the footer instead of stopping a row short.
				footer = settingsBlankDividerRow(scopeW, innerW, isOverlay || m.settingsScopeFocus)
			}
			sb.WriteString(sepStyle.Render("│"))
			sb.WriteString(footer)
			sb.WriteString(sepStyle.Render("│"))
			sb.WriteString("\n")
			continue
		}
		scope, _ := bodyColumn(scopeVis, scopeBar, i)
		row, rightEdge := bodyColumn(rowVis, rowBar, i)
		div := SepStyle.Render("┆")
		if scopeBar != nil && i < len(scopeBar) {
			div = scopeBar[i]
		}
		scopeCell := lipgloss.NewStyle().Width(scopeW).Render(ansi.Truncate(scope, scopeW-1, "…") + " ")
		rowCell := lipgloss.NewStyle().Width(rowsW).Render(row)
		if isOverlay {
			scopeCell = InactiveStyle.Render(ansi.Strip(scopeCell))
			rowCell = InactiveStyle.Render(ansi.Strip(rowCell))
			div = SepInactiveStyle.Render("┆")
		} else if m.settingsScopeFocus {
			// Scope column has focus — dim the rows pane so the active pane stands out.
			rowCell = InactiveStyle.Render(ansi.Strip(rowCell))
			div = SepInactiveStyle.Render("┆")
		}
		sb.WriteString(sepStyle.Render("│"))
		sb.WriteString(scopeCell)
		sb.WriteString(div)
		sb.WriteString(rowCell)
		sb.WriteString(rightEdge)
		sb.WriteString("\n")
	}
	return sb.String()
}

// settingsBlankDividerRow renders a blank separator row that still carries the
// scope/rows divider, so the vertical line extends one row below the last
// settings row to meet the footer (no gap). The divider dims when the rows pane
// is inactive (overlay or scope-focused), matching the body divider.
func settingsBlankDividerRow(scopeW, innerW int, dim bool) string {
	div := SepStyle.Render("┆")
	if dim {
		div = SepInactiveStyle.Render("┆")
	}
	left := lipgloss.NewStyle().Width(scopeW).Render("")
	right := lipgloss.NewStyle().Width(max(innerW-scopeW-1, 0)).Render("")
	return left + div + right
}

// clampOffset bounds a scroll offset to [0, total-visible].
func clampOffset(off, total, visible int) int {
	if maxOff := max(total-visible, 0); off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	return off
}

// settingsScopeLines renders the left Scope column: Global first (filled dot),
// then one row per active workspace. The selected scope drives which config the
// rows on the right edit.
func (m Model) settingsScopeLines(w int) []string {
	focused := m.settingsScopeFocus
	titleStyle := PanelHeaderFadedStyle
	if focused {
		titleStyle = PanelHeaderStyle
	}
	lines := []string{titleStyle.Render(" Scope"), ""}
	for i, sc := range m.settingsScopes {
		selected := i == m.settingsScopeCursor
		// One first-column marker: the cursor (❯) when selected, otherwise a
		// muted bullet (∙).
		marker := "∙"
		if selected {
			marker = "❯"
		}
		label := sc.label
		avail := max(w-4, 4)
		if r := []rune(label); len(r) > avail {
			label = string(r[:avail-1]) + "…"
		}
		line := " " + marker + " " + label
		switch {
		case selected:
			lines = append(lines, SelectedStyle.Render(line))
		case focused:
			lines = append(lines, ItemStyle.Render(line))
		default:
			lines = append(lines, FadedStyle.Render(line))
		}
	}
	return lines
}
