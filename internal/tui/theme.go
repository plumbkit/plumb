package tui

import (
	"image/color"
	"sort"

	"charm.land/lipgloss/v2"
)

// Theme defines the complete set of foreground colour tokens used by the TUI.
// All fields are color.Color values — either constructed via lipgloss.Color()
// (which accepts a hex string like "#AABBCC" or a terminal-palette index like
// "12") or any other color.Color implementation.
//
// No background colours are ever set: the terminal's own background colour is
// always preserved.
//
// Semantic naming convention: every field answers "what is this colour FOR?"
// — never "what does it look like?". This keeps themes portable across
// palettes that may use completely different hues for the same role.
type Theme struct {
	// Accent is used for the app title, selected-item text, and cursor arrow
	// (▸). It is the primary "this is active/important" signal.
	Accent color.Color

	// Border is used for all box-drawing characters: panel frames, section
	// separators (┆), horizontal rules (──).
	Border color.Color

	// PanelTitle is used for the text labels embedded in panel borders
	// ("Sessions", "Call Detail", etc.).
	PanelTitle color.Color

	// TextPrimary is for high-importance foreground text: detail row values,
	// table data the user reads most often.
	TextPrimary color.Color

	// TextSecondary is for detail panel values — clearly readable but not
	// the first thing the eye lands on.
	TextSecondary color.Color

	// TextMuted is for low-priority secondary text: durations, timestamps in
	// unselected rows, table "when" / "dur" column data.
	TextMuted color.Color

	// TextDim is used to fade the *background* panel content when a popup
	// overlays it — should be very dark, nearly invisible.
	TextDim color.Color

	// TextUnfocused is for text in an active (foreground) panel that has lost
	// focus to its sibling — e.g. the Timestamp list when Call Detail is
	// focused. Must be clearly readable, just less prominent than TextMuted.
	TextUnfocused color.Color

	// TextHint is for column headers and the global key-hint bar. Sits
	// between TextMuted and TextPrimary: clearly readable but not dominant.
	TextHint color.Color

	// Key is for the key column in detail rows ("Tool", "Status", …).
	Key color.Color

	// ItemText is for unselected list items in the session panel.
	ItemText color.Color

	// Success is the colour for positive status indicators: ✓, ●, "success".
	Success color.Color

	// Warning is the colour for error/failure indicators: ✗, error counts.
	Warning color.Color

	// ScrollThumb is the colour of the active scrollbar thumb (┃).
	ScrollThumb color.Color

	// ScrollTrack is the colour of the inactive scrollbar track (╎).
	ScrollTrack color.Color
}

// Nordico is the default theme — a TUI adaptation of the Nordico colour
// palette (github.com/madprops/Nordico), itself a customised Nord variant.
// Hex values are taken from the Nord/Nordico palette; terminal-palette indices
// ("2", "3", "8" …) delegate to the user's terminal theme so that anyone
// running a Nord terminal profile gets perfect harmony automatically.
var Nordico = Theme{
	Accent:        lipgloss.Color("12"),      // bright blue  — Nord9 / #88C0D0 family
	Border:        lipgloss.Color("#4C566A"), // Nord3 — clearly visible border, not dominant
	PanelTitle:    lipgloss.Color("15"),      // bright white — Nord6 #ECEFF4
	TextPrimary:   lipgloss.Color("15"),      // bright white
	TextSecondary: lipgloss.Color("#C8D8E8"), // light blue-white — Nord4 tint
	TextMuted:     lipgloss.Color("#AABBCC"), // mid blue-gray — between Nord3 and Nord4
	TextDim:       lipgloss.Color("#2E3440"), // Nord0 — background panel fade, nearly invisible
	TextUnfocused: lipgloss.Color("#AABBCC"), // same as TextMuted — readable, clearly not background
	TextHint:      lipgloss.Color("#7B8EA6"), // mid-tone — Nord3/Nord4 midpoint
	Key:           lipgloss.Color("6"),       // cyan         — Nord8 #88C0D0
	ItemText:      lipgloss.Color("7"),       // light gray   — Nord4
	Success:       lipgloss.Color("2"),       // green        — Nord14 #A3BE8C
	Warning:       lipgloss.Color("3"),       // yellow       — Nord13 #EBCB8B
	ScrollThumb:   lipgloss.Color("12"),      // bright blue — same as Accent
	ScrollTrack:   lipgloss.Color("#4C566A"), // same as Border — track blends with border
}

// ActiveTheme is the theme used by RebuildStyles. Set it before calling
// tui.Run() to select a different built-in theme. Defaults to Nordico.
var ActiveTheme = Nordico

// AvailableThemes is the catalogue of built-in themes, keyed by the name
// accepted by the --theme flag.
var AvailableThemes = map[string]Theme{
	"nordico": Nordico,
}

// ThemeNames returns the sorted list of available theme names, suitable for
// use in flag help text.
func ThemeNames() []string {
	names := make([]string, 0, len(AvailableThemes))
	for k := range AvailableThemes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
