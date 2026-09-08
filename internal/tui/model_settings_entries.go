package tui

// model_settings_entries.go — the presentation of a settings row's VALUE, where
// that value is more than a string: a filesystem path that has to be readable in
// a fixed column, and an analyser entry that has to say whether plumb will
// actually run it.
//
// Split out of model_settings.go, which builds the rows themselves. The rule
// here is that nothing rewrites a stored value: everything below produces a
// display form, and what the list editor seeds from and writes back is untouched.

import (
	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/quality"
	"github.com/plumbkit/plumb/internal/render"
)

// listEntry is one rendered entry of a settingList row: the text, an optional
// trailing badge glyph, and the style both take.
//
// The badge is kept separate from the text rather than concatenated because the
// value column is ellipsis-truncated by RUNE budget: a badge folded into the
// text can be severed mid-glyph (⚠️ is a base character plus a variation
// selector), which prints a different symbol than the one that was chosen. The
// renderer truncates the text and then appends the badge, so the signal survives
// a narrow pane even when the detail does not.
type listEntry struct {
	text  string
	badge string
	style lipgloss.Style
	// styled says whether style is meaningful. An explicit flag rather than a
	// zero-value probe on style: what an unset lipgloss.Style reports for its
	// foreground is an implementation detail of the library, and an entry that
	// silently lost its colour on a lipgloss upgrade would fail in exactly the
	// direction this row must not — a broken analyser rendering as a fine one.
	styled bool
}

// display returns the entry as one plain string, for width measurement.
func (e listEntry) display() string {
	if e.badge == "" {
		return e.text
	}
	return e.text + " " + e.badge
}

// settingsPathWidth caps how many columns a path may occupy in the value column.
// Wide enough for "~/.local/share/uv/tools/ruff/bin/ruff" and its neighbours,
// narrow enough that one deeply nested entry does not set the column width for
// every row beside it — settingsColumnWidths sizes the column to its widest cell,
// so an uncapped path is a layout decision made by whoever has the longest
// GOPATH.
const settingsPathWidth = 50

// displayPath is how every filesystem path in the Settings pane is shown: $HOME
// contracted to ~, then interior segments elided on separator boundaries until
// it fits.
//
// Both halves are render's, not new here. The tilde is a real stored value and
// not merely a display form — config.expandPath expands it back on load — so a
// path shown as ~/... can be typed back in as ~/... . The elision cuts in the
// MIDDLE because the two ends of a path are what identify it: the root says
// whose it is and the tail says which tool, while the nesting in between is the
// part a reader skips anyway.
//
// Display only. effectiveList and the list editor keep the raw string, so what
// is written back is exactly what the user typed.
func displayPath(p string) string {
	if p == "" {
		return p
	}
	return render.ShortenPath(render.ContractPath(p), settingsPathWidth)
}

// analyserEntries renders the [quality] analysers list: one entry per line as
// "<name>  <language>  <resolved path>", or the reason it will not run.
//
// This row is why the whole entry-annotation mechanism exists. An unrecognised
// analyser name used to be dropped inside cli.buildAnalysers with no error, no
// log and no mark here, so the pane displayed a configured-and-working analyser
// that plumb had silently discarded. The three failure colours are the three
// answers a user needs, and they differ in who has to act:
//
//   - red, a name plumb supports whose binary is nowhere — install it, or point
//     [quality.bin] at it;
//   - yellow, something plumb can see but has no adapter for — nothing the user
//     installs will help;
//   - red again for a name that is neither, which is nearly always a typo.
//
// The language column is not decoration: an analyser name a user does not
// recognise ("taplo", "oxlint") is unreadable without it, and it is the fastest
// way to see that a list has nothing configured for the language being written.
func analyserEntries(q config.QualityConfig) []listEntry {
	states := quality.ClassifyEntries(q.Analysers, q.Bin)
	out := make([]listEntry, 0, len(states))
	for _, st := range states {
		out = append(out, analyserEntry(st))
	}
	return out
}

func analyserEntry(st quality.EntryState) listEntry {
	// A recognised name is shown as itself; anything else is shown as the user
	// typed it, contracted, because the string they need to find and fix is the
	// one in their config file.
	head := st.Entry
	if !st.Known {
		head = displayPath(st.Entry)
	}
	if lang := st.Language(); lang != "" {
		head += "  " + lang
	}

	if st.Status == quality.EntryOK {
		return listEntry{text: head + "  " + displayPath(st.Binary)}
	}
	entry := listEntry{
		text:  head + "  " + st.Short,
		style: WarnStyle, styled: true, badge: badgeUnsupported,
	}
	if st.Status.Blocking() {
		entry.style, entry.badge = MissingStyle, badgeMissing
	}
	return entry
}

// The two badges an analyser row can carry. They are deliberately different
// SHAPES and not merely different colours: the pane is read at a glance, some
// terminals render 16-colour palettes unpredictably, and a colour-blind reader
// gets nothing from red-versus-yellow alone.
const (
	// badgeMissing marks something plumb would run but cannot find.
	badgeMissing = "⚠️"
	// badgeUnsupported marks something plumb can see but has no adapter for.
	badgeUnsupported = "\U0001f6a7"
)

// settingItemID identifies a row across two builds of the settings list: the key
// alone is not enough, because the per-language [lsp.<lang>] rows repeat every
// key once per language.
type settingItemID struct {
	key  settingKey
	lang string
}

func indexSettingItems(items []settingItem) map[settingItemID]settingItem {
	out := make(map[settingItemID]settingItem, len(items))
	for _, it := range items {
		out[settingItemID{it.key, it.lspLang}] = it
	}
	return out
}

// globalValueFor replaces a row's displayed VALUE with the one built from the
// global config, keeping everything else about the row.
//
// It is needed because "not in effect" has two shapes and only one of them is
// self-correcting. A trust-gated key is forced back to the global value by
// LoadProject itself, so a row built from the merged config already shows what
// is in force. An INERT key is not forced by anything — nothing reads it, so
// nothing bothers to overwrite it — and the merged config faithfully carries the
// project's value. Rendering that would put the project's list on screen,
// resolved and unbadged, next to a ⁶ saying it does not apply, while the
// analysers actually running appeared nowhere: a quieter version of the very bug
// the ⁶ mark was added to fix.
func globalValueFor(it settingItem, globals map[settingItemID]settingItem) settingItem {
	g, ok := globals[settingItemID{it.key, it.lspLang}]
	if !ok {
		return it
	}
	it.value, it.list, it.entries = g.value, g.list, g.entries
	return it
}
