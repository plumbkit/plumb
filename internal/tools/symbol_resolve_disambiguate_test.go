package tools

import (
	"strconv"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// assertRoundTrips is the MANDATORY round-trip guard (PLAN-363 review round 1
// — both blocking defects were candidate strings that never got fed back
// through the real resolver): for every candidate disambiguatedNames(syms,
// matches) emits with a non-empty SymbolName, re-resolving that SymbolName
// through resolveSymbolsByName — the exact call a caller's retry makes — must
// yield EXACTLY ONE match, and it must BE the original match (same Name and
// SelectionRange.Start), never a different symbol or an ambiguous re-hit.
func assertRoundTrips(t *testing.T, syms, matches []protocol.DocumentSymbol) []disambiguatedCandidate {
	t.Helper()
	cands := disambiguatedNames(syms, matches)
	if len(cands) != len(matches) {
		t.Fatalf("got %d candidates for %d matches", len(cands), len(matches))
	}
	for i, c := range cands {
		if c.SymbolName == "" {
			continue // no proven name for this match — the caller falls back to line/character, nothing to round-trip
		}
		resolved := resolveSymbolsByName(syms, c.SymbolName)
		if len(resolved) != 1 {
			t.Errorf("candidate[%d] SymbolName %q resolves to %d symbols, want exactly 1 (the round-trip contract)", i, c.SymbolName, len(resolved))
			continue
		}
		if !sameSymbol(resolved[0], matches[i]) {
			t.Errorf("candidate[%d] SymbolName %q resolves to %+v, want the original match %+v",
				i, c.SymbolName, resolved[0], matches[i])
		}
	}
	return cands
}

// TestDisambiguatedNames_GoFlatMethodForm covers gopls' flat "(*Recv).Method"
// method-symbol shape mixed with a same-named nested match — a dotted query
// ("Foo.Close") is the only way resolveSymbolsByName ever surfaces a flat-form
// symbol as an ambiguous match (a plain query never matches "(*Recv).Method",
// since baseSymbolName strips everything before the leading "("), so this
// builds the mixed nested+flat tree that dotted query actually resolves,
// rather than hand-picking an unreachable matches slice.
func TestDisambiguatedNames_GoFlatMethodForm(t *testing.T) {
	nestedClose := protocol.DocumentSymbol{Name: "Close", SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 8}}}
	syms := []protocol.DocumentSymbol{
		{Name: "Foo", Children: []protocol.DocumentSymbol{nestedClose}},
		{Name: "(*Foo).Close", SelectionRange: protocol.Range{Start: protocol.Position{Line: 9, Character: 20}}},
	}
	matches := resolveSymbolsByName(syms, "Foo.Close")
	if len(matches) != 2 {
		t.Fatalf("setup: expected the dotted query to ambiguously match both the nested and flat forms, got %d", len(matches))
	}
	cands := assertRoundTrips(t, syms, matches)
	// The flat-form match's own literal query ("Foo.Close") does NOT round-trip
	// here: re-resolving it hits the nested form too (still 2 matches), so it
	// must NOT be offered as a symbol_name candidate — this is the mixed-form
	// trap provenSymbolName exists to catch.
	for _, c := range cands {
		if c.SymbolName == "Foo.Close" {
			t.Errorf("candidate %q is the original ambiguous query itself — it does not disambiguate anything", c.SymbolName)
		}
	}
}

