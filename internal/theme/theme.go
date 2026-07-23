// Package theme holds plumb's UI-agnostic colour palettes. It carries no
// rendering dependency (no bubbletea, no lipgloss) so both the terminal UI
// (internal/tui) and the web UI (internal/web) can consume the same palette
// catalogue. Every colour is a plain hex string ("#RRGGBB"), which the web UI
// feeds straight into CSS custom properties and the TUI maps onto lipgloss
// colours.
//
// Concurrency: the package state is read-only after init — the palette maps are
// package-level literals, never mutated — so all exported values are safe for
// concurrent use.
package theme

import (
	"slices"
	"sort"
)

// Palette is the full set of colour tokens for one theme, expressed as hex
// strings. The semantic naming answers "what is this colour FOR?", mirroring
// the TUI Theme so a palette change tracks consistently across both surfaces.
//
// The first block (Bg…Forest) carries the surface/structural colours the web UI
// needs for cards, rules and gradients — the TUI never sets a background, so
// these are web-only. The second block mirrors the TUI's semantic foreground
// tokens. Cats are the categorical accents used by charts (per-language nodes,
// stacked series).
type Palette struct {
	// Surfaces.
	Bg    string `json:"bg"`    // page background
	Card  string `json:"card"`  // card surface
	Card2 string `json:"card2"` // nested/secondary card surface
	Rule  string `json:"rule"`  // borders, separators

	// Foreground / semantic.
	Text   string `json:"text"`   // primary text
	Soft   string `json:"soft"`   // secondary text
	Faint  string `json:"faint"`  // muted text, timestamps
	Acc    string `json:"acc"`    // primary accent
	Acc2   string `json:"acc2"`   // accent, darker shade
	Grn    string `json:"grn"`    // success / sage
	Warn   string `json:"warn"`   // error / failure
	Forest string `json:"forest"` // deep success shade (gauges, light themes)

	// Cats are categorical chart accents (force-graph languages, stacked series,
	// sankey groups). Index 0/1 deliberately echo Acc/Grn.
	Cats []string `json:"cats"`

	// Dark reports whether this is a dark theme, so the web UI can pick a
	// matching base colour-scheme without parsing the background.
	Dark bool `json:"dark"`

	// ChromaStyle is the syntax-highlighting style name, mirrored from the TUI
	// theme so code blocks in the web UI can match.
	ChromaStyle string `json:"chromaStyle"`
}

// Default is the palette name used when config names an unknown theme.
const Default = "plumb"

// catsDark / catsLight are the categorical accent ramps. They stay constant
// across themes of the same mode so charts keep a stable, legible legend.
var (
	catsDark  = []string{"#e08a55", "#5cb88a", "#46606c", "#6e5a7a", "#8a5e2c", "#a14a26"}
	catsLight = []string{"#b35a2e", "#2f6e4f", "#46606c", "#6e5a7a", "#8a5e2c", "#a14a26"}
)

