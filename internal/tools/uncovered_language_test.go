package tools

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// The tests here all guard one property: when plumb cannot index a language, it
// must SAY SO at the point of use. An agent can route around a stated
// limitation; it cannot route around silence, and every one of these surfaces
// previously returned a confident-looking empty answer instead.

// This test needs a WITNESS — a language plumb recognises but has no extractor
// for — and it moves as the coverage programme lands: .rb, then .php, then
// .scala, going red each time as the registry pin does its job. The witness is
// currently Svelte, whose single-file-component shape embeds three languages in
// one file and so is not a quick extractor to wire.
//
// The property: for that language the note must FIRE and must name a route that
// still works, because an agent can route around a stated limitation and cannot
// route around silence.
func TestUncoveredOutlineNote_NamesTheGapForAnUncoveredLanguage(t *testing.T) {
	uncovered := langsupport.Uncovered()
	if len(uncovered) == 0 {
		t.Skip("no uncovered language left; the note is dormant and there is nothing to witness")
	}
	note := uncoveredOutlineNote("file:///ws/src/App" + uncovered[0].Extensions[0])
	if note == "" {
		t.Fatalf("%s is uncovered; an empty outline for it must be explained as a coverage gap", uncovered[0].Name)
	}
	if !strings.Contains(note, uncovered[0].Name) {
		t.Errorf("the note must name the language; got:\n%s", note)
	}
	if !strings.Contains(note, "search_in_files") {
		t.Errorf("the note must offer a route that still works; got:\n%s", note)
	}
}

// The companion: a language that IS indexed must stay silent. Explaining an
// empty outline away as a coverage gap is the same failure in reverse.
func TestUncoveredOutlineNote_SilentForIndexedLanguages(t *testing.T) {
	for _, uri := range []string{
		"file:///ws/app/models/User.scala",
		"file:///ws/lib/mod.ex",
		"file:///ws/src/Widget.cs",
	} {
		if note := uncoveredOutlineNote(uri); note != "" {
			t.Errorf("%s is indexed; no coverage-gap note expected, got:\n%s", uri, note)
		}
	}
}

// The note must not fire for a language plumb DOES index: there an empty outline
// is a fact about the file, and explaining it away as a coverage gap would be
// the same failure in reverse.
func TestUncoveredOutlineNote_SilentForIndexedAndUnknownTypes(t *testing.T) {
	for _, uri := range []string{
		"file:///ws/main.go",
		"file:///ws/app.py",
		"file:///ws/app/models/user.rb", // indexed since the Ruby extractor landed
		"file:///ws/README.md",
		"file:///ws/logo.png",
		"file:///ws/yarn.lock",
	} {
		if note := uncoveredOutlineNote(uri); note != "" {
			t.Errorf("%s is either indexed or unrecognised; no coverage-gap note expected, got:\n%s", uri, note)
		}
	}
}

// The companion to the above: with nothing uncovered, an empty outline is a
// fact about the FILE and must not be explained away as a coverage gap.
func TestFormatFileOutline_EmptyOutlineIsNotBlamedOnCoverage(t *testing.T) {
	out := formatFileOutline(&outlineResult{uri: "file:///ws/app/models/User.scala", source: "topology"})
	if !strings.Contains(out, "(no symbols)") {
		t.Errorf("the empty-outline line should still be present:\n%s", out)
	}
	if strings.Contains(out, "coverage gap") {
		t.Errorf("Scala is indexed; an empty outline is a fact about the file:\n%s", out)
	}
}

// A supported language keeps the terse output — the explanation is reserved for
// the case that actually needs it, so the common path gains no noise.
func TestFormatFileOutline_EmptyGoOutlineStaysTerse(t *testing.T) {
	out := formatFileOutline(&outlineResult{uri: "file:///ws/empty.go", source: "lsp"})
	if strings.Contains(out, "coverage gap") {
		t.Errorf("Go is indexed; an empty outline is a fact about the file:\n%s", out)
	}
}

func TestUncoveredPrimaryLanguageNote(t *testing.T) {
	tests := []struct {
		name string
		lang string // the display label session_start detected
		want string // "" ⇒ no note expected
	}{
		// The witness moves as extractors land: it was Ruby, then PHP, then
		// Elixir. There is no non-empty witness in this table, and that is a
		// property of langFileProfile rather than of coverage: it only ever
		// produces two labels that can be uncovered at all, "Elixir" and
		// "C/C++ (CMake)", and both are indexed. The languages that ARE uncovered
		// (Svelte, Vue) have no label here, so session_start cannot detect one as
		// a primary language and this note stays dormant for every label below.
		// TestUncoveredOutlineNote_NamesTheGapForAnUncoveredLanguage is where the
		// firing case is pinned.
		{"elixir is indexed since its extractor landed", "Elixir", ""},
		// langFileProfile maps this label to .c/.cpp/.cc/.h/.hpp; the first
		// uncovered extension decides, which is what makes the label→registry
		// resolution work without a second hand-written table. Every one of
		// those extensions is now indexed — .c since the C extractor landed and
		// .cpp/.cc/.hpp since the C++ one — so the label resolves to no
		// uncovered language at all and must produce no note.
		{"c/c++ is indexed since the C++ extractor landed", "C/C++ (CMake)", ""},
		{"go is indexed", "Go", ""},
		{"ruby is indexed since its extractor landed", "Ruby", ""},
		{"swift is indexed", "Swift", ""},
		{"typescript is indexed", "TypeScript", ""},
		{"an unknown label yields nothing", "Brainfuck", ""},
		{"no detected language yields nothing", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uncoveredPrimaryLanguageNote(tt.lang)
			if tt.want == "" {
				if got != "" {
					t.Errorf("expected no coverage note for %q, got:\n%s", tt.lang, got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("note for %q should name %q; got:\n%s", tt.lang, tt.want, got)
			}
			if !strings.Contains(got, "search_in_files") {
				t.Errorf("note for %q must offer a route that still works; got:\n%s", tt.lang, got)
			}
		})
	}
}
