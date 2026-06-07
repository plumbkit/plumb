package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// pickerLine is one rendered row of the theme picker: raw text plus the style
// to apply. A zero value renders as a blank padding line. When header is set,
// the text is rendered as a section name followed by a faded dotted rule.
type pickerLine struct {
	text   string
	style  lipgloss.Style
	header bool
}

// themeGroups splits the sorted theme names into dark and light buckets.
func themeGroups() (dark, light []string) {
	for _, n := range ThemeNames() {
		if isLightTheme(AvailableThemes[n]) {
			light = append(light, n)
		} else {
			dark = append(dark, n)
		}
	}
	return dark, light
}

// themePickerOrder is the cursor navigation order: dark themes first, then
// light, matching the grouped layout in renderThemePicker.
func themePickerOrder() []string {
	dark, light := themeGroups()
	return append(dark, light...)
}

// renderThemePicker draws the centred theme-picker modal over a dimmed
// background. Moving the cursor applies and saves the theme live, so the dimmed
// background re-renders in the highlighted theme — that is the preview.
func (m Model) renderThemePicker(bg string) string {
	dark, light := themeGroups()
	rows := m.themePickerRows(dark, light)

	contentW := lipgloss.Width("↑↓ apply + save  ·  esc close")
	for _, ln := range rows {
		if !ln.header {
			if w := lipgloss.Width(ln.text); w > contentW {
				contentW = w
			}
		}
	}
	const pad = 3 // blank columns on each side of the row content
	innerW := contentW + pad*2

	var b strings.Builder
	b.WriteString(themePickerTop(innerW) + "\n")
	for _, ln := range rows {
		b.WriteString(themePickerBodyLine(ln, innerW, pad, contentW) + "\n")
	}
	// Footer status bar (same treatment as the Settings status bar).
	b.WriteString(SepStyle.Render("│") + statusBarLine(themePickerFooterContent(innerW-4), innerW, false) + SepStyle.Render("│") + "\n")
	b.WriteString(SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯"))

	return spliceOverlay(bg, b.String(), m.width, m.height)
}

// themePickerRows builds the grouped body of the picker: a blank line at the
// top, a Dark and a Light section (dotted header + rows), and a blank line
// before the footer (rendered separately).
func (m Model) themePickerRows(dark, light []string) []pickerLine {
	var lines []pickerLine
	blank := func() { lines = append(lines, pickerLine{}) }
	flat := 0
	group := func(title string, names []string) {
		lines = append(lines, pickerLine{text: title, header: true})
		for _, n := range names {
			sel := flat == m.themePickerCursor
			st := ItemStyle
			if sel {
				st = SelectedStyle
			}
			lines = append(lines, pickerLine{text: themePickerRow(n, sel), style: st})
			flat++
		}
	}

	blank()
	if len(dark) > 0 {
		group("Dark", dark)
	}
	if len(light) > 0 {
		if len(dark) > 0 {
			blank()
		}
		group("Light", light)
	}
	blank()
	return lines
}

// themePickerRow formats one theme row: a cursor (❯) when focused and a ✓ after
// the name when it is the active theme.
func themePickerRow(name string, cursor bool) string {
	c := "  "
	if cursor {
		c = "❯ "
	}
	row := c + name
	if name == ActiveThemeName {
		row += " ✓"
	}
	return row
}

func themePickerTop(innerW int) string {
	title := " Theme "
	dashes := max(innerW-1-lipgloss.Width(title), 0)
	return SepStyle.Render("╭─") + PanelHeaderStyle.Render(title) + SepStyle.Render(strings.Repeat("─", dashes)+"╮")
}

func themePickerBodyLine(ln pickerLine, innerW, pad, contentW int) string {
	var styled string
	var vis int
	if ln.header {
		dots := max(contentW-lipgloss.Width(ln.text)-1, 0)
		styled = PanelHeaderFadedStyle.Render(ln.text) + " " + SepStyle.Render(strings.Repeat("╌", dots))
		vis = lipgloss.Width(ln.text) + 1 + dots
	} else {
		styled = ln.style.Render(ln.text)
		vis = lipgloss.Width(ln.text)
	}
	rpad := max(innerW-pad-vis, 0)
	body := strings.Repeat(" ", pad) + styled + strings.Repeat(" ", rpad)
	return SepStyle.Render("│") + body + SepStyle.Render("│")
}

// themePickerFooterContent centres the apply/close hint (brighter keys) within
// the footer content width.
func themePickerFooterContent(contentW int) string {
	hint := SettingsBarKeyStyle.Render("↑↓") + SettingsBarStyle.Render(" apply + save  ·  ") +
		SettingsBarKeyStyle.Render("esc") + SettingsBarStyle.Render(" close")
	w := lipgloss.Width("↑↓ apply + save  ·  esc close")
	left := max((contentW-w)/2, 0)
	right := max(contentW-w-left, 0)
	return SettingsBarStyle.Render(strings.Repeat(" ", left)) + hint + SettingsBarStyle.Render(strings.Repeat(" ", right))
}

// isLightTheme heuristically identifies a light theme by checking whether
// the SelectionBackground is brighter than a mid-point luminance threshold.
// The result is used only for the "dark / light" badge in the theme list.
func isLightTheme(th Theme) bool {
	if th.SelectionBackground == nil {
		return false
	}
	r, g, b, _ := th.SelectionBackground.RGBA()
	// RGBA() returns 16-bit values (0–65535); scale to 0–255.
	lum := (float64(r>>8)*299 + float64(g>>8)*587 + float64(b>>8)*114) / 1000
	return lum > 127
}