// palettes is the catalogue, keyed by the same names as the TUI AvailableThemes
// (the value stored in [ui] theme). Each is a hand-tuned hex palette so the web
// UI never has to resolve terminal-palette indices (which the TUI uses for some
// themes and which carry no fixed hex).
var palettes = map[string]Palette{
	"plumb": {
		Bg: "#121310", Card: "#1b1c17", Card2: "#22231d", Rule: "#2e2f27",
		Text: "#ece8dc", Soft: "#aaa595", Faint: "#787465",
		Acc: "#e08a55", Acc2: "#b35a2e", Grn: "#5cb88a", Warn: "#d9694a", Forest: "#2f6e4f",
		Cats: catsDark, Dark: true, ChromaStyle: "gruvbox",
	},
	"plumb-light": {
		Bg: "#faf9f5", Card: "#ffffff", Card2: "#f1efe8", Rule: "#e5e2d8",
		Text: "#191813", Soft: "#56524a", Faint: "#8d887c",
		Acc: "#b35a2e", Acc2: "#8a4421", Grn: "#2f6e4f", Warn: "#b3302a", Forest: "#214f38",
		Cats: catsLight, Dark: false, ChromaStyle: "gruvbox-light",
	},
	"nordico": {
		Bg: "#2e3440", Card: "#3b4252", Card2: "#434c5e", Rule: "#4c566a",
		Text: "#eceff4", Soft: "#d8dee9", Faint: "#7b8ea6",
		Acc: "#88c0d0", Acc2: "#5e81ac", Grn: "#a3be8c", Warn: "#bf616a", Forest: "#3b6e4f",
		Cats: []string{"#88c0d0", "#a3be8c", "#5e81ac", "#b48ead", "#d08770", "#ebcb8b"},
		Dark: true, ChromaStyle: "nord",
	},
	"darcula": {
		Bg: "#2b2b2b", Card: "#313335", Card2: "#3c3f41", Rule: "#4b4b4b",
		Text: "#a9b7c6", Soft: "#9aa6b5", Faint: "#808080",
		Acc: "#6897bb", Acc2: "#4a6e8c", Grn: "#6a8759", Warn: "#cf8b53", Forest: "#4a6e3f",
		Cats: []string{"#6897bb", "#6a8759", "#cc7832", "#9876aa", "#bbb529", "#cf8b53"},
		Dark: true, ChromaStyle: "darcula",
	},
	"dracula": {
		Bg: "#282a36", Card: "#343746", Card2: "#44475a", Rule: "#44475a",
		Text: "#f8f8f2", Soft: "#cccccc", Faint: "#6272a4",
		Acc: "#bd93f9", Acc2: "#8a5fd6", Grn: "#50fa7b", Warn: "#ff5555", Forest: "#3fae5a",
		Cats: []string{"#bd93f9", "#50fa7b", "#8be9fd", "#ff79c6", "#ffb86c", "#f1fa8c"},
		Dark: true, ChromaStyle: "dracula",
	},
	"gruvbox": {
		Bg: "#282828", Card: "#32302f", Card2: "#3c3836", Rule: "#504945",
		Text: "#ebdbb2", Soft: "#d5c4a1", Faint: "#928374",
		Acc: "#83a598", Acc2: "#458588", Grn: "#b8bb26", Warn: "#fb4934", Forest: "#79740e",
		Cats: []string{"#83a598", "#b8bb26", "#fabd2f", "#d3869b", "#fe8019", "#8ec07c"},
		Dark: true, ChromaStyle: "gruvbox",
	},
	"github-light": {
		Bg: "#ffffff", Card: "#f6f8fa", Card2: "#eaeef2", Rule: "#d0d7de",
		Text: "#24292f", Soft: "#57606a", Faint: "#6e7781",
		Acc: "#0969da", Acc2: "#0550ae", Grn: "#1a7f37", Warn: "#cf222e", Forest: "#116329",
		Cats: []string{"#0969da", "#1a7f37", "#8250df", "#bf3989", "#bc4c00", "#9a6700"},
		Dark: false, ChromaStyle: "github",
	},
	"solarized-light": {
		Bg: "#fdf6e3", Card: "#eee8d5", Card2: "#e4ddc8", Rule: "#93a1a1",
		Text: "#657b83", Soft: "#586e75", Faint: "#839496",
		Acc: "#268bd2", Acc2: "#1c6aa5", Grn: "#859900", Warn: "#cb4b16", Forest: "#5f7000",
		Cats: []string{"#268bd2", "#859900", "#2aa198", "#d33682", "#cb4b16", "#b58900"},
		Dark: false, ChromaStyle: "solarized-light",
	},
}

// Get returns the palette for name, falling back to Default when name is empty
// or unknown. The boolean reports whether name matched a known theme.
//
// The returned Cats slice is a copy: the catalogue palettes share package-level
// backing arrays (catsDark/catsLight), so handing out the alias would let one
// caller's in-place mutation corrupt the palette for everyone.
func Get(name string) (Palette, bool) {
	p, ok := palettes[name]
	if !ok {
		p = palettes[Default]
	}
	p.Cats = slices.Clone(p.Cats)
	return p, ok
}

// Names returns the sorted catalogue of palette names. It matches the TUI's
// ThemeNames so the web theme picker and the TUI offer the same set.
func Names() []string {
	out := make([]string, 0, len(palettes))
	for k := range palettes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
