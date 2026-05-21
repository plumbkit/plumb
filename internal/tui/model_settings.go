package tui

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// settingsLeftLines renders the theme-picker list for the Settings section.
// One row per available theme; ❯ marks the current cursor, ✓ marks the saved
// (confirmed) theme.
func (m Model) settingsLeftLines() []string {
	lf := m.focusPanel == focusSessions || m.focusPanel == focusDetails
	titleStyle := PanelHeaderStyle
	if !lf {
		titleStyle = PanelHeaderFadedStyle
	}

	lines := []string{titleStyle.Render(" Themes"), ""}
	names := ThemeNames()
	for i, name := range names {
		cursor := "  "
		if i == m.themePickerCursor {
			cursor = "❯ "
		}
		saved := "  "
		if name == ActiveThemeName {
			saved = "✓ "
		}

		th := AvailableThemes[name]
		kind := "dark"
		if isLightTheme(th) {
			kind = "light"
		}

		label := fmt.Sprintf("%s%s%-16s (%s)", cursor, saved, name, kind)
		if i == m.themePickerCursor {
			lines = append(lines, SelectedStyle.Render(label))
		} else {
			lines = append(lines, ItemStyle.Render(label))
		}
	}
	lines = append(lines, "")
	lines = append(lines, HintStyle.Render("  ↑↓/jk  select  ·  enter  save  ·  esc  revert"))
	return lines
}

// settingsRightLines renders the colour-swatch preview for the currently
// highlighted theme.
func (m Model) settingsRightLines(rw int) []string {
	names := ThemeNames()
	if len(names) == 0 {
		return []string{MutedStyle.Render("No themes available.")}
	}
	if m.themePickerCursor < 0 || m.themePickerCursor >= len(names) {
		return []string{}
	}

	name := names[m.themePickerCursor]
	th := AvailableThemes[name]

	kind := "dark theme"
	if isLightTheme(th) {
		kind = "light theme"
	}

	saved := ""
	if name == ActiveThemeName {
		saved = "  " + OkStyle.Render("✓ saved")
	}

	lines := []string{
		PanelHeaderStyle.Render(fmt.Sprintf(" %s", name)) + MutedStyle.Render(fmt.Sprintf("  %s", kind)) + saved,
		"",
	}

	// Colour swatches
	swatches := []struct {
		label string
		c     color.Color
	}{
		{"Accent", th.Accent},
		{"Border", th.Border},
		{"TextPrimary", th.TextPrimary},
		{"TextSecondary", th.TextSecondary},
		{"TextMuted", th.TextMuted},
		{"TextFaded", th.TextFaded},
		{"TextHint", th.TextHint},
		{"Key", th.Key},
		{"ItemText", th.ItemText},
		{"Success", th.Success},
		{"Warning", th.Warning},
		{"SelectionBackground", th.SelectionBackground},
		{"ContrastText", th.ContrastText},
		{"ScrollThumb", th.ScrollThumb},
	}

	const (
		swatchW = 2 // "██"
		labelW  = 20
		colsGap = 4
	)
	colW := swatchW + 1 + labelW
	cols := max((rw-2)/(colW+colsGap), 1)

	for i := 0; i < len(swatches); i += cols {
		var row strings.Builder
		row.WriteString("  ")
		for c := range cols {
			idx := i + c
			if idx >= len(swatches) {
				break
			}
			if c > 0 {
				row.WriteString(strings.Repeat(" ", colsGap))
			}
			sw := swatches[idx]
			swatch := lipgloss.NewStyle().Foreground(sw.c).Render("██")
			lbl := MutedStyle.Render(sw.label)
			fmt.Fprintf(&row, "%s %s", swatch, lbl)
		}
		lines = append(lines, row.String())
	}

	lines = append(lines, "")

	// Mini example row showing selection and item styles in this theme.
	sel := lipgloss.NewStyle().Bold(true).Foreground(th.Accent).Render("❯ selected-item")
	itm := lipgloss.NewStyle().Foreground(th.ItemText).Render("  unselected-item")
	muted := lipgloss.NewStyle().Foreground(th.TextMuted).Render("  muted text")

	lines = append(lines,
		HintStyle.Render("  Preview"),
		"  "+sel,
		"  "+itm,
		"  "+muted,
	)

	return lines
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
