package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// genericReceiverSrc has two methods named Run: A's value-receiver one first,
// and the pointer-receiver method of the generic type S second. It is the shape
// that made move_symbol move the wrong method (PR #559 review): the topology
// index could not express S[T] as a receiver, so "S[T]/Run" matched no parent
// and the resolver quietly fell back to the first node named Run — A's.
const genericReceiverSrc = "package demo\n\n" +
	"type A struct{}\n\n" +
	"type S[T any] struct{}\n\n" +
	"func (a A) Run() int { return 1 }\n\n" +
	"func (s *S[T]) Run() int { return 2 }\n\n" +
	"func Free() {}\n"

func extractGoNodes(t *testing.T, src string) []topology.Node {
	t.Helper()
	nodes, _, err := goext.New().Extract(context.Background(), "demo.go", []byte(src))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes
}

// TestTopologyNodeByPath_GoMethodsByReceiver: a Recv/Method name_path resolves
// the method of THAT receiver — the value receiver, the pointer receiver, and a
// generic one under either spelling of its type parameters.
func TestTopologyNodeByPath_GoMethodsByReceiver(t *testing.T) {
	nodes := extractGoNodes(t, genericReceiverSrc)
	for _, tc := range []struct {
		path, wantQualified string
	}{
		{"A/Run", "(A).Run"},
		{"S/Run", "(*S).Run"},
		{"S[T]/Run", "(*S).Run"},
	} {
		n := topologyNodeByPath(nodes, tc.path)
		if n == nil {
			t.Errorf("%q: not resolved, want %s", tc.path, tc.wantQualified)
			continue
		}
		if n.Qualified != tc.wantQualified {
			t.Errorf("%q resolved %s, want %s", tc.path, n.Qualified, tc.wantQualified)
		}
	}
}

// TestTopologyNodeByPath_NoParentMatchIsNotFound is the root cause of the wrong
// move: a multi-segment path whose parent segment matches nothing used to return
// the FIRST node with the leaf's name. Every miss must now be a miss.
func TestTopologyNodeByPath_NoParentMatchIsNotFound(t *testing.T) {
	nodes := extractGoNodes(t, genericReceiverSrc)
	for _, path := range []string{
		"Nope/Run",  // no such parent anywhere
		"A/Free",    // Free exists, but not under A
		"S/Free",    // likewise for the generic type
		"Free/Run",  // a function is not a receiver
		"B[T]/Run",  // an unknown generic receiver
		"/Run",      // an empty parent segment is a malformed path, not a wildcard
		"A//Run",    // likewise in the middle
		"A/S/Nope",  // the leaf does not exist at all
		"S[T]/Free", // a generic parent with no such method
	} {
		if n := topologyNodeByPath(nodes, path); n != nil {
			t.Errorf("%q resolved %s (line %d), want not found — a path whose parent matches nothing must never fall back to a same-named node",
				path, n.Qualified, n.StartLine)
		}
	}
}

// TestTopologyNodeByPath_TypeParametersAreStrippedFromTheParent: every spelling
// of a generic receiver names the same type, so "S[K, V]/Run" is S's Run. Pinned
// so the strip is a decision, not an accident.
func TestTopologyNodeByPath_TypeParametersAreStrippedFromTheParent(t *testing.T) {
	nodes := extractGoNodes(t, genericReceiverSrc)
	if n := topologyNodeByPath(nodes, "S[K, V]/Run"); n == nil || n.Qualified != "(*S).Run" {
		t.Errorf("S[K, V]/Run = %+v, want S's method (*S).Run", n)
	}
}

// TestTopologyNodeByPath_PlainNameKeepsFirstMatch: a single-segment name keeps
// the behaviour every caller already relies on — the first node of that name.
func TestTopologyNodeByPath_PlainNameKeepsFirstMatch(t *testing.T) {
	nodes := extractGoNodes(t, genericReceiverSrc)
	n := topologyNodeByPath(nodes, "Run")
	if n == nil || n.Qualified != "(A).Run" {
		t.Fatalf("plain Run = %+v, want the first node named Run, (A).Run", n)
	}
	if topologyNodeByPath(nodes, "Missing") != nil {
		t.Error("an unknown plain name must not resolve")
	}
}

// TestTopologyNodeByPath_ParentByContainment covers the extractors that do not
// qualify a nested member (Python, Java, Rust, Kotlin ... record Qualified as
// the bare name): their parent is evidenced by the span that encloses the
// member, not by Qualified. Without it, requiring a parent match would refuse
// every Class/method path they resolve today — and the old first-match fallback
// answered "Other/run" with the method of Greeter.
func TestTopologyNodeByPath_ParentByContainment(t *testing.T) {
	node := func(kind topology.NodeKind, name string, start, end int) topology.Node {
		return topology.Node{Kind: kind, Name: name, Qualified: name, StartLine: start, EndLine: end}
	}
	nodes := []topology.Node{
		node(topology.KindType, "Greeter", 1, 10),
		node(topology.KindMethod, "run", 3, 5),
		node(topology.KindType, "Other", 12, 20),
		node(topology.KindMethod, "run", 14, 16),
		node(topology.KindFunction, "helper", 22, 24),
	}
	for _, tc := range []struct {
		path      string
		wantStart int // 0: not found
	}{
		{"Greeter/run", 3},
		{"Other/run", 14},
		{"Nope/run", 0},
		{"Greeter/helper", 0}, // helper exists, but outside Greeter
		{"helper/run", 0},     // a node that encloses nothing is no parent
	} {
		n := topologyNodeByPath(nodes, tc.path)
		switch {
		case tc.wantStart == 0 && n != nil:
			t.Errorf("%q resolved the node at line %d, want not found", tc.path, n.StartLine)
		case tc.wantStart != 0 && (n == nil || n.StartLine != tc.wantStart):
			t.Errorf("%q = %+v, want the node at line %d", tc.path, n, tc.wantStart)
		}
	}

	// A member never names ITSELF as its parent: "run/run" is not found when no
	// other node called run encloses one.
	if n := topologyNodeByPath(nodes[:2], "run/run"); n != nil {
		t.Errorf("run/run resolved the node at line %d: a node is not its own parent", n.StartLine)
	}
}

