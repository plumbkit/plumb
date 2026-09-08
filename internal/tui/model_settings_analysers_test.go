package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/quality"
)

// The Analysers row is the reason the entry-annotation mechanism exists. A
// configured analyser plumb cannot run used to render identically to one it runs
// on every write, because the name was dropped inside cli.buildAnalysers with
// nothing reported anywhere. Each case below is one of the answers that were
// previously all rendered as "fine".

func TestAnalyserEntry_OKCarriesNoBadgeAndNoColour(t *testing.T) {
	got := analyserEntry(quality.EntryState{
		Entry:  "ruff",
		Known:  true,
		Tool:   mustQualityTool(t, "ruff"),
		Status: quality.EntryOK,
		Binary: "/usr/local/bin/ruff",
	})
	if got.badge != "" {
		t.Errorf("a working analyser must carry no badge, got %q", got.badge)
	}
	if got.styled {
		t.Error("a working analyser must take the row's ordinary scope style")
	}
	for _, want := range []string{"ruff", "python", "/usr/local/bin/ruff"} {
		if !strings.Contains(got.text, want) {
			t.Errorf("entry text %q is missing %q", got.text, want)
		}
	}
}

// A missing binary and an unsupported tool must be told apart on sight, because
// the person who has to act differs: installing something fixes the first and
// can never fix the second. They differ in SHAPE as well as colour — a
// colour-blind reader, or a terminal with an odd 16-colour palette, still gets
// the distinction.
func TestAnalyserEntry_MissingAndUnsupportedAreDistinguishable(t *testing.T) {
	missing := analyserEntry(quality.EntryState{
		Entry: "ruff", Known: true, Tool: mustQualityTool(t, "ruff"),
		Status: quality.EntryBinaryMissing, Short: "not found",
	})
	unsupported := analyserEntry(quality.EntryState{
		Entry: "eslint", Known: true, Tool: mustQualityTool(t, "eslint"),
		Status: quality.EntryUnsupported, Short: "no plumb adapter",
	})

	if missing.badge == unsupported.badge {
		t.Errorf("both statuses render badge %q — they must be distinguishable without colour", missing.badge)
	}
	if !missing.styled || !unsupported.styled {
		t.Fatal("both statuses must carry an explicit style")
	}
	if missing.style.GetForeground() == unsupported.style.GetForeground() {
		t.Error("a missing binary (the user's to fix) and an unsupported tool (plumb's) render the same colour")
	}
	if !strings.Contains(missing.text, "not found") {
		t.Errorf("missing entry must say so in words, got %q", missing.text)
	}
	if !strings.Contains(unsupported.text, "no plumb adapter") {
		t.Errorf("unsupported entry must say so in words, got %q", unsupported.text)
	}
}

// The case that prompted the change: an absolute path pasted into the list. It
// is rejected — a path must never become an argv — and the row has to carry the
// spelling that works, contracted so it fits.
func TestAnalyserEntry_PathEntryIsContractedAndNamesTheFix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	entry := filepath.Join(home, ".local", "bin", "ruff")

	got := analyserEntry(quality.EntryState{
		Entry: entry, Known: false,
		Status: quality.EntryUnsupported, Short: `use the name "ruff"`,
	})
	if strings.Contains(got.text, home) {
		t.Errorf("the home directory must be contracted to ~, got %q", got.text)
	}
	if !strings.Contains(got.text, "~/.local/bin/ruff") {
		t.Errorf("entry text should show the contracted path, got %q", got.text)
	}
	if !strings.Contains(got.text, `use the name "ruff"`) {
		t.Errorf("entry must name the working spelling, got %q", got.text)
	}
}

func mustQualityTool(t *testing.T, name string) quality.Tool {
	t.Helper()
	tool, ok := quality.ToolByName(name)
	if !ok {
		t.Fatalf("registry has no %q row", name)
	}
	return tool
}

// --- path display ----------------------------------------------------------

func TestDisplayPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("contracts the home directory", func(t *testing.T) {
		got := displayPath(filepath.Join(home, ".local", "bin", "ruff"))
		if got != "~/.local/bin/ruff" {
			t.Errorf("displayPath = %q, want ~/.local/bin/ruff", got)
		}
	})

	// The cut comes out of the MIDDLE, on separator boundaries. Both ends of a
	// path identify it — the root says whose it is, the tail says which tool —
	// and a leading or trailing cut throws one of those away.
	t.Run("elides interior segments only", func(t *testing.T) {
		long := "/one/two/three/four/five/six/seven/eight/nine/ten/eleven/twelve/tool"
		got := displayPath(long)
		if lipgloss.Width(got) > settingsPathWidth {
			t.Errorf("displayPath = %q (%d cols), want at most %d", got, lipgloss.Width(got), settingsPathWidth)
		}
		if !strings.HasSuffix(got, "tool") {
			t.Errorf("the tail identifies the tool and must survive, got %q", got)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("an over-long path must show where it was cut, got %q", got)
		}
	})

	t.Run("leaves a path that already fits byte-identical", func(t *testing.T) {
		if got := displayPath("/usr/bin/ruff"); got != "/usr/bin/ruff" {
			t.Errorf("displayPath = %q, want the input unchanged", got)
		}
	})

	t.Run("empty stays empty", func(t *testing.T) {
		if got := displayPath(""); got != "" {
			t.Errorf("displayPath(\"\") = %q", got)
		}
	})
}

// Contraction is DISPLAY ONLY. The list editor seeds from the stored value and
// writes it back, so a contracted path leaking into the model would rewrite the
// user's config on the next save.
func TestPathRowsDoNotMutateTheStoredValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stored := filepath.Join(home, "projects", "thing")

	it := settingItem{kind: settingList, key: skExtraRoots, list: []string{stored}, path: true}
	if got := rowValues(it)[0]; got == stored {
		t.Fatalf("the row did not contract the path at all, got %q", got)
	}
	if it.list[0] != stored {
		t.Errorf("the stored entry was rewritten to %q, want %q", it.list[0], stored)
	}
}

// A row that is not a path is left exactly as stored, so an ordinary setting
// that happens to contain a slash is never quietly rewritten.
func TestNonPathRowIsNotContracted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	value := filepath.Join(home, "looks", "like", "a", "path")

	it := settingItem{kind: settingList, key: skExcludePatterns, list: []string{value}}
	if got := rowValues(it)[0]; got != value {
		t.Errorf("rowValues = %q, want the value unchanged (%q)", got, value)
	}
}

// --- rendering -------------------------------------------------------------

