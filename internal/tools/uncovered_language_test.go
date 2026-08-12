package tools

import (
	"strings"
	"testing"
)

// The tests here all guard one property: when plumb cannot index a language, it
// must SAY SO at the point of use. An agent can route around a stated
// limitation; it cannot route around silence, and every one of these surfaces
// previously returned a confident-looking empty answer instead.

// The witness must be a language that is still uncovered. This test used .rb
// until the Ruby extractor landed and correctly went red — the registry pin
// working, not a fault. Whoever wires Scala should move it on again.
func TestUncoveredOutlineNote_NamesTheGapForAnUnindexedLanguage(t *testing.T) {
	note := uncoveredOutlineNote("file:///ws/app/models/User.scala")
	if note == "" {
		t.Fatal("a .scala file has no extractor; the empty outline must be explained, not left bare")
	}
	for _, want := range []string{"scala", "read_file", "search_in_files"} {
		if !strings.Contains(note, want) {
			t.Errorf("note should name %q so the agent has somewhere to go; got:\n%s", want, note)
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

func TestFormatFileOutline_ExplainsAnEmptyOutlineForAnUncoveredLanguage(t *testing.T) {
	out := formatFileOutline(&outlineResult{uri: "file:///ws/app/models/User.scala", source: "topology"})
	if !strings.Contains(out, "(no symbols)") {
		t.Errorf("the empty-outline line should still be present:\n%s", out)
	}
	if !strings.Contains(out, "coverage gap") {
		t.Errorf("an empty Scala outline must be attributed to coverage, not read as an empty file:\n%s", out)
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
		{"elixir is recognised but unindexed", "Elixir", "elixir"},
		// langFileProfile maps this label to .c/.cpp/.cc/.h/.hpp; the first
		// uncovered extension decides, which is what makes the label→registry
		// resolution work without a second hand-written table.
		{"c/c++ resolves through its extensions", "C/C++ (CMake)", "c"},
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