// gopls lists methods flat — "(*S[T]).Run", never nested under S — and spells a
// generic receiver with its type parameters.
func genericFlatSymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		{Name: "A", Kind: protocol.SKStruct},
		{Name: "S", Kind: protocol.SKStruct},
		{Name: "(A).Run", Kind: protocol.SKMethod, SelectionRange: protocol.Range{Start: protocol.Position{Line: 6, Character: 12}}},
		{Name: "(*S[T]).Run", Kind: protocol.SKMethod, SelectionRange: protocol.Range{Start: protocol.Position{Line: 8, Character: 16}}},
		{Name: "Container", Kind: protocol.SKClass, Children: []protocol.DocumentSymbol{
			{Name: "size", Kind: protocol.SKMethod},
		}},
	}
}

// TestFindSymbolByPath_ReceiverSlashMethod: the name_path the move_symbol
// refusal offers resolves against the language server's own flat symbols, so it
// works with no topology index wired — the hint no longer depends on it.
func TestFindSymbolByPath_ReceiverSlashMethod(t *testing.T) {
	syms := genericFlatSymbols()
	for _, tc := range []struct {
		path, want string
	}{
		{"A/Run", "(A).Run"},
		{"S/Run", "(*S[T]).Run"},
		{"S[T]/Run", "(*S[T]).Run"},
		{"Container/size", "size"}, // a nested symbol still resolves the way it always did
	} {
		got := findSymbolByPath(syms, tc.path)
		if got == nil || got.Name != tc.want {
			t.Errorf("%q = %+v, want %s", tc.path, got, tc.want)
		}
	}
	for _, path := range []string{"Nope/Run", "A/Nope", "S/Run/Extra", "Container/Run", "/Run"} {
		if got := findSymbolByPath(syms, path); got != nil {
			t.Errorf("%q resolved %s, want not found", path, got.Name)
		}
	}
}

// TestMoveNamePaths_GenericReceiverOffersTheStrippedReceiver: the hint must be
// a path the resolvers can follow, so the type parameters come off the receiver.
func TestMoveNamePaths_GenericReceiverOffersTheStrippedReceiver(t *testing.T) {
	syms := genericFlatSymbols()
	got := moveNamePaths(syms, []protocol.DocumentSymbol{syms[2], syms[3]})
	if want := []string{"A/Run", "S/Run"}; !slices.Equal(got, want) {
		t.Fatalf("moveNamePaths = %q, want %q", got, want)
	}
}

// TestMoveNamePaths_OffersOnlyWhatRoundTrips: a hint is offered only when the
// resolver a retry will call returns exactly that match. A symbol nested two
// deep names its parent, but findSymbolByPath follows a path from the top
// level, so "Mid/Run" would answer "not found" — the generic hint is honest, a
// path that errors is not.
func TestMoveNamePaths_OffersOnlyWhatRoundTrips(t *testing.T) {
	deep := protocol.DocumentSymbol{
		Name: "Run", Kind: protocol.SKMethod,
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 3, Character: 4}},
	}
	syms := []protocol.DocumentSymbol{
		{Name: "Outer", Kind: protocol.SKClass, Children: []protocol.DocumentSymbol{
			{Name: "Mid", Kind: protocol.SKClass, Children: []protocol.DocumentSymbol{deep}},
		}},
		{Name: "(A).Run", Kind: protocol.SKMethod, SelectionRange: protocol.Range{Start: protocol.Position{Line: 9, Character: 12}}},
	}
	if got := moveNamePaths(syms, []protocol.DocumentSymbol{deep, syms[1]}); got != nil {
		t.Errorf("moveNamePaths = %q, want nil: Mid/Run does not resolve, so no list is offered", got)
	}
}

// TestMoveNamePaths_EveryOfferedPathResolvesToItsMatch guards the proof from the
// other side: each path offered is one findSymbolByPath resolves to its match.
func TestMoveNamePaths_EveryOfferedPathResolvesToItsMatch(t *testing.T) {
	syms := genericFlatSymbols()
	matches := []protocol.DocumentSymbol{syms[2], syms[3]}
	paths := moveNamePaths(syms, matches)
	if len(paths) != len(matches) {
		t.Fatalf("moveNamePaths = %q for %d matches", paths, len(matches))
	}
	for i, p := range paths {
		got := findSymbolByPath(syms, p)
		if got == nil || !sameSymbol(*got, matches[i]) {
			t.Errorf("offered %q does not resolve to %s: %+v", p, matches[i].Name, got)
		}
		if strings.ContainsAny(p, "[]") {
			t.Errorf("offered %q still carries type parameters", p)
		}
	}
}
