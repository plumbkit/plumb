package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/golimpio/plumb/internal/config"
)

// settingKind classifies how a settings row is edited.
type settingKind int

const (
	settingPopup  settingKind = iota // enter opens a sub-popup (theme picker)
	settingCycle                     // ←→ cycles through a fixed option set
	settingToggle                    // enter/space flips on↔off
	settingNumber                    // ←→ adjusts a numeric value
)

// settingKey identifies which config field a settings row edits. The key
// handler switches on this to mutate settingsCfg and persist via config.Save.
type settingKey int

const (
	skTheme settingKey = iota
	skLogLevel
	skLogFormat
	skStrict
	skShowWriteDiff
	skRateLimit
	skTopology
	skQuality
	skGitWrites
	skGitDestructive
	skGitPush
	skCacheTTL
	skCacheMaxSize
	skLSPTimeout
	skAutoAttach
)

// settingItem is one selectable row on the Settings screen. Group headers are
// not items — they are derived from the group field during rendering.
type settingItem struct {
	group   string
	label   string
	kind    settingKind
	key     settingKey
	value   string   // formatted current value
	options []string // option set for settingCycle
	live    bool     // change takes effect immediately (no daemon restart)
	restart bool     // change applies only on next daemon start
}

var (
	logLevelOptions   = []string{"debug", "info", "warn", "error"}
	logFormatOptions  = []string{"text", "json"}
	cacheTTLOptions   = []string{"1m", "5m", "10m", "30m", "1h"}
	lspTimeoutOptions = []string{"0s", "10s", "30s", "1m", "2m"}
)

// durValue formats a duration as its matching preset string when one exists,
// so the value column and the cycle option set line up.
func durValue(d config.Duration, presets []string) string {
	for _, p := range presets {
		if pd, err := time.ParseDuration(p); err == nil && pd == d.Duration {
			return p
		}
	}
	return d.String()
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func rateLimitValue(n int) string {
	if n <= 0 {
		return "off"
	}
	return fmt.Sprintf("%d", n)
}

// buildSettingItems returns the curated, editable settings rows in display
// order. Theme reflects the live ActiveThemeName; everything else comes from
// the supplied config snapshot.
func buildSettingItems(cfg config.Config) []settingItem {
	return []settingItem{
		{group: "Appearance", label: "Theme", kind: settingPopup, key: skTheme, value: ActiveThemeName, live: true},
		{group: "Logging", label: "Log level", kind: settingCycle, key: skLogLevel, value: cfg.LogLevel, options: logLevelOptions, live: true},
		{group: "Logging", label: "Log format", kind: settingCycle, key: skLogFormat, value: cfg.LogFormat, options: logFormatOptions, restart: true},
		{group: "Editing", label: "Strict edits", kind: settingToggle, key: skStrict, value: onOff(cfg.Edits.Strict), restart: true},
		{group: "Editing", label: "Show write diff", kind: settingToggle, key: skShowWriteDiff, value: onOff(cfg.Edits.ShowWriteDiff), restart: true},
		{group: "Editing", label: "Rate limit / min", kind: settingNumber, key: skRateLimit, value: rateLimitValue(cfg.Edits.RateLimitPerMinute), restart: true},
		{group: "Indexing", label: "Topology", kind: settingToggle, key: skTopology, value: onOff(cfg.Topology.Enabled), restart: true},
		{group: "Indexing", label: "Quality analysis", kind: settingToggle, key: skQuality, value: onOff(cfg.Quality.Enabled), restart: true},
		{group: "Git", label: "git allow_writes", kind: settingToggle, key: skGitWrites, value: onOff(cfg.Git.AllowWrites), restart: true},
		{group: "Git", label: "git allow_destructive", kind: settingToggle, key: skGitDestructive, value: onOff(cfg.Git.AllowDestructive), restart: true},
		{group: "Git", label: "git allow_push", kind: settingToggle, key: skGitPush, value: onOff(cfg.Git.AllowPush), restart: true},
		{group: "Others", label: "cache ttl", kind: settingCycle, key: skCacheTTL, value: durValue(cfg.Cache.TTL, cacheTTLOptions), options: cacheTTLOptions, restart: true},
		{group: "Others", label: "cache max_size", kind: settingNumber, key: skCacheMaxSize, value: fmt.Sprintf("%d", cfg.Cache.MaxSize), restart: true},
		{group: "Others", label: "lsp_query timeout", kind: settingCycle, key: skLSPTimeout, value: durValue(cfg.LSPQuery.Timeout, lspTimeoutOptions), options: lspTimeoutOptions, restart: true},
		{group: "Others", label: "workspace auto_attach", kind: settingToggle, key: skAutoAttach, value: onOff(cfg.Workspace.AutoAttach), restart: true},
	}
}

// settingsFooterRows is the number of body rows reserved at the bottom for the
// pinned footer: a blank separator, the key-hint bar, and the status bar.
const settingsFooterRows = 3

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
}