// TestDisambiguatedNames_NestedParentForm covers the Python/Java-style nested
// shape: a method that is a Child of a class symbol yields "Class.method",
// and it round-trips to exactly that method.
func TestDisambiguatedNames_NestedParentForm(t *testing.T) {
	closeInWidget := protocol.DocumentSymbol{Name: "close", SelectionRange: protocol.Range{Start: protocol.Position{Line: 5, Character: 8}}}
	closeInGadget := protocol.DocumentSymbol{Name: "close", SelectionRange: protocol.Range{Start: protocol.Position{Line: 15, Character: 8}}}
	syms := []protocol.DocumentSymbol{
		{Name: "Widget", Children: []protocol.DocumentSymbol{closeInWidget}},
		{Name: "Gadget", Children: []protocol.DocumentSymbol{closeInGadget}},
	}
	matches := resolveSymbolsByName(syms, "close")
	if len(matches) != 2 {
		t.Fatalf("setup: expected 2 ambiguous \"close\" matches, got %d", len(matches))
	}
	cands := assertRoundTrips(t, syms, matches)
	want := []string{"Widget.close", "Gadget.close"}
	for i, w := range want {
		if cands[i].SymbolName != w {
			t.Errorf("candidate[%d].SymbolName = %q, want %q", i, cands[i].SymbolName, w)
		}
	}
}

// TestDisambiguatedNames_DeeplyNestedFallsBackNoFakeName is the BLOCKING-2
// regression guard: a match nested two levels deep (Outer > Middle > target)
// has an enclosing-symbol candidate ("Middle.target"), but Middle is itself
// nested inside Outer, not top-level — and resolveSymbolsByName's dotted
// resolver only scans TOP-LEVEL parents. The old code emitted "Middle.target"
// anyway; it must now emit NO SymbolName (empty), because that candidate does
// not resolve back to the match (proven: it does not resolve to anything).
func TestDisambiguatedNames_DeeplyNestedFallsBackNoFakeName(t *testing.T) {
	target1 := protocol.DocumentSymbol{Name: "run", SelectionRange: protocol.Range{Start: protocol.Position{Line: 4, Character: 4}}}
	target2 := protocol.DocumentSymbol{Name: "run", SelectionRange: protocol.Range{Start: protocol.Position{Line: 14, Character: 4}}}
	syms := []protocol.DocumentSymbol{
		{Name: "Outer", Children: []protocol.DocumentSymbol{
			{Name: "Middle", Children: []protocol.DocumentSymbol{target1}},
		}},
		{Name: "OtherOuter", Children: []protocol.DocumentSymbol{
			{Name: "Middle", Children: []protocol.DocumentSymbol{target2}},
		}},
	}
	matches := resolveSymbolsByName(syms, "run")
	if len(matches) != 2 {
		t.Fatalf("setup: expected 2 ambiguous \"run\" matches, got %d", len(matches))
	}
	// Sanity: the naive (unverified) "Middle.run" candidate does NOT resolve —
	// pins the exact defect this test guards against.
	if resolved := resolveSymbolsByName(syms, "Middle.run"); len(resolved) != 0 {
		t.Fatalf("setup invariant broken: \"Middle.run\" now resolves to %d symbols; this test needs it to resolve to 0", len(resolved))
	}
	cands := assertRoundTrips(t, syms, matches) // still must hold: any non-empty candidate round-trips
	for i, c := range cands {
		if c.SymbolName != "" {
			t.Errorf("candidate[%d] = %+v: a depth-2-nested match must have NO proven symbol_name, got %q", i, c, c.SymbolName)
		}
	}
}

// TestDisambiguatedNames_TopLevelDuplicateFallsBackNoFakeName is the
// BLOCKING-1 regression guard: two unrelated top-level symbols sharing a name
// with no enclosing type get NO symbol_name candidate (previously the code
// fabricated "Foo (line N)", which rename_symbol then told the caller to
// retry with — and it re-erred, since "Foo (line N)" is not a symbol_name
// resolveSymbolsByName accepts).
func TestDisambiguatedNames_TopLevelDuplicateFallsBackNoFakeName(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}}},
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 8, Character: 5}}},
	}
	matches := resolveSymbolsByName(syms, "Foo")
	cands := assertRoundTrips(t, syms, matches)
	for i, c := range cands {
		if c.SymbolName != "" {
			t.Errorf("candidate[%d].SymbolName = %q, want \"\" (no fake candidate for an undisambiguable top-level duplicate)", i, c.SymbolName)
		}
	}
	// The old fake-candidate string ("Foo (line 3)") must never appear as
	// something that LOOKS like a proposed symbol_name in the resolveSymbolsByName
	// sense — it is not a valid candidate string in this design at all.
	if _, ok := provenSymbolName(syms, matches[0]); ok {
		t.Error("provenSymbolName unexpectedly proved a name for an undisambiguable top-level duplicate")
	}
}