// The badge is appended AFTER truncation, so a narrow pane loses detail and
// never the signal. Folding it into the text would let textfmt.Ellipsis — which
// budgets runes — sever ⚠️ between its base character and its variation
// selector, printing a different symbol than the one that was chosen.
func TestRenderValueCell_BadgeSurvivesTruncation(t *testing.T) {
	e := listEntry{
		text:   "a-very-long-analyser-entry-that-will-not-fit-in-the-column",
		badge:  badgeMissing,
		style:  MissingStyle,
		styled: true,
	}
	got := ansi.Strip(renderValueCell(e, 20, DetailStyle))
	if !strings.HasSuffix(got, badgeMissing) {
		t.Errorf("truncated cell %q lost its badge", got)
	}
	if lipgloss.Width(got) > 20 {
		t.Errorf("cell is %d cols wide, want at most 20: %q", lipgloss.Width(got), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("a truncated cell must show it was cut, got %q", got)
	}
}

// The FOCUSED row renders label, value and control in one SelectedStyle pass, so
// it cannot reuse the styled cell - and while it built its own value it lost the
// truncation entirely. A long value then ran past its column into the control,
// wrapping the row and corrupting the pane's borders, which is the exact failure
// clampSettingsValueW exists to prevent.
func TestSettingsRowDisplay_FocusedRowIsTruncatedLikeAnUnfocusedOne(t *testing.T) {
	const valueW = 24
	it := settingItem{
		kind: settingList, key: skAnalysers,
		list: []string{"x"},
		entries: []listEntry{{
			text:   "an-analyser-entry-far-too-long-for-this-column",
			badge:  badgeUnsupported,
			style:  WarnStyle,
			styled: true,
		}},
	}
	focused := ansi.Strip(settingsRowDisplay(it, true, false, 16, valueW))
	unfocused := ansi.Strip(settingsRowDisplay(it, false, false, 16, valueW))

	if !strings.Contains(focused, "\u2026") {
		t.Errorf("the focused row was not truncated: %q", focused)
	}
	if lipgloss.Width(focused) != lipgloss.Width(unfocused) {
		t.Errorf("focused row is %d cols and unfocused is %d - both must fit the same layout\nfocused:   %q\nunfocused: %q",
			lipgloss.Width(focused), lipgloss.Width(unfocused), focused, unfocused)
	}
}

// The value column is padded to a DISPLAY width. fmt's "%-*s" counts runes, and
// a badge glyph is one rune occupying two terminal columns, so padding that way
// left an emoji row a column short and dragged its control out of line with
// every other row's.
func TestSettingsRowDisplay_EmojiRowStaysAlignedWithAPlainOne(t *testing.T) {
	const labelW, valueW = 16, 30
	plain := settingItem{
		kind: settingList, key: skAnalysers, list: []string{"a"},
		entries: []listEntry{{text: "ruff  python  ~/bin/ruff"}},
	}
	badged := settingItem{
		kind: settingList, key: skAnalysers, list: []string{"a"},
		entries: []listEntry{{text: "eslint  ts  no adapter", badge: badgeUnsupported, style: WarnStyle, styled: true}},
	}

	p := ansi.Strip(settingsRowDisplay(plain, false, false, labelW, valueW))
	b := ansi.Strip(settingsRowDisplay(badged, false, false, labelW, valueW))
	if lipgloss.Width(p) != lipgloss.Width(b) {
		t.Errorf("a badged row is %d cols and a plain one %d - the control column would not line up\nplain:  %q\nbadged: %q",
			lipgloss.Width(b), lipgloss.Width(p), p, b)
	}
}

// An entry with no style of its own takes the row's scope style, so an
// inherited-and-dimmed workspace row does not have one entry burning bright.
func TestRenderValueCell_UnstyledEntryUsesTheFallback(t *testing.T) {
	plain := renderValueCell(listEntry{text: "ruff"}, 30, MissingStyle)
	if plain != MissingStyle.Render("ruff") {
		t.Errorf("an unstyled entry must render in the fallback style, got %q", plain)
	}
}

// The "(N)" label count, the continuation lines and the rendered cells must all
// come from one number. A row whose entries and list disagreed would stack
// entries the label had not counted, or drop one off the bottom.
func TestRowCountsComeFromASingleSource(t *testing.T) {
	it := settingItem{
		kind: settingList,
		key:  skAnalysers,
		list: []string{"golangci-lint", "ruff", "eslint"},
		entries: []listEntry{
			{text: "golangci-lint  go"},
			{text: "ruff  python"},
			{text: "eslint  typescript  no plumb adapter", badge: badgeUnsupported},
		},
	}
	if got := rowValueCount(it); got != 3 {
		t.Fatalf("rowValueCount = %d, want 3", got)
	}
	if got := rowLabel(it); !strings.Contains(got, "(3)") {
		t.Errorf("rowLabel = %q, want a (3) count", got)
	}
	values := rowValues(it)
	if len(values) != 3 {
		t.Fatalf("rowValues returned %d lines, want 3", len(values))
	}
	// The measured width must include the badge, or the column is sized one
	// glyph too narrow and every badge on the pane is truncated away.
	if !strings.HasSuffix(values[2], badgeUnsupported) {
		t.Errorf("the measured line must include the badge, got %q", values[2])
	}

	lines := settingsLogicalLines([]settingItem{it})
	conts := 0
	for _, l := range lines {
		if l.kind == slRow && l.cont > 0 {
			conts++
		}
	}
	if conts != 2 {
		t.Errorf("got %d continuation lines for a 3-entry row, want 2", conts)
	}
}

// --- scope honesty ---------------------------------------------------------

// [quality] is read from the global store, so a value a project sets is written
// and then never read. It used to render with the green ⁴ override mark: a user
// could set analysers on a workspace, watch the pane confirm an override, and
// get no findings, with nothing anywhere to explain it.
func TestBuildScopeItems_QualityRowsAreNotInEffectAtWorkspaceScope(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"quality", "analysers"}, []string{"ruff"}); err != nil {
		t.Fatal(err)
	}
	if err := config.SetProjectValue(ws, []string{"topology", "watch"}, false); err != nil {
		t.Fatal(err)
	}

	m := &Model{
		settingsCfg:         config.Defaults(),
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
	}
	var found bool
	for _, it := range m.buildScopeItems() {
		switch it.key {
		case skAnalysers:
			found = true
			if !it.notInEffect {
				t.Error("quality.analysers set in a project config must be marked NOT in effect: " +
					"the runner reads the global store")
			}
			if it.overridden {
				t.Error("an ignored value must never also report as a live override")
			}
			// The value column must show what is ACTUALLY in force - the global
			// list - not the project's. Nothing forces an inert key back to base
			// (nothing reads it, so nothing bothers), so the merged config
			// faithfully carries the project's value and rendering it would put
			// "ruff" on screen, resolved and unbadged, beside a mark saying the
			// row does not apply, while the analyser really running appeared
			// nowhere.
			if len(it.list) != 1 || it.list[0] != "golangci-lint" {
				t.Errorf("value column = %v, want the global [\"golangci-lint\"] that is actually in force", it.list)
			}
		case skTopoWatch:
			// The control: a genuinely project-overridable key is unaffected, so
			// the new check has not simply marked everything dead.
			if !it.overridden || it.notInEffect {
				t.Error("topology.watch is project-overridable and must stay a live override")
			}
		}
	}
	if !found {
		t.Error("the analysers row is missing from the workspace scope")
	}
}