// settingsLogicalLines describes the scrollable list: a blank line at the top,
// then each group as a header followed by its rows, with a blank line between
// groups.
func settingsLogicalLines(items []settingItem) []settingsLine {
	out := []settingsLine{{kind: slBlank}}
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
	}
	return out
}

// settingsColumnWidths returns the label and value column widths (including a
// trailing gap) so every row aligns regardless of label/value lengths.
func settingsColumnWidths(items []settingItem) (labelW, valueW int) {
	for _, it := range items {
		if w := lipgloss.Width(it.label); w > labelW {
			labelW = w
		}
		if w := lipgloss.Width(it.value); w > valueW {
			valueW = w
		}
	}
	return labelW + 3, valueW + 4
}

// renderSettingsSection renders the full-width Settings section (section 4): a
// grouped, scrollable settings list with a pinned footer bar. Overlays (help,
// section menu, theme picker) are composited on top.
func (m Model) renderSettingsSection() string {
	isOverlay := m.showHelp || m.sectionMenuOpen || m.showThemePicker
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
		sb.WriteString(menu[i] + sepStyle.Render(logoLines[i]) + "\n")
	}
	sb.WriteString(sepStyle.Render(overlayLogoBottom("╭"+strings.Repeat("─", innerW)+"╮", m.width)) + "\n")

	sb.WriteString(m.renderSettingsBody(innerW, bodyHeight, isOverlay))

	sb.WriteString(sepStyle.Render("╰"+strings.Repeat("─", innerW)+"╯") + "\n")
	sb.WriteString(m.renderMainStatusBar(isOverlay))

	final := sb.String()
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

// renderSettingsBody renders the scrollable list rows plus the pinned footer.
func (m Model) renderSettingsBody(innerW, bodyHeight int, isOverlay bool) string {
	sepStyle := SepStyle
	if isOverlay {
		sepStyle = SepInactiveStyle
	}
	scrollH := max(bodyHeight-settingsFooterRows, 1)

	lines := m.settingsDisplayLines(innerW)
	offset := m.settingsScroll
	if maxOff := max(len(lines)-scrollH, 0); offset > maxOff {
		offset = maxOff
	}
	if offset < 0 {
		offset = 0
	}
	visible := lines[offset:]
	rBar := scrollbarCol(len(lines), scrollH, offset, isOverlay)

	var sb strings.Builder
	for i := range bodyHeight {
		if i < scrollH {
			l := ""
			if i < len(visible) {
				l = visible[i]
			}
			rBarChar := sepStyle.Render("│")
			if rBar != nil && i < len(rBar) {
				rBarChar = rBar[i]
			}
			padded := lipgloss.NewStyle().Width(innerW).Render(l)
			if isOverlay {
				padded = InactiveStyle.Render(ansi.Strip(padded))
			}
			sb.WriteString(sepStyle.Render("│") + padded + rBarChar + "\n")
			continue
		}
		sb.WriteString(sepStyle.Render("│") + m.settingsFooterRow(i-scrollH, innerW, isOverlay) + sepStyle.Render("│") + "\n")
	}
	return sb.String()
}

// settingsDisplayLines renders the scrollable logical lines to display strings.
func (m Model) settingsDisplayLines(innerW int) []string {
	labelW, valueW := settingsColumnWidths(m.settingsItems)
	logical := settingsLogicalLines(m.settingsItems)
	out := make([]string, len(logical))
	for i, ln := range logical {
		switch ln.kind {
		case slHeader:
			out[i] = settingsHeaderDisplay(ln.group, innerW)
		case slRow:
			it := m.settingsItems[ln.item]
			out[i] = settingsRowDisplay(it, ln.item == m.settingsCursor, labelW, valueW)
		default:
			out[i] = ""
		}
	}
	return out
}