// TestFormatDisambiguation_MixedCandidates asserts the rendered message uses
// the proven symbol_name where one exists and an explicit line/character
// fallback — never a fabricated name — where none does.
func TestFormatDisambiguation_MixedCandidates(t *testing.T) {
	cands := []disambiguatedCandidate{
		{Name: "close", Line: 5, Character: 8, SymbolName: "Widget.close"},
		{Name: "Foo", Line: 2, Character: 5, SymbolName: ""},
	}
	got := formatDisambiguation(cands)
	if !strings.Contains(got, "Widget.close") {
		t.Errorf("expected the proven candidate in the message, got: %s", got)
	}
	if !strings.Contains(got, "line 3") || !strings.Contains(got, "line:2") || !strings.Contains(got, "character:5") {
		t.Errorf("expected an explicit line/character fallback for the unresolvable candidate, got: %s", got)
	}
	// The fallback text must not itself look like a pasteable symbol_name — it
	// must be clearly marked as non-copy-pasteable prose (BLOCKING-1 guard).
	if !strings.Contains(got, "no unique symbol_name") {
		t.Errorf("expected the fallback to say it has no unique symbol_name, got: %s", got)
	}
}

// TestFormatDisambiguation_AllProven covers the common case, string-joined and
// directly copy-pasteable.
func TestFormatDisambiguation_AllProven(t *testing.T) {
	cands := []disambiguatedCandidate{
		{SymbolName: "Widget.close"},
		{SymbolName: "Gadget.close"},
	}
	got := formatDisambiguation(cands)
	if got != "Widget.close; Gadget.close" {
		t.Errorf("got %q, want \"Widget.close; Gadget.close\"", got)
	}
}

// TestDisambiguatedCandidate_LineCharacterIsUsableAsIs pins that the fallback
// coordinates are literally what a caller can pass as line/character (0-based,
// matching every other position parameter in this package) — a round-trip
// guard for the OTHER retry path (line/character instead of symbol_name).
func TestDisambiguatedCandidate_LineCharacterIsUsableAsIs(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}}},
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 8, Character: 5}}},
	}
	matches := resolveSymbolsByName(syms, "Foo")
	cands := disambiguatedNames(syms, matches)
	for i, c := range cands {
		if c.Line != matches[i].SelectionRange.Start.Line || c.Character != matches[i].SelectionRange.Start.Character {
			t.Errorf("candidate[%d] line/character = %d/%d, want the match's own SelectionRange.Start %d/%d",
				i, c.Line, c.Character, matches[i].SelectionRange.Start.Line, matches[i].SelectionRange.Start.Character)
		}
	}
}

// TestDisambiguatedNames_TopLevelDuplicateFallsBackToLine (legacy name kept
// for the message-format guard, now asserting the accurate line/character
// fallback text — the OLD fake-candidate string is covered by
// TestDisambiguatedNames_TopLevelDuplicateFallsBackNoFakeName above).
func TestDisambiguatedNames_TopLevelDuplicateFallsBackToLine(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}}},
		{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 8, Character: 5}}},
	}
	matches := resolveSymbolsByName(syms, "Foo")
	cands := disambiguatedNames(syms, matches)
	got := formatDisambiguation(cands)
	for _, wantLine := range []int{3, 9} {
		if !strings.Contains(got, "line "+strconv.Itoa(wantLine)) {
			t.Errorf("message %q missing line %d", got, wantLine)
		}
	}
}
