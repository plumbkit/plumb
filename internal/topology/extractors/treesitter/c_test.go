package treesitter

import (
	"context"
	"slices"
	"strings"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// cSrc covers the shapes that distinguish C from a language with one spelling
// per idea: a typedef'd anonymous struct, a named struct, an enum, a
// function-like macro, a pointer-returning definition, and a prototype that is
// later defined (which must not double-count).
var cSrc = []byte(`#include <stdio.h>
#include "local.h"

#define MAX_ITEMS 10
#define SQUARE(x) ((x) * (x))

// A point in the plane.
typedef struct { int x; int y; } Point;

struct Node {
  int value;
  struct Node *next;
};

// Four members, not three, deliberately: the three-enumerator form hits a known
// gotreesitter defect (see TestC_EnumRecovery_*), and this fixture doubles as
// the parse-fidelity sample, which must isolate a grammar CASCADE rather than
// re-measure a bug already pinned by its own tests.
enum Colour { RED, GREEN, BLUE, ALPHA };

static const int LIMIT = 5;

int add(int a, int b);

char *render(struct Node *n);

int add(int a, int b) {
  int local = a + b;
  return trailing_helper(local);
}

char *render(struct Node *n) {
  return (char *)n;
}

int trailing_helper(int v) {
  return v;
}
`)

func cExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewC().Extract(context.Background(), "src/geom.c", cSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func cFind(t *testing.T, nodes []topology.Node, kind topology.NodeKind, name string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return n
		}
	}
	t.Fatalf("no %s named %q among %d nodes", kind, name, len(nodes))
	return topology.Node{}
}

func TestC_KindsExtracted(t *testing.T) {
	nodes, _ := cExtract(t)
	for _, w := range []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "stdio.h"},
		{topology.KindImport, "local.h"},
		{topology.KindConstant, "MAX_ITEMS"},
		{topology.KindFunction, "SQUARE"}, // function-like macros are called like functions
		{topology.KindType, "Point"},      // typedef of an anonymous struct
		{topology.KindType, "Node"},
		{topology.KindType, "Colour"},
		{topology.KindConstant, "RED"},
		{topology.KindConstant, "LIMIT"},
		{topology.KindVariable, "value"}, // a code type's member, never a KindField
		{topology.KindFunction, "add"},
		{topology.KindFunction, "trailing_helper"},
	} {
		cFind(t, nodes, w.kind, w.name)
	}
}

// A header is nothing but prototypes, so they have to be emitted or every .h
// file indexes empty. But a .c file that declares and then defines the same
// function must still report ONE symbol, not two — the reason prototypes are
// held back until the walk has seen the whole file.
func TestC_PrototypeAndDefinitionYieldOneSymbol(t *testing.T) {
	nodes, _ := cExtract(t)
	for _, name := range []string{"add", "render"} {
		count := 0
		for _, n := range nodes {
			if n.Kind == topology.KindFunction && n.Name == name {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%q yielded %d function nodes, want 1 (the prototype must defer to the definition)", name, count)
		}
	}
	// The surviving node must be the DEFINITION, not the prototype: only the
	// definition spans the body, which is what an outline and every byte-span
	// consumer needs.
	add := cFind(t, nodes, topology.KindFunction, "add")
	if !strings.Contains(string(cSrc[add.StartByte:add.EndByte]), "return trailing_helper") {
		t.Error("the retained `add` node is the prototype; it should be the definition, which spans the body")
	}
}

// A header-only translation unit is the common case for .h files, and it must
// not index as empty.
func TestC_HeaderOnlyPrototypesAreIndexed(t *testing.T) {
	hdr := []byte("#ifndef GEOM_H\n#define GEOM_H\n\nint add(int a, int b);\nvoid reset(void);\n\n#endif\n")
	nodes, _, err := NewC().Extract(context.Background(), "src/geom.h", hdr)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"add", "reset"} {
		cFind(t, nodes, topology.KindFunction, want)
	}
}