// settingsHeaderDisplay renders a group header as the name followed by a faded
// dotted rule that fills to the right gap (3 spaces from each border).
func settingsHeaderDisplay(group string, innerW int) string {
	used := 3 + lipgloss.Width(group) + 1 // "   " + name + " "
	dots := max(innerW-3-used, 0)
	return "   " + PanelHeaderFadedStyle.Render(group) + " " + SepStyle.Render(strings.Repeat("╌", dots))
}

// settingsRowDisplay renders one aligned settings row: 3-space gap, cursor,
// fixed-width label and value columns, the control, then live/restart marks.
func settingsRowDisplay(it settingItem, focused bool, labelW, valueW int) string {
	label := fmt.Sprintf("%-*s", labelW, it.label)
	value := fmt.Sprintf("%-*s", valueW, it.value)
	ctrl := settingControl(it)

	var core string
	if focused {
		core = SelectedStyle.Render("❯ " + label + value + ctrl)
	} else {
		core = "  " + ItemStyle.Render(label) + DetailStyle.Render(value) + MutedStyle.Render(ctrl)
	}
	out := "   " + core
	if it.live {
		out += " " + OkStyle.Render("live")
	}
	if it.restart {
		out += " " + HintStyle.Render("*")
	}
	return out
}

// settingsFooterRow renders one of the three pinned footer rows: a blank
// separator (0), the key-hint bar (1), and the status bar (2).
func (m Model) settingsFooterRow(idx, innerW int, isOverlay bool) string {
	switch idx {
	case 1:
		return settingsBar("↑↓ move · ←→ change · enter toggle/open · * applies on next daemon start", innerW, isOverlay)
	case 2:
		return settingsBar(m.settingsStatus, innerW, isOverlay)
	default:
		return lipgloss.NewStyle().Width(innerW).Render("")
	}
}

// settingsBar renders a footer line with a subtle background that sits one
// column in from each border (1-space gap left and right).
func settingsBar(text string, innerW int, isOverlay bool) string {
	if isOverlay {
		return lipgloss.NewStyle().Width(innerW).Render(" " + text)
	}
	bgW := max(innerW-2, 0)
	return " " + LogStatusStyle.Width(bgW).Render(text) + " "
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
	default:
		return ""
	}
}

// pickerLine is one rendered row of the theme picker: raw text plus the style
// to apply. A zero value renders as a blank padding line.
type pickerLine struct {
	text  string
	style lipgloss.Style
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
	lines := m.themePickerLines(dark, light)

	contentW := 0
	for _, ln := range lines {
		if w := lipgloss.Width(ln.text); w > contentW {
			contentW = w
		}
	}
	const pad = 3 // blank columns on each side of the content
	innerW := contentW + pad*2

	var b strings.Builder
	b.WriteString(themePickerTop(innerW) + "\n")
	for _, ln := range lines {
		b.WriteString(themePickerBodyLine(ln, innerW, pad) + "\n")
	}
	b.WriteString(SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯"))

	return spliceOverlay(bg, b.String(), m.width, m.height)
}

// themePickerLines builds the grouped body of the picker: a blank line top and
// bottom, a Dark and a Light section (header + rows), and the footer.
func (m Model) themePickerLines(dark, light []string) []pickerLine {
	var lines []pickerLine
	blank := func() { lines = append(lines, pickerLine{}) }
	flat := 0
	group := func(title string, names []string) {
		lines = append(lines, pickerLine{text: title, style: PanelHeaderFadedStyle})
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
	lines = append(lines, pickerLine{text: "↑↓ apply + save · esc close", style: HintStyle})
	blank()
	return lines
}

func themePickerRow(name string, cursor bool) string {
	c := " "
	if cursor {
		c = "❯"
	}
	s := " "
	if name == ActiveThemeName {
		s = "✓"
	}
	return c + " " + s + " " + name
}

func themePickerTop(innerW int) string {
	title := " Theme "
	dashes := max(innerW-1-lipgloss.Width(title), 0)
	return SepStyle.Render("╭─") + PanelHeaderStyle.Render(title) + SepStyle.Render(strings.Repeat("─", dashes)+"╮")
}

func themePickerBodyLine(ln pickerLine, innerW, pad int) string {
	rpad := max(innerW-pad-lipgloss.Width(ln.text), 0)
	body := strings.Repeat(" ", pad) + ln.style.Render(ln.text) + strings.Repeat(" ", rpad)
	return SepStyle.Render("│") + body + SepStyle.Render("│")
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
