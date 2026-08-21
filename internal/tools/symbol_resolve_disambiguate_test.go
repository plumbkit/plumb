package tools

import (
	"strconv"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// TestDisambiguatedNames_GoFlatMethodForm covers gopls' flat "(*Recv).Method"
// method-symbol shape: the candidate list must offer the dotted "Recv.Method"
// form a caller can paste straight into symbol_name.
func TestDisambiguatedNames_GoFlatMethodForm(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "(*Foo).Close", SelectionRange: protocol.Range{Start: protocol.Position{Line: 3, Character: 20}}},
		{Name: "(*Bar).Close", SelectionRange: protocol.Range{Start: protocol.Position{Line: 9, Character: 20}}},
	}
	got := disambiguatedNames(syms, syms)
	want := []string{"Foo.Close", "Bar.Close"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("candidate[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

// TestDisambiguatedNames_NestedParentForm covers the Python/Java-style nested
// shape: a method that is a Child of a class symbol yields "Class.method".
func TestDisambiguatedNames_NestedParentForm(t *testing.T) {
	method := protocol.DocumentSymbol{Name: "close", SelectionRange: protocol.Range{Start: protocol.Position{Line: 5, Character: 8}}}
	syms := []protocol.DocumentSymbol{
		{Name: "Widget", Children: []protocol.DocumentSymbol{method}},
	}
	matches := []protocol.DocumentSymbol{method}
	got := disambiguatedNames(syms, matches)
	if len(got) != 1 || got[0] != "Widget.close" {
		t.Errorf("got %v, want [\"Widget.close\"]", got)
	}
}

// TestDisambiguatedNames_TopLevelDuplicateFallsBackToLine covers two
// unrelated top-level symbols sharing a name with no enclosing type: the
// fallback names the line so the list is still total and actionable.
func TestDisambiguatedNames_TopLevelDuplicateFallsBackToLine(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}}},
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 8, Character: 5}}},
	}
	got := disambiguatedNames(syms, syms)
	for i, wantLine := range []int{3, 9} {
		if !strings.Contains(got[i], "line "+strconv.Itoa(wantLine)) {
			t.Errorf("candidate[%d] = %q, want it to mention line %d", i, got[i], wantLine)
		}
	}
}
