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

// This test used to need a WITNESS — a language plumb recognised but had no
// extractor for — and it moved as the coverage programme landed: .rb, then
// .php, then .scala, going red each time as the registry pin did its job.
//
// There is no witness left. Every language in the registry is now indexed, so
// Uncovered() is empty and this note can never fire. That is the programme
// completing, not the mechanism breaking, and the mechanism is still here and
// still correct — the moment a new EngineNone row is added, the note starts
// firing again and the assertion below starts failing, which is exactly the
// signal whoever adds that language wants.
//
// So the property under test inverts: the note must stay SILENT while nothing
// is uncovered.
func TestUncoveredOutlineNote_SilentWhileEveryLanguageIsIndexed(t *testing.T) {
	if got := len(langsupport.Uncovered()); got != 0 {
		t.Fatalf("Uncovered() = %d languages, want 0 — a new uncovered row was added, "+
			"so this test should go back to naming it as the witness", got)
	}
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
		// Elixir. NOTE FOR WHOEVER WIRES THE LAST EXTRACTOR — langFileProfile only
		// ever produces two labels that can be uncovered at all, "Elixir" and
		// "C/C++ (CMake)", and the coverage programme indexes both. With Elixir
		// landed, Scala is the only uncovered language left and it has no label
		// here, so this table already has no non-empty witness. When Scala lands,
		// Uncovered() is empty and every label correctly yields no note — assert
		// that dormant state. It is the design completing, not breaking.
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