// The remedy differs by reason, and the wrong one is worse than none: sending
// someone to `plumb trust` for a global-only key has them grant a permission
// that changes nothing.
func TestScopedStatus_GlobalOnlyRowDoesNotSuggestPlumbTrust(t *testing.T) {
	ws := t.TempDir()
	m := Model{
		settingsScopes:      []settingScope{{global: true, label: "Global"}, {folder: ws, label: "ws"}},
		settingsScopeCursor: 1,
		settingsItems:       []settingItem{{key: skAnalysers, notInEffect: true}},
		settingsCursor:      0,
	}
	got := m.scopedStatus(skAnalysers, "set")
	if strings.Contains(got, "plumb trust") {
		t.Errorf("a global-only row must not be blamed on trust: %q", got)
	}
	if !strings.Contains(got, "Global scope") {
		t.Errorf("the status must name where the setting DOES apply: %q", got)
	}

	// The trust reason still gets the trust remedy.
	m.settingsItems = []settingItem{{key: skGitPush, notInEffect: true}}
	if got := m.scopedStatus(skGitPush, "set"); !strings.Contains(got, "plumb trust") {
		t.Errorf("a trust-gated row must still name `plumb trust`: %q", got)
	}
}

// Guards the file-level invariant the scope check depends on: every [quality]
// key the pane offers at a workspace scope answers false to
// config.AppliesAtProjectScope, so none of them can render as a live override.
func TestQualityRowsAreAllGlobalOnly(t *testing.T) {
	for _, k := range []settingKey{skQuality, skQualityMode, skQualityTimeoutMs, skQualityMaxFindings, skAnalysers} {
		path, ok := tomlPath(k)
		if !ok {
			continue // hidden at workspace scope; nothing to misreport
		}
		if config.AppliesAtProjectScope(strings.Join(path, ".")) {
			t.Errorf("%v is offered at workspace scope and claims to apply there, but the quality "+
				"runner reads the global store", path)
		}
	}
	// The negative control: the helper is not simply answering false for
	// everything, which would make the loop above pass for no reason.
	if !config.AppliesAtProjectScope("topology.watch") {
		t.Error("AppliesAtProjectScope answers false for a plain project preference")
	}
}
