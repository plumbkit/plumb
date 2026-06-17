package tools

import (
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// TestNodeToDocSymbol_CharPreciseWhenSpansPresent proves a node carrying a
// byte-precise span yields a char-precise range (real start/end columns), and a
// node without spans falls back to the line-granular 0→end-of-line range.
func TestNodeToDocSymbol_CharPreciseWhenSpansPresent(t *testing.T) {
	lines := []string{"package p", "", "\tfunc Foo() {}", "trailing"}

	withSpan := topology.Node{
		Name: "Foo", Kind: topology.KindFunction,
		StartLine: 3, EndLine: 3,
		HasBytes: true, StartCol: 1, EndCol: 14,
	}
	ds := nodeToDocSymbol(withSpan, lines)
	if ds.Range.Start.Character != 1 {
		t.Errorf("precise start char = %d, want 1", ds.Range.Start.Character)
	}
	if ds.Range.End.Character != 14 {
		t.Errorf("precise end char = %d, want 14", ds.Range.End.Character)
	}
	if ds.Range.Start.Line != 2 || ds.Range.End.Line != 2 {
		t.Errorf("lines = (%d,%d), want (2,2)", ds.Range.Start.Line, ds.Range.End.Line)
	}

	noSpan := topology.Node{
		Name: "Foo", Kind: topology.KindFunction,
		StartLine: 3, EndLine: 3, // HasBytes false → line-granular fallback
	}
	dl := nodeToDocSymbol(noSpan, lines)
	if dl.Range.Start.Character != 0 {
		t.Errorf("fallback start char = %d, want 0", dl.Range.Start.Character)
	}
	// End char is the byte length of line index 2 ("\tfunc Foo() {}" = 14).
	if dl.Range.End.Character != 14 {
		t.Errorf("fallback end char = %d, want 14 (EOL of last line)", dl.Range.End.Character)
	}
}

// TestByteOffsetToPosition covers a multibyte case: byte columns, not rune
// columns, and out-of-range rejection.
func TestByteOffsetToPosition(t *testing.T) {
	content := []byte("café x\nfoo")
	// 'x' sits at byte offset 6 (c=1,a=1,f=1,é=2,space=1 → offset 6), line 0.
	pos, ok := byteOffsetToPosition(content, 6)
	if !ok {
		t.Fatal("offset 6 should be in range")
	}
	if pos.Line != 0 || pos.Character != 6 {
		t.Errorf("pos = (line %d, char %d), want (0, 6)", pos.Line, pos.Character)
	}
	// "café x" is 7 bytes, the '\n' is byte 7, so offset 8 is the start of line 1
	// ('f' in foo).
	pos2, ok := byteOffsetToPosition(content, 8)
	if !ok || pos2.Line != 1 || pos2.Character != 0 {
		t.Errorf("line-start pos = (line %d, char %d), want (1, 0)", pos2.Line, pos2.Character)
	}
	if _, ok := byteOffsetToPosition(content, 999); ok {
		t.Error("out-of-range offset should return ok=false")
	}
}
