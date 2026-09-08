package tui

// model_settings_rows.go — per-row rendering for the Settings section: the
// display-line builder, individual row / header / continuation rendering, the
// reload-tier and override markers, the controls, and the footer status bar.

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// settingsDisplayLines renders the scrollable logical lines to display strings
// for the rows pane (width rowsW). In a workspace scope each row shows whether
// it is a workspace override or inherited; in Global scope it shows the reload
// tier.
func (m Model) settingsDisplayLines(rowsW int) []string {
	if len(m.settingsItems) == 0 {
		msg := "  (no settings in this tab)"
		if m.settingsTab == settingsTabLSP {
			msg = "  (no language servers configured — add [lsp.<lang>] to config)"
		}
		return []string{MutedStyle.Render(msg)}
	}
	labelW, valueW := settingsColumnWidths(m.settingsItems)
	valueW = clampSettingsValueW(valueW, labelW, m.settingsItems, rowsW)
	logical := settingsLogicalLines(m.settingsItems)
	wsScope := !m.currentScope().global
	missing := map[string]bool{} // language groups with an enabled-but-missing server
	for _, it := range m.settingsItems {
		if it.lspMissing {
			missing[it.group] = true
		}
	}
	out := make([]string, len(logical))
	for i, ln := range logical {
		switch ln.kind {
		case slHeader:
			out[i] = settingsHeaderDisplay(ln.group, rowsW, missing[ln.group])
		case slRow:
			it := m.settingsItems[ln.item]
			if ln.cont > 0 {
				out[i] = settingsContLine(it, ln.cont, labelW, valueW, wsScope)
			} else {
				out[i] = settingsRowDisplay(it, ln.item == m.settingsCursor, wsScope, labelW, valueW)
			}
		default:
			out[i] = ""
		}
	}
	return out
}

// clampSettingsValueW caps the value column so the widest row (value plus its
// control) still fits the pane — an over-wide row would be wrapped by the
// fixed-width cell render and corrupt the layout. Values are ellipsis-truncated
// to the capped width instead.
func clampSettingsValueW(valueW, labelW int, items []settingItem, rowsW int) int {
	maxCtrl := 0
	for _, it := range items {
		if w := lipgloss.Width(settingControl(it)); w > maxCtrl {
			maxCtrl = w
		}
	}
	// 3 = the leading space plus the 2-cell cursor column.
	if maxW := rowsW - labelW - maxCtrl - 3; valueW > maxW {
		return max(maxW, 8)
	}
	return valueW
}

// settingsContLine renders a list-entry continuation line, padded so the entry
// aligns under the value column of the row above. Missing-LSP rows render red.
func settingsContLine(it settingItem, idx, labelW, valueW int, wsScope bool) string {
	_, style := rowScopeStyles(it, wsScope)
	if it.lspMissing {
		style = MissingStyle
	}
	cell := ""
	if idx < rowValueCount(it) {
		cell = renderValueCell(rowValue(it, idx), valueW, style)
	}
	return strings.Repeat(" ", labelW+3) + cell
}

// renderValueCell renders one value cell: the text truncated two short of the
// column (so an over-long value keeps a gap before the control instead of
// running into it), then the badge, then the whole thing in the entry's own
// style when it has one.
//
// Truncating BEFORE appending the badge is the point. textfmt.Ellipsis budgets
// runes, and a badge folded into the text can be cut mid-glyph — ⚠️ is a base
// character plus a variation selector, so losing the selector silently prints a
// different symbol. Reserving the badge's display width up front means a narrow
// pane loses detail and never the signal.
func renderValueCell(e listEntry, valueW int, fallback lipgloss.Style) string {
	style := fallback
	if e.styled {
		style = e.style
	}
	return style.Render(valueCellText(e, valueW))
}

// valueCellText is renderValueCell's unstyled half: the text a cell shows, cut
// to the column and carrying its badge.
//
// Separate because the FOCUSED row cannot use the styled form — it renders
// label, value and control in one SelectedStyle pass — and must still be cut to
// the same width. Sharing one function is what keeps the two from disagreeing:
// while the focused branch did its own thing it quietly lost the truncation, and
// a long value ran past its column into the control, wrapping the row and
// corrupting the pane's borders — the exact failure clampSettingsValueW exists
// to prevent.
func valueCellText(e listEntry, valueW int) string {
	budget := valueW - 2
	if e.badge != "" {
		budget -= lipgloss.Width(e.badge) + 1 // the badge plus its leading space
	}
	text := textfmt.Ellipsis(e.text, max(budget, 1))
	if e.badge != "" {
		text += " " + e.badge
	}
	return text
}

// padValueCell right-pads a rendered cell to the column width.
//
// Measured with lipgloss.Width rather than fmt's "%-*s": fmt counts RUNES, and a
// badge glyph is one rune occupying two terminal columns, so a padded emoji cell
// comes out a column short and drags the control left on that row alone.
// lipgloss.Width measures display columns and ignores ANSI, so one call is
// correct for both the styled and the plain form.
func padValueCell(cell string, valueW int) string {
	return cell + strings.Repeat(" ", max(valueW-lipgloss.Width(cell), 0))
}

