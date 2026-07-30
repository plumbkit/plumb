package tui

import (
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/theme"
)

// The theme catalogue is deliberately expressed twice, in two different colour
// spaces: internal/tui holds lipgloss Themes (some values are terminal-palette
// indices like "12", which have no fixed hex and delegate to the user's terminal
// profile), and internal/theme holds hand-tuned hex Palettes that the web UI can
// feed straight into CSS. Neither can be derived from the other, so the
// duplication stays.
//
// What must NOT diverge is the catalogue itself. Both packages' doc comments
// already promise it — internal/theme's palettes are "keyed by the same names as
// the TUI AvailableThemes" and its Names() "matches the TUI's ThemeNames so the
// web theme picker and the TUI offer the same set" — but until these tests
// nothing checked it. The failure mode is quiet: add a theme to the TUI picker
// only, and theme.Get falls back to Default, so the web UI renders the wrong
// colours with no error anywhere.

// TestThemeCatalogues_KeySetsMatch is the guard both doc comments assumed.
func TestThemeCatalogues_KeySetsMatch(t *testing.T) {
	tuiNames := map[string]bool{}
	for name := range AvailableThemes {
		tuiNames[name] = true
	}

	webNames := map[string]bool{}
	for _, name := range theme.Names() {
		webNames[name] = true
	}

	for name := range tuiNames {
		if !webNames[name] {
			t.Errorf("theme %q exists in tui.AvailableThemes but has no palette in internal/theme.\n"+
				"    The web UI would silently fall back to %q for this theme.\n"+
				"    Add a hex palette for it to internal/theme/theme.go.", name, theme.Default)
		}
	}
	for name := range webNames {
		if !tuiNames[name] {
			t.Errorf("palette %q exists in internal/theme but has no tui.Theme.\n"+
				"    The web picker would offer a theme the TUI cannot render.\n"+
				"    Add a Theme literal for it to internal/tui/theme.go.", name)
		}
	}
}

// TestThemeCatalogues_ChromaStylesMatch pins the other cross-package claim:
// Palette.ChromaStyle is documented as "mirrored from the TUI theme so code
// blocks in the web UI can match". A mismatch means `plumb config show` and the
// web UI highlight the same code with different palettes.
func TestThemeCatalogues_ChromaStylesMatch(t *testing.T) {
	for name, tuiTheme := range AvailableThemes {
		palette, ok := theme.Get(name)
		if !ok {
			continue // reported by TestThemeCatalogues_KeySetsMatch
		}
		if palette.ChromaStyle != tuiTheme.ChromaStyle {
			t.Errorf("theme %q: chroma style differs — tui has %q, internal/theme has %q",
				name, tuiTheme.ChromaStyle, palette.ChromaStyle)
		}
	}
}

// TestThemeCatalogues_EveryThemeResolvesExactly asserts theme.Get reports a real
// hit (ok == true) for every advertised theme. Get returns the default palette
// even on a miss, so a caller that ignores ok sees plausible colours; this test
// is what makes that fallback a safety net rather than a hiding place.
func TestThemeCatalogues_EveryThemeResolvesExactly(t *testing.T) {
	for name := range AvailableThemes {
		if _, ok := theme.Get(name); !ok {
			t.Errorf("theme.Get(%q) fell back to the default instead of resolving exactly", name)
		}
	}
}

// TestThemeDefaults_Agree keeps the three places that name a default theme in
// step: the palette catalogue's Default, the TUI's initial ActiveThemeName, and
// the compiled config default for [ui] theme.
func TestThemeDefaults_Agree(t *testing.T) {
	if theme.Default != ActiveThemeName {
		t.Errorf("default theme disagrees: theme.Default = %q, tui.ActiveThemeName = %q",
			theme.Default, ActiveThemeName)
	}
	if _, ok := AvailableThemes[theme.Default]; !ok {
		t.Errorf("theme.Default = %q is not a key in tui.AvailableThemes", theme.Default)
	}

	// AGENTS.md documents the [ui] theme default as a key in AvailableThemes; a
	// default that does not resolve would make a config-less start render the
	// fallback rather than the intended theme.
	if got := config.Defaults().UI.Theme; got != "" {
		if _, ok := AvailableThemes[got]; !ok {
			t.Errorf("config default [ui] theme = %q is not a key in tui.AvailableThemes", got)
		}
	}
}