// C wraps a declared name in one node per piece of type syntax, so a
// pointer-returning function or an array member hides its identifier several
// levels down. Matching only the outermost node would silently miss most real
// declarations.
func TestC_PointerAndArrayDeclaratorsResolveTheirName(t *testing.T) {
	src := []byte("char *render(int n) { return 0; }\nstruct S { char *name; int vals[4]; };\n")
	nodes, _, err := NewC().Extract(context.Background(), "a.c", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cFind(t, nodes, topology.KindFunction, "render")
	cFind(t, nodes, topology.KindVariable, "name")
	cFind(t, nodes, topology.KindVariable, "vals")
}

// A member of a code type is a KindConstant or KindVariable by mutability, never
// a KindField — that kind is reserved for a key of a data-format file (a SQL
// column, a TOML key), per its doc comment on topology.KindField. The qualifier's
// POSITION decides for a pointer: `const char *p` is a mutable pointer to
// constant chars, `char *const p` is the constant one.
func TestC_MembersAreConstantOrVariableNeverField(t *testing.T) {
	src := []byte("struct S {\n  const int immut;\n  int mut;\n  const char *pstr;\n  char *const cptr;\n  volatile int vol;\n};\n")
	nodes, _, err := NewC().Extract(context.Background(), "a.c", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindField {
			t.Errorf("member %q emitted as KindField; a code type's member is a constant or a variable", n.Name)
		}
	}
	for _, want := range []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindConstant, "immut"},
		{topology.KindVariable, "mut"},
		{topology.KindVariable, "pstr"}, // const belongs to the pointee
		{topology.KindConstant, "cptr"}, // const belongs to the pointer itself
		{topology.KindVariable, "vol"},  // volatile is not immutable
	} {
		cFind(t, nodes, want.kind, want.name)
	}
}

func TestC_LocalsInsideFunctionsAreSuppressed(t *testing.T) {
	nodes, _ := cExtract(t)
	for _, n := range nodes {
		if n.Name == "local" {
			t.Errorf("a local declaration leaked into the index as %s %q", n.Kind, n.Qualified)
		}
	}
}