// settingsHeaderDisplay renders a group header as the name followed by a faded
// dotted rule that fills to the right gap (1 space from each border).
func settingsHeaderDisplay(group string, innerW int, warn bool) string {
	marker := ""
	if warn { // an enabled LSP server in this group is not on PATH
		marker = MissingStyle.Render("*")
	}
	used := 1 + lipgloss.Width(group) + lipgloss.Width(marker) + 1 // " " + name + marker + " "
	dots := max(innerW-1-used, 0)
	return " " + PanelHeaderFadedStyle.Render(group) + marker + " " + SepStyle.Render(strings.Repeat("╌", dots))
}

// settingsRowDisplay renders one aligned settings row: 1-space gap, cursor,
// fixed-width label and value columns, the control. In Global scope the
// reload-tier numeral sits right after the setting name (¹ live / ² next session
// / ³ restart — see settingsHintContent for the legend); in a workspace scope a
// superscript ⁴/⁵ after the numeral marks override vs inherited.
func settingsRowDisplay(it settingItem, focused, wsScope bool, labelW, valueW int) string {
	label := rowLabel(it)
	ctrl := settingControl(it)

	numeral, numeralPlain := reloadNumeral(it.key)
	// Workspace scope: a superscript marker after the tier numeral flags the row
	// as an override (⁴), inherited (⁵), or set-but-ignored (⁶).
	mark, markPlain := "", ""
	if wsScope {
		mark, markPlain = workspaceMark(it)
	}
	markers := numeralPlain + markPlain
	pad := strings.Repeat(" ", max(labelW-lipgloss.Width(label)-lipgloss.Width(markers), 0))

	labelStyle, valueStyle := rowScopeStyles(it, wsScope)
	if it.lspMissing {
		labelStyle, valueStyle = MissingStyle, MissingStyle
	}

	var core string
	if focused {
		// One SelectedStyle pass, so the markers take the selection colour. The
		// entry's own colour is deliberately dropped here: a per-entry red inside
		// a selection highlight is unreadable on several themes, and the row is
		// the one the status bar is already describing in words.
		plain := padValueCell(valueCellText(rowValue(it, 0), valueW), valueW)
		core = SelectedStyle.Render("❯ " + label + markers + pad + plain + ctrl)
	} else {
		padded := padValueCell(renderValueCell(rowValue(it, 0), valueW, valueStyle), valueW)
		core = "  " + labelStyle.Render(label) + numeral + mark + pad + padded + MutedStyle.Render(ctrl)
	}
	return " " + core
}

// reloadNumeral returns the coloured reload-tier numeral and its plain rune (the
// plain form is used in the focused row's single SelectedStyle render).
func reloadNumeral(key settingKey) (coloured, plain string) {
	switch reloadTierFor(key) {
	case config.ReloadNextSession:
		return WarnStyle.Render("²"), "²"
	case config.ReloadRestart:
		return RestartStyle.Render("³"), "³"
	default:
		return OkStyle.Render("¹"), "¹"
	}
}

// workspaceMark returns the coloured + plain superscript that flags a workspace
// row as an override (⁴, green), inherited (⁵, muted), or set in the project
// config but ignored because the workspace is not trusted (⁶, warning). It sits
// right after the reload-tier numeral on the label.
//
// ⁶ is the whole point of the three-state scheme: without it a
// capability-granting key the project sets would render as ⁵ inherited, which is
// true of the VALUE and a lie about the FILE — leaving the user with a setting
// they wrote, cannot see, and cannot explain.
func workspaceMark(it settingItem) (coloured, plain string) {
	switch {
	case it.notInEffect:
		return WarnStyle.Render("⁶"), "⁶"
	case it.overridden:
		return OkStyle.Render("⁴"), "⁴"
	default:
		return MutedStyle.Render("⁵"), "⁵"
	}
}

// rowScopeStyles picks the label and value styles for a workspace row: inherited
// rows are dimmed so real overrides stand out, and a set-but-ignored row takes
// the warning colour so it reads as "look at me", not as background.
func rowScopeStyles(it settingItem, wsScope bool) (labelStyle, valueStyle lipgloss.Style) {
	switch {
	case !wsScope:
		return ItemStyle, DetailStyle
	case it.notInEffect:
		return WarnStyle, WarnStyle
	case it.overridden:
		return ItemStyle, DetailStyle
	default:
		return FadedStyle, FadedStyle
	}
}

// settingsFooterRow renders one of the three pinned footer rows: a blank
// separator (0), the key-hint bar (1), and the status bar (2).
func (m Model) settingsFooterRow(idx, innerW int, isOverlay bool) string {
	contentW := max(innerW-4, 0)
	switch idx {
	case 1:
		return statusBarLine(settingsHintContent(contentW, !m.currentScope().global, m.hasNotInEffectRow()), innerW, isOverlay)
	case 2:
		return statusBarLine(settingsStatusContent(m.settingsStatusOrHelp(), contentW), innerW, isOverlay)
	default:
		return lipgloss.NewStyle().Width(innerW).Render("")
	}
}

