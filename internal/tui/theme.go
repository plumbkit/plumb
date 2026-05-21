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

	// TextInactive is for text in a panel that is fully behind a popup overlay
	// — non-interactive, not the current context. #3E444F.
	TextInactive color.Color

	// TextFaded is for text in a panel that is active but has lost focus to
	// its sibling — still navigable, just not the current focus. #6C768A.
	TextFaded color.Color

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

	// SelectionBackground is the dark background for selected table rows.
	SelectionBackground color.Color

	// ContrastText is the foreground colour rendered on top of a coloured
	// background (accent-coloured selection row, tab badges). Use
	// lipgloss.Color("0") (black) for dark themes with bright accent colours;
	// lipgloss.Color("15") (white) for light themes where the accent is dark.
	ContrastText color.Color

	// ChromaStyle is the chroma syntax-highlighting style name used by
	// `plumb config show`. Must match a name in alecthomas/chroma/v2/styles.
	ChromaStyle string
}

// Nordico is the default theme — a TUI adaptation of the Nordico colour
// palette (github.com/madprops/Nordico), itself a customised Nord variant.
// Hex values are taken from the Nord/Nordico palette; terminal-palette indices
// ("2", "3", "8" …) delegate to the user's terminal theme so that anyone
// running a Nord terminal profile gets perfect harmony automatically.
var Nordico = Theme{
	Accent:              lipgloss.Color("12"),      // bright blue  — Nord9 / #88C0D0 family
	Border:              lipgloss.Color("#4C566A"), // Nord3 — clearly visible border, not dominant
	PanelTitle:          lipgloss.Color("15"),      // bright white — Nord6 #ECEFF4
	TextPrimary:         lipgloss.Color("15"),      // bright white
	TextSecondary:       lipgloss.Color("#C8D8E8"), // light blue-white — Nord4 tint
	TextMuted:           lipgloss.Color("#AABBCC"), // mid blue-gray — between Nord3 and Nord4
	TextInactive:        lipgloss.Color("#3E444F"), // panel fully behind popup overlay
	TextFaded:           lipgloss.Color("#6C768A"), // active panel that lost focus to sibling
	TextHint:            lipgloss.Color("#7B8EA6"), // mid-tone — Nord3/Nord4 midpoint
	Key:                 lipgloss.Color("6"),       // cyan         — Nord8 #88C0D0
	ItemText:            lipgloss.Color("7"),       // light gray   — Nord4
	Success:             lipgloss.Color("2"),       // green        — Nord14 #A3BE8C
	Warning:             lipgloss.Color("3"),       // yellow       — Nord13 #EBCB8B
	ScrollThumb:         lipgloss.Color("12"),      // bright blue — same as Accent
	ScrollTrack:         lipgloss.Color("#4C566A"), // same as Border — track blends with border
	SelectionBackground: lipgloss.Color("#2E3440"), // Nord0 — dark selected-row fill
	ContrastText:        lipgloss.Color("0"),       // black — contrast on bright accent bg
	ChromaStyle:         "nord",
}

// Darcula is the JetBrains Darcula colour scheme: warm dark background with
// muted blue/orange accents.
var Darcula = Theme{
	Accent:              lipgloss.Color("#6897BB"), // JetBrains blue
	Border:              lipgloss.Color("#4B4B4B"),
	PanelTitle:          lipgloss.Color("#A9B7C6"),
	TextPrimary:         lipgloss.Color("#A9B7C6"),
	TextSecondary:       lipgloss.Color("#9AA6B5"),
	TextMuted:           lipgloss.Color("#808080"),
	TextInactive:        lipgloss.Color("#3C3F41"),
	TextFaded:           lipgloss.Color("#606366"),
	TextHint:            lipgloss.Color("#6A6A6A"),
	Key:                 lipgloss.Color("#CC7832"), // JetBrains orange
	ItemText:            lipgloss.Color("#A9B7C6"),
	Success:             lipgloss.Color("#6A8759"),
	Warning:             lipgloss.Color("#CF8B53"),
	ScrollThumb:         lipgloss.Color("#6897BB"),
	ScrollTrack:         lipgloss.Color("#4B4B4B"),
	SelectionBackground: lipgloss.Color("#214283"),
	ContrastText:        lipgloss.Color("15"), // white — accent is dark blue
	ChromaStyle:         "darcula",
}

// Dracula is the dracula.github.io colour scheme: dark purple-tinted background
// with vivid pink, cyan, and green accents.
var Dracula = Theme{
	Accent:              lipgloss.Color("#BD93F9"), // Dracula purple
	Border:              lipgloss.Color("#44475A"),
	PanelTitle:          lipgloss.Color("#F8F8F2"),
	TextPrimary:         lipgloss.Color("#F8F8F2"),
	TextSecondary:       lipgloss.Color("#CCCCCC"),
	TextMuted:           lipgloss.Color("#6272A4"),
	TextInactive:        lipgloss.Color("#3D404F"),
	TextFaded:           lipgloss.Color("#5C6270"),
	TextHint:            lipgloss.Color("#6272A4"),
	Key:                 lipgloss.Color("#8BE9FD"), // Dracula cyan
	ItemText:            lipgloss.Color("#F8F8F2"),
	Success:             lipgloss.Color("#50FA7B"), // Dracula green
	Warning:             lipgloss.Color("#FFB86C"), // Dracula orange
	ScrollThumb:         lipgloss.Color("#BD93F9"),
	ScrollTrack:         lipgloss.Color("#44475A"),
	SelectionBackground: lipgloss.Color("#44475A"),
	ContrastText:        lipgloss.Color("0"), // black — accent is bright purple
	ChromaStyle:         "dracula",
}