func TestC_ContainmentEdgeCertain(t *testing.T) {
	nodes, edges := cExtract(t)
	var node, value int64 = -1, -1
	for i, n := range nodes {
		if n.Kind == topology.KindType && n.Name == "Node" {
			node = int64(i)
		}
		if n.Kind == topology.KindVariable && n.Name == "value" {
			value = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == node && e.ToID == value {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("containment edge = %v/%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Error("struct Node must contain its member value")
}

func TestC_EnumeratorsBelongToTheirEnum(t *testing.T) {
	nodes, edges := cExtract(t)
	var colour, red int64 = -1, -1
	for i, n := range nodes {
		if n.Kind == topology.KindType && n.Name == "Colour" {
			colour = int64(i)
		}
		if n.Kind == topology.KindConstant && n.Name == "RED" {
			red = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == colour && e.ToID == red {
			return
		}
	}
	t.Error("enum Colour must contain enumerator RED")
}

func TestC_CallEdgeIntraFile(t *testing.T) {
	nodes, edges := cExtract(t)
	var add, helper int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindFunction && n.Name == "add":
			add = int64(i)
		case n.Kind == topology.KindFunction && n.Name == "trailing_helper":
			helper = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == add && e.ToID == helper {
			return
		}
	}
	t.Error("no call edge from add to trailing_helper")
}

func TestC_SignaturesOmitTheBody(t *testing.T) {
	nodes, _ := cExtract(t)
	sig := cFind(t, nodes, topology.KindFunction, "add").Signature
	if !strings.Contains(sig, "int add(int a, int b)") {
		t.Errorf("signature = %q, want the declaration head", sig)
	}
	if strings.Contains(sig, "return") {
		t.Errorf("signature = %q, must not include the body", sig)
	}
	if got := cFind(t, nodes, topology.KindFunction, "SQUARE").Signature; !strings.Contains(got, "(x)") {
		t.Errorf("macro signature = %q, want its parameter list", got)
	}
}

func TestC_ByteSpanReconstructsDeclaration(t *testing.T) {
	nodes, _ := cExtract(t)
	n := cFind(t, nodes, topology.KindType, "Node")
	if !n.HasBytes {
		t.Fatal("HasBytes false; every emitted node must carry its span")
	}
	if got := string(cSrc[n.StartByte:n.EndByte]); !strings.HasPrefix(got, "struct Node") {
		t.Errorf("span does not reconstruct the declaration:\n%s", got)
	}
}

func TestC_DocSpanCoversPrecedingComment(t *testing.T) {
	nodes, _ := cExtract(t)
	n := cFind(t, nodes, topology.KindType, "Point")
	if n.DocStartByte == 0 && n.DocEndByte == 0 {
		t.Fatal("expected a doc span for a typedef with a comment directly above it")
	}
	if got := string(cSrc[n.DocStartByte:n.DocEndByte]); !strings.Contains(got, "A point in the plane") {
		t.Errorf("doc span = %q, want the comment above the typedef", got)
	}
}

func TestC_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range []string{"", "// just a comment\n", "\n\n"} {
		nodes, edges, err := NewC().Extract(context.Background(), "a.c", []byte(src))
		if err != nil {
			t.Errorf("Extract(%q): %v", src, err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("Extract(%q) = %d nodes, %d edges; want none", src, len(nodes), len(edges))
		}
	}
}

func TestC_LanguageAndPath(t *testing.T) {
	nodes, _ := cExtract(t)
	if len(nodes) == 0 {
		t.Fatal("fixture produced no nodes; the loop below would be vacuous")
	}
	for _, n := range nodes {
		if n.Language != "c" {
			t.Errorf("node %q language = %q, want c", n.Name, n.Language)
		}
		if n.Path != "src/geom.c" {
			t.Errorf("node %q path = %q, want the path passed to Extract", n.Name, n.Path)
		}
	}
}

func TestC_Extensions(t *testing.T) {
	got := NewC().Extensions()
	for _, want := range []string{".c", ".h"} {
		if !slicesContains(got, want) {
			t.Errorf("Extensions() = %v, missing %q", got, want)
		}
	}
}

// TestC_EnumeratorsSurviveEveryArity pins that no enumerator is silently
// dropped at any arity.
//
// It succeeds a source-text recovery workaround. On gotreesitter v0.48.1 an
// enum with exactly three enumerators and no trailing comma parsed with its
// THIRD enumerator wrapped in an ERROR node rather than sitting in the
// enumerator_list, so a walk reading direct children saw only two and
// `enum Colour { RED, GREEN, BLUE }` silently lost BLUE. That was filed
// upstream as odvcencio/gotreesitter#667 and fixed in v0.49.0; the workaround
// (c_enum_recovery.go) was deleted at the v0.51.0 bump.
//
// A silently short enum is the failure that makes a Map worse than no Map — an
// agent trusts the two it sees and never learns of the third — so the shapes
// stay pinned here even though the recovery is gone.
func TestC_EnumeratorsSurviveEveryArity(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want []string
	}{
		{"enum E { A };\n", []string{"A"}},
		{"enum E { A, B };\n", []string{"A", "B"}},
		{"enum Colour { RED, GREEN, BLUE };\n", []string{"RED", "GREEN", "BLUE"}}, // was #667
		{"enum E { A, B, C, D };\n", []string{"A", "B", "C", "D"}},
		{"enum E { A = 1, B = 2, C = 3 };\n", []string{"A", "B", "C"}},
		{"typedef enum { RED, GREEN, BLUE } Colour;\n", []string{"RED", "GREEN", "BLUE"}}, // was #667
		{"enum E { A, B, C, };\n", []string{"A", "B", "C"}},
	} {
		nodes, _, err := NewC().Extract(context.Background(), "a.c", []byte(tc.src))
		if err != nil {
			t.Fatalf("Extract(%q): %v", tc.src, err)
		}
		got := map[string]topology.Node{}
		for _, n := range nodes {
			if n.Kind == topology.KindConstant {
				got[n.Name] = n
			}
		}
		for _, want := range tc.want {
			n, ok := got[want]
			if !ok {
				t.Errorf("Extract(%q) lost enumerator %q; got %d constants", tc.src, want, len(got))
				continue
			}
			// Consumers slice source with these spans, so every enumerator must
			// carry one and it must start at the name.
			if !n.HasBytes {
				t.Errorf("enumerator %q in %q has no byte span", want, tc.src)
				continue
			}
			if slice := tc.src[n.StartByte:n.EndByte]; !strings.HasPrefix(slice, want) {
				t.Errorf("enumerator %q in %q has span covering %q, which does not start with the name", want, tc.src, slice)
			}
		}
		if len(got) != len(tc.want) {
			t.Errorf("Extract(%q) = %d constants, want %d", tc.src, len(got), len(tc.want))
		}
	}
}

// TestC_EnumeratorsNeverFabricated keeps the counterweight the deleted recovery
// needed: a comma inside a comment, a macro argument list, a character or string
// literal, or a parenthesised expression does NOT separate enumerators.
//
// Reading enumerator names out of raw source once turned each of these into an
// INVENTED constant — `/* red, green */` became one named `green` spanning
// "green */ B", `MAX(x, y)` became one named `y` — each with a confidence-1.0
// containment edge claiming membership of the enum. A fabricated declaration is
// a missing one with the sign flipped, and a sweep counting parse errors and
// span validity sees neither. The grammar handles these now; the cases stay so
// that a future recovery shim cannot quietly reintroduce the fabrication.
func TestC_EnumeratorsNeverFabricated(t *testing.T) {
	for _, src := range []string{
		"enum E { A, /* red, green */ B, C };\n",
		"enum E { A, // one, two\n  B, C };\n",
		"enum E { A = MAX(x, y), B, C };\n",
		"enum E { A = ',x', B, C };\n",
		"enum E { A = \"x, y\"[0], B, C };\n",
		"enum E { A = (1, 2), B, C };\n",
		"enum E {\n  A, /* first,\n       still a comment */\n  B,\n  C\n};\n",
	} {
		nodes, edges, err := NewC().Extract(context.Background(), "a.c", []byte(src))
		if err != nil {
			t.Fatalf("Extract(%q): %v", src, err)
		}
		var got []string
		for i, n := range nodes {
			if n.Kind != topology.KindConstant {
				continue
			}
			got = append(got, n.Name)
			if slice := src[n.StartByte:n.EndByte]; !strings.HasPrefix(slice, n.Name) {
				t.Errorf("Extract(%q): enumerator %q spans %q, which does not start with the name", src, n.Name, slice)
			}
			for _, e := range edges {
				if e.Kind == topology.EdgeContains && e.ToID == int64(i) && e.Confidence != 1.0 {
					t.Errorf("Extract(%q): containment edge for %q has confidence %v", src, n.Name, e.Confidence)
				}
			}
		}
		if want := []string{"A", "B", "C"}; !slices.Equal(got, want) {
			t.Errorf("Extract(%q) constants = %v, want exactly %v", src, got, want)
		}
	}
}

// An enumerator reports ITS OWN lines, not the enclosing enum's. This caught the
// deleted recovery giving a recovered node the whole enum's L1-5 while its
// parsed siblings reported L2-2; it stays as an ordinary span guard.
func TestC_EnumeratorLineRangeIsItsOwn(t *testing.T) {
	src := []byte("enum E {\n  A,\n  B,\n  C\n};\n")
	nodes, _, err := NewC().Extract(context.Background(), "a.c", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for name, want := range map[string]int{"A": 2, "B": 3, "C": 4} {
		n := cFind(t, nodes, topology.KindConstant, name)
		if n.StartLine != want || n.EndLine != want {
			t.Errorf("enumerator %q = L%d-%d, want L%d-%d", name, n.StartLine, n.EndLine, want, want)
		}
	}
}

// TestC_ThreeEnumeratorsParseWithoutRecoveryNodes is the direct successor to the
// old TestC_EnumRecovery_TripwireForUpstreamFix. That tripwire asserted the
// defect was still present so the workaround could not be carried silently
// forever; this asserts the opposite — that the raw parse is clean — so a
// gotreesitter regression that brings #667 back fails here loudly rather than
// quietly shortening every three-member C enum in the index.
func TestC_ThreeEnumeratorsParseWithoutRecoveryNodes(t *testing.T) {
	lang := grammars.CLanguage()
	for _, src := range []string{
		"enum E { A, B, C };\n",
		"enum Colour { RED, GREEN, BLUE };\n",
		"typedef enum { RED, GREEN, BLUE } Colour;\n",
	} {
		parser := tsg.NewParser(lang)
		tree, err := parser.Parse([]byte(src))
		if err != nil || tree == nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		root := tree.RootNode()
		if hasMissingOrError(root) {
			t.Errorf("Parse(%q) carries an ERROR or MISSING node — upstream #667 has regressed; "+
				"the third enumerator is no longer a direct child of enumerator_list and will be dropped", src)
		}
		// The walk reads DIRECT enumerator children, so pin that shape too: a
		// clean tree that nests the third enumerator elsewhere would still lose
		// it, and hasMissingOrError alone would not notice.
		if got := cDirectEnumerators(root, lang, []byte(src)); got != 3 {
			t.Errorf("Parse(%q) has %d direct enumerator children of enumerator_list, want 3", src, got)
		}
		tree.Release()
	}
}

// cDirectEnumerators counts the enumerator nodes that are DIRECT children of the
// first enumerator_list in the tree — the same nodes cWalk.addEnumerators reads.
func cDirectEnumerators(n *tsg.Node, lang *tsg.Language, src []byte) int {
	if n.Type(lang) == "enumerator_list" {
		count := 0
		for _, e := range n.Children() {
			if e.Type(lang) == "enumerator" {
				count++
			}
		}
		return count
	}
	for _, c := range n.Children() {
		if got := cDirectEnumerators(c, lang, src); got > 0 {
			return got
		}
	}
	return 0
}