// settingsStatusOrHelp returns the transient action status when one is set,
// otherwise the focused row's one-line help — so the second status-bar line
// describes the highlighted setting whenever the user is just navigating.
func (m Model) settingsStatusOrHelp() string {
	if m.settingsStatus != "" {
		return m.settingsStatus
	}
	if m.settingsCursor >= 0 && m.settingsCursor < len(m.settingsItems) {
		return m.settingsItems[m.settingsCursor].help
	}
	return ""
}

// statusBarLine frames footer content on a subtle background bar: a 1-space
// plain gap from each border, then the background — within which the content is
// inset one further space on each side, so text begins one column into the
// background. content must already be exactly innerW-4 wide and styled.
func statusBarLine(content string, innerW int, isOverlay bool) string {
	if isOverlay {
		return lipgloss.NewStyle().Width(innerW).Render("  " + ansi.Strip(content))
	}
	return " " + SettingsBarStyle.Render(" ") + content + SettingsBarStyle.Render(" ") + " "
}

// hasNotInEffectRow reports whether any visible row is set in the project config
// but ignored, so the legend only spends its scarce width explaining ⁶ when a ⁶
// is actually on screen.
func (m Model) hasNotInEffectRow() bool {
	for _, it := range m.settingsItems {
		if it.notInEffect {
			return true
		}
	}
	return false
}

// settingsHintContent builds the hint bar: a legend on the left (the reload
// tiers in Global scope, the inherit/override key in a workspace scope) and the
// navigation shortcuts (brighter keys) on the right.
func settingsHintContent(contentW int, wsScope, untrusted bool) string {
	legend := settingsLegend(wsScope, untrusted)
	shortcut := SettingsBarKeyStyle.Render("↑↓") + SettingsBarStyle.Render(" move  ·  ") +
		SettingsBarKeyStyle.Render("←→") + SettingsBarStyle.Render(" change  ·  ") +
		SettingsBarKeyStyle.Render("tab") + SettingsBarStyle.Render(" panes  ·  ") +
		SettingsBarKeyStyle.Render("[ ]") + SettingsBarStyle.Render(" width")
	shortcutW := lipgloss.Width("↑↓ move  ·  ←→ change  ·  tab panes  ·  [ ] width")
	gap := max(contentW-lipgloss.Width(legend)-shortcutW, 1)
	return legend + SettingsBarStyle.Render(strings.Repeat(" ", gap)) + shortcut
}

// settingsLegend renders the left-hand legend on the status bar. Global scope
// explains the reload-tier numerals with matching colours (¹ green, ² yellow,
// ³ purple); a workspace scope explains the override/inherit marks. All segments
// carry the bar background.
func settingsLegend(wsScope, untrusted bool) string {
	ok := SettingsBarStyle.Foreground(ActiveTheme.Success)
	warn := SettingsBarStyle.Foreground(ActiveTheme.Warning)
	restart := SettingsBarStyle.Foreground(lipgloss.Color("#9D7CD8"))
	muted := SettingsBarStyle.Foreground(ActiveTheme.TextMuted)
	legend := ok.Render("¹") + SettingsBarStyle.Render(" immediate  ·  ") +
		warn.Render("²") + SettingsBarStyle.Render(" new sessions  ·  ") +
		restart.Render("³") + SettingsBarStyle.Render(" daemon restart")
	if wsScope {
		legend += SettingsBarStyle.Render("  ·  ") +
			ok.Render("⁴") + SettingsBarStyle.Render(" override  ·  ") +
			muted.Render("⁵") + SettingsBarStyle.Render(" inherited")
	}
	if wsScope && untrusted {
		// Deliberately generic. A ⁶ has two possible causes — an untrusted
		// workspace, and a key no consumer reads from a project file — and this
		// legend has room for a key, not a manual. Naming only `plumb trust` here
		// would send a user with a global-only row to grant a permission that
		// changes nothing; the per-row remedy is on the status line beneath it and
		// in the message the edit itself returns.
		legend += SettingsBarStyle.Render("  ·  ") +
			warn.Render("⁶") + SettingsBarStyle.Render(" set here, not in effect")
	}
	return legend
}

// settingsStatusContent left-aligns the status message on the bar, padded with
// the background colour to the full content width.
func settingsStatusContent(text string, contentW int) string {
	if lipgloss.Width(text) > contentW {
		text = textfmt.Ellipsis(text, contentW)
	}
	pad := max(contentW-lipgloss.Width(text), 0)
	return SettingsBarMsgStyle.Render(text) + SettingsBarStyle.Render(strings.Repeat(" ", pad))
}

// settingControl renders the interactive control affordance for a row. Cycle
// rows expose their full option set so the choices are discoverable; the
// current value lives in the row's value column.
func settingControl(it settingItem) string {
	switch it.kind {
	case settingPopup:
		return "›"
	case settingToggle:
		return "[ " + it.value + " ]"
	case settingCycle:
		return "‹ " + strings.Join(it.options, "·") + " ›"
	case settingNumber:
		return "‹ -/+ ›"
	case settingList, settingText:
		return "‹ edit ›"
	default:
		return ""
	}
}
