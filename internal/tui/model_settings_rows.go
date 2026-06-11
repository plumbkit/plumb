package tui

// model_settings_rows.go — per-row rendering for the Settings section: the
// display-line builder, individual row / header / continuation rendering, the
// reload-tier and override markers, the controls, and the footer status bar.

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
	style := DetailStyle
	if wsScope && !it.overridden {
		style = FadedStyle
	}
	if it.lspMissing {
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	}
	entry := ""
	if idx < len(it.list) {
		entry = truncate(it.list[idx], valueW-2)
	}
	return strings.Repeat(" ", labelW+3) + style.Render(entry)
}

// settingsHeaderDisplay renders a group header as the name followed by a faded
// dotted rule that fills to the right gap (1 space from each border).
func settingsHeaderDisplay(group string, innerW int, warn bool) string {
	marker := ""
	if warn { // an enabled LSP server in this group is not on PATH
		marker = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render("*")
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
	// Truncate two short of the column so an over-long value keeps a gap
	// before the control instead of running into it.
	value := fmt.Sprintf("%-*s", valueW, truncate(rowValues(it)[0], valueW-2))
	ctrl := settingControl(it)

	numeral, numeralPlain := reloadNumeral(it.key)
	// Workspace scope: a superscript marker after the tier numeral flags the row
	// as an override (⁴) or inherited (⁵).
	mark, markPlain := "", ""
	if wsScope {
		mark, markPlain = workspaceMark(it.overridden)
	}
	markers := numeralPlain + markPlain
	pad := strings.Repeat(" ", max(labelW-lipgloss.Width(label)-lipgloss.Width(markers), 0))

	// Dim inherited rows so workspace overrides stand out; flag a missing LSP
	// server's whole block in red.
	labelStyle, valueStyle := ItemStyle, DetailStyle
	if wsScope && !it.overridden {
		labelStyle, valueStyle = FadedStyle, FadedStyle
	}
	if it.lspMissing {
		red := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
		labelStyle, valueStyle = red, red
	}

	var core string
	if focused {
		// One SelectedStyle pass, so the markers take the selection colour.
		core = SelectedStyle.Render("❯ " + label + markers + pad + value + ctrl)
	} else {
		core = "  " + labelStyle.Render(label) + numeral + mark + pad + valueStyle.Render(value) + MutedStyle.Render(ctrl)
	}
	return " " + core
}

// reloadNumeral returns the coloured reload-tier numeral and its plain rune (the
// plain form is used in the focused row's single SelectedStyle render).
func reloadNumeral(key settingKey) (coloured, plain string) {
	switch reloadTierFor(key) {
	case reloadNextSession:
		return WarnStyle.Render("²"), "²"
	case reloadRestart:
		return RestartStyle.Render("³"), "³"
	default:
		return OkStyle.Render("¹"), "¹"
	}
}

// workspaceMark returns the coloured + plain superscript that flags a workspace
// row as an override (⁴, green) or inherited (⁵, muted), shown right after the
// reload-tier numeral on the label.
func workspaceMark(overridden bool) (coloured, plain string) {
	if overridden {
		return OkStyle.Render("⁴"), "⁴"
	}
	return MutedStyle.Render("⁵"), "⁵"
}

// settingsFooterRow renders one of the three pinned footer rows: a blank
// separator (0), the key-hint bar (1), and the status bar (2).
func (m Model) settingsFooterRow(idx, innerW int, isOverlay bool) string {
	contentW := max(innerW-4, 0)
	switch idx {
	case 1:
		return statusBarLine(settingsHintContent(contentW, !m.currentScope().global), innerW, isOverlay)
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

// settingsHintContent builds the hint bar: a legend on the left (the reload
// tiers in Global scope, the inherit/override key in a workspace scope) and the
// navigation shortcuts (brighter keys) on the right.
func settingsHintContent(contentW int, wsScope bool) string {
	legend := settingsLegend(wsScope)
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
func settingsLegend(wsScope bool) string {
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
	return legend
}

// settingsStatusContent left-aligns the status message on the bar, padded with
// the background colour to the full content width.
func settingsStatusContent(text string, contentW int) string {
	if lipgloss.Width(text) > contentW {
		text = truncate(text, contentW)
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