// Gruvbox is the dark variant of the Gruvbox colour scheme: earthy warm
// background with retro yellow, orange, and aqua accents.
var Gruvbox = Theme{
	Accent:              lipgloss.Color("#83A598"), // Gruvbox aqua
	Border:              lipgloss.Color("#504945"),
	PanelTitle:          lipgloss.Color("#EBDBB2"),
	TextPrimary:         lipgloss.Color("#EBDBB2"),
	TextSecondary:       lipgloss.Color("#D5C4A1"),
	TextMuted:           lipgloss.Color("#928374"),
	TextInactive:        lipgloss.Color("#3C3836"),
	TextFaded:           lipgloss.Color("#665C54"),
	TextHint:            lipgloss.Color("#7C6F64"),
	Key:                 lipgloss.Color("#FABD2F"), // Gruvbox yellow
	ItemText:            lipgloss.Color("#EBDBB2"),
	Success:             lipgloss.Color("#B8BB26"), // Gruvbox green
	Warning:             lipgloss.Color("#FE8019"), // Gruvbox orange
	ScrollThumb:         lipgloss.Color("#83A598"),
	ScrollTrack:         lipgloss.Color("#504945"),
	SelectionBackground: lipgloss.Color("#3C3836"),
	ContrastText:        lipgloss.Color("0"), // black — accent is bright aqua
	ChromaStyle:         "gruvbox",
}

// GithubLight is a light theme matching GitHub's clean, minimal UI palette.
var GithubLight = Theme{
	Accent:              lipgloss.Color("#0969DA"), // GitHub blue
	Border:              lipgloss.Color("#D0D7DE"),
	PanelTitle:          lipgloss.Color("#24292F"),
	TextPrimary:         lipgloss.Color("#24292F"),
	TextSecondary:       lipgloss.Color("#57606A"),
	TextMuted:           lipgloss.Color("#6E7781"),
	TextInactive:        lipgloss.Color("#D0D7DE"),
	TextFaded:           lipgloss.Color("#8C959F"),
	TextHint:            lipgloss.Color("#6E7781"),
	Key:                 lipgloss.Color("#0550AE"),
	ItemText:            lipgloss.Color("#24292F"),
	Success:             lipgloss.Color("#1A7F37"),
	Warning:             lipgloss.Color("#CF222E"),
	ScrollThumb:         lipgloss.Color("#0969DA"),
	ScrollTrack:         lipgloss.Color("#D0D7DE"),
	SelectionBackground: lipgloss.Color("#DDF4FF"),
	ContrastText:        lipgloss.Color("15"), // white — accent is dark blue
	ChromaStyle:         "github",
}

// SolarizedLight is the classic Solarized light colour scheme, praised for
// its readability and contrast ratios.
var SolarizedLight = Theme{
	Accent:              lipgloss.Color("#268BD2"), // Solarized blue
	Border:              lipgloss.Color("#93A1A1"),
	PanelTitle:          lipgloss.Color("#657B83"),
	TextPrimary:         lipgloss.Color("#657B83"),
	TextSecondary:       lipgloss.Color("#586E75"),
	TextMuted:           lipgloss.Color("#839496"),
	TextInactive:        lipgloss.Color("#EEE8D5"),
	TextFaded:           lipgloss.Color("#93A1A1"),
	TextHint:            lipgloss.Color("#839496"),
	Key:                 lipgloss.Color("#2AA198"), // Solarized cyan
	ItemText:            lipgloss.Color("#657B83"),
	Success:             lipgloss.Color("#859900"), // Solarized green
	Warning:             lipgloss.Color("#CB4B16"), // Solarized orange
	ScrollThumb:         lipgloss.Color("#268BD2"),
	ScrollTrack:         lipgloss.Color("#93A1A1"),
	SelectionBackground: lipgloss.Color("#EEE8D5"),
	ContrastText:        lipgloss.Color("15"), // white — accent is dark blue
	ChromaStyle:         "solarized-light",
}

// ActiveTheme is the theme used by RebuildStyles. Set it before calling
// tui.Run() to select a different built-in theme. Defaults to Nordico.
var ActiveTheme = Nordico

// ActiveThemeName is the key in AvailableThemes for the current ActiveTheme.
// Updated by the theme picker alongside ActiveTheme.
var ActiveThemeName = "nordico"

// AvailableThemes is the catalogue of built-in themes, keyed by the name
// accepted by the --theme flag and stored in the [ui] config section.
var AvailableThemes = map[string]Theme{
	"nordico":         Nordico,
	"darcula":         Darcula,
	"dracula":         Dracula,
	"gruvbox":         Gruvbox,
	"github-light":    GithubLight,
	"solarized-light": SolarizedLight,
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
