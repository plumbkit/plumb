package tools

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// symbol_path_chain_test.go pins the parent rule of topologyNodesByPath (PR #559
// round-3 review). A Parent/Name path names a declaration whose DIRECT parent is
// Parent — and for P1/P2/Name, whose parent is P2 whose parent is P1 — never one
// that merely sits somewhere inside Parent. The round-2 rule accepted any
// enclosing node of that name and looked at the last parent segment only, so
// Outer/run answered with Inner's run, and Wrong/Inner/run answered although
// there is no Wrong.

// pathCase is one name_path and the line of the one node it must resolve to.
type pathCase struct {
	path      string
	wantStart int // 0: refused — no node, or more than one, answers the path
}

// extractGraph runs ex over src and returns the nodes and edges it draws.
func extractGraph(t *testing.T, ex topology.Extractor, file, src string) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := ex.Extract(context.Background(), file, []byte(src))
	if err != nil {
		t.Fatalf("Extract %s: %v", file, err)
	}
	return nodes, edges
}

// soleNode is the node path names, or nil when none does or several do.
func soleNode(nodes []topology.Node, edges []topology.Edge, path string) *topology.Node {
	if m := topologyNodesByPath(nodes, edges, path); len(m) == 1 {
		return m[0]
	}
	return nil
}

func checkPaths(t *testing.T, nodes []topology.Node, edges []topology.Edge, cases []pathCase) {
	t.Helper()
	for _, tc := range cases {
		n := soleNode(nodes, edges, tc.path)
		switch {
		case tc.wantStart == 0 && n != nil:
			t.Errorf("%q resolved the node at line %d (%s), want it refused", tc.path, n.StartLine, n.Qualified)
		case tc.wantStart != 0 && n == nil:
			t.Errorf("%q refused, want the node at line %d (matches: %d)", tc.path, tc.wantStart, len(topologyNodesByPath(nodes, edges, tc.path)))
		case tc.wantStart != 0 && n.StartLine != tc.wantStart:
			t.Errorf("%q resolved the node at line %d, want line %d", tc.path, n.StartLine, tc.wantStart)
		}
	}
}

// nestedMembers is the shape of the round-3 repro: Inner, nested in Outer, has a
// run of its own at line 3, and Outer has its own run at line 5.
func nestedMembersCases() []pathCase {
	return []pathCase{
		{"Outer/run", 5},         // Outer's OWN run — never Inner's, which sits deeper
		{"Inner/run", 3},         // Inner is the direct parent of the run at line 3
		{"Outer/Inner/run", 3},   // the whole chain, each step a direct child
		{"Outer/Inner", 2},       // a nested class is a member of its parent too
		{"Wrong/Inner/run", 0},   // there is no Wrong: the chain is checked to its root
		{"Wrong/Outer/run", 0},   // likewise a wrong root above a real parent
		{"Outer/Wrong/run", 0},   // a wrong middle segment
		{"Inner/Outer/run", 0},   // the chain read backwards
		{"Outer/Outer/run", 0},   // a parent is not its own child
		{"Outer/Inner/run/x", 0}, // nothing is below run
	}
}

// TestTopologyPath_ParentIsDirectNotTransitive: the first repro, on both real
// extractors that record a member's Qualified as its bare name, so only the
// enclosing span can say whose member it is.
func TestTopologyPath_ParentIsDirectNotTransitive(t *testing.T) {
	const py = "class Outer:\n" + // 1
		"    class Inner:\n" + // 2
		"        def run(self):\n" + // 3
		"            return 1\n" + // 4
		"    def run(self):\n" + // 5
		"        return 2\n" // 6
	const java = "class Outer {\n" + // 1
		"    class Inner {\n" + // 2
		"        void run() {}\n" + // 3
		"    }\n" + // 4
		"    void run() {}\n" + // 5
		"}\n" // 6
	for _, tc := range []struct {
		name string
		ex   topology.Extractor
		file string
		src  string
	}{
		{"python", treesitter.NewPython(), "a.py", py},
		{"java", treesitter.NewJava(), "A.java", java},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, edges := extractGraph(t, tc.ex, tc.file, tc.src)
			checkPaths(t, nodes, edges, nestedMembersCases())
		})
	}
}

// TestTopologyPath_ParentWithNoMemberOfItsOwnIsRefused: Outer holds no run — only
// Inner does — so Outer/run names nothing. This is what replace_symbol_body
// did on a language server that said "not found": it edited Inner's run.
func TestTopologyPath_ParentWithNoMemberOfItsOwnIsRefused(t *testing.T) {
	const py = "class Outer:\n" + // 1
		"    class Inner:\n" + // 2
		"        def run(self):\n" + // 3
		"            return 1\n" // 4
	const java = "class Outer {\n" + // 1
		"    class Inner {\n" + // 2
		"        void run() {}\n" + // 3
		"    }\n" + // 4
		"}\n" // 5
	for _, tc := range []struct {
		name string
		ex   topology.Extractor
		file string
		src  string
	}{
		{"python", treesitter.NewPython(), "a.py", py},
		{"java", treesitter.NewJava(), "A.java", java},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, edges := extractGraph(t, tc.ex, tc.file, tc.src)
			checkPaths(t, nodes, edges, []pathCase{
				{"Outer/run", 0},
				{"Inner/run", 3},
				{"Outer/Inner/run", 3},
			})
		})
	}
}

// TestTopologyPath_SameNameAtTwoDepthsIsAmbiguous: a class Foo nested in a class
// Foo, each with a run. Both runs are the direct child of a Foo, so Foo/run names
// two declarations and is refused rather than resolved to the first; the full
// chain says which one is meant.
func TestTopologyPath_SameNameAtTwoDepthsIsAmbiguous(t *testing.T) {
	nodes, edges := extractGraph(t, treesitter.NewPython(), "a.py",
		"class Foo:\n"+ // 1
			"    class Foo:\n"+ // 2
			"        def run(self):\n"+ // 3
			"            return 1\n"+ // 4
			"    def run(self):\n"+ // 5
			"        return 2\n") // 6
	if got := len(topologyNodesByPath(nodes, edges, "Foo/run")); got != 2 {
		t.Errorf("Foo/run matched %d declarations, want 2 (it is ambiguous)", got)
	}
	checkPaths(t, nodes, edges, []pathCase{
		{"Foo/run", 0},
		{"Foo/Foo/run", 3},
	})
}

// TestTopologyPath_OverloadsAreAmbiguous: two declarations with one name in one
// parent cannot be told apart by a name_path, so the fallback names neither.
func TestTopologyPath_OverloadsAreAmbiguous(t *testing.T) {
	nodes, edges := extractGraph(t, treesitter.NewJava(), "A.java",
		"class Foo {\n"+ // 1
			"    void add(int a) {}\n"+ // 2
			"    void add(String s) {}\n"+ // 3
			"    void sub() {}\n"+ // 4
			"}\n") // 5
	if got := len(topologyNodesByPath(nodes, edges, "Foo/add")); got != 2 {
		t.Errorf("Foo/add matched %d declarations, want 2", got)
	}
	checkPaths(t, nodes, edges, []pathCase{{"Foo/add", 0}, {"Foo/sub", 4}})
}

// TestTopologyPath_QualifiedNamePathNeedsTheExactChain: an extractor that does
// qualify a member (A.Foo.run) is evidence for a path that spells out THAT chain
// and no other. "B/Foo/run" used to resolve to A's run because Foo, the last
// parent segment, appeared somewhere in A.Foo.run. The spans here enclose
// nothing, so the Qualified name is the only evidence there is.
func TestTopologyPath_QualifiedNamePathNeedsTheExactChain(t *testing.T) {
	nodes := []topology.Node{
		{Kind: topology.KindMethod, Name: "run", Qualified: "A.Foo.run", StartLine: 2, EndLine: 2},
		{Kind: topology.KindMethod, Name: "run", Qualified: "B.Foo.run", StartLine: 5, EndLine: 5},
	}
	checkPaths(t, nodes, nil, []pathCase{
		{"A/Foo/run", 2},
		{"B/Foo/run", 5},
		{"C/Foo/run", 0}, // not A, not B
		{"Foo/run", 0},   // a suffix of the chain is not the chain
		{"B/run", 0},     // nor is a prefix with Foo left out
		{"Foo/B/run", 0}, // nor the same names in another order
	})
}

// TestTopologyPath_TypeScriptNamespacesAreNotEvidence: the TypeScript extractor
// records no node for a namespace, and a member's Qualified is its bare name, so
// nothing says which namespace's Foo a run belongs to. B/Foo/run therefore
// resolves to nothing — never to A's run, which is what the round-2 rule did —
// and Foo/run, which three classes answer, is refused as ambiguous.
func TestTopologyPath_TypeScriptNamespacesAreNotEvidence(t *testing.T) {
	nodes, edges := extractGraph(t, treesitter.NewTypeScript(), "a.ts",
		"namespace A { export class Foo { run() {} } }\n"+
			"namespace B { export class Foo { run() {} } }\n"+
			"class Foo { run() {} }\n")
	if got := len(topologyNodesByPath(nodes, edges, "Foo/run")); got != 3 {
		t.Errorf("Foo/run matched %d declarations, want 3 (the three classes named Foo)", got)
	}
	checkPaths(t, nodes, edges, []pathCase{
		{"B/Foo/run", 0},
		{"A/Foo/run", 0},
		{"Foo/run", 0},
	})
}

// rustParentsSrc declares two types with a run each, a trait, and a trait impl.
// An impl block is not a node, so the only link from a method to its type is the
// extractor's own containment edge.
const rustParentsSrc = "struct Foo;\n" + // 1
	"struct Bar;\n" + // 2
	"impl Foo {\n" + // 3
	"    fn run(&self) {}\n" + // 4
	"}\n" + // 5
	"impl Bar {\n" + // 6
	"    fn run(&self) {}\n" + // 7
	"}\n" + // 8
	"trait T { fn go(&self); }\n" + // 9
	"impl T for Foo {\n" + // 10
	"    fn go(&self) {}\n" + // 11
	"}\n" // 12

// TestTopologyPath_RustMethodsResolveThroughTheirImpl: Type/method resolves to
// the method inside the impl of THAT type, for an inherent impl and a trait impl
// alike. Round 2 refused every one of these — the method's Qualified is the bare
// name and no node named Foo encloses it — which broke a path that had always
// worked.
func TestTopologyPath_RustMethodsResolveThroughTheirImpl(t *testing.T) {
	nodes, edges := extractGraph(t, treesitter.NewRust(), "a.rs", rustParentsSrc)
	checkPaths(t, nodes, edges, []pathCase{
		{"Foo/run", 4},
		{"Bar/run", 7},
		{"Foo/go", 11}, // the method of `impl T for Foo`, not the trait's signature at line 9
		{"T/go", 9},    // the trait's own signature
		{"Bar/go", 0},  // Bar implements nothing
		{"T/run", 0},   // the trait declares no run
		{"Foo/Foo", 0},
		{"Nope/run", 0},
	})
}

// TestTopologyPath_RustImplOfAnUndeclaredTypeIsRefused: the impl-to-type link is
// a name match within the file, so an impl for a type the file does not declare
// links to nothing and its methods have no evidenced parent. Refusing is the
// conservative answer; the language server resolves it when it answers.
func TestTopologyPath_RustImplOfAnUndeclaredTypeIsRefused(t *testing.T) {
	nodes, edges := extractGraph(t, treesitter.NewRust(), "a.rs",
		"impl Missing {\n    fn run(&self) {}\n}\n")
	checkPaths(t, nodes, edges, []pathCase{{"Missing/run", 0}})
}

// TestTopologyPath_RustSameTypeNameInTwoModulesIsAmbiguous: the extractor links
// an impl to the LAST type of that name in the file, so two modules' Foo cannot
// be told apart and Foo/run is refused rather than answered with one of them.
func TestTopologyPath_RustSameTypeNameInTwoModulesIsAmbiguous(t *testing.T) {
	nodes, edges := extractGraph(t, treesitter.NewRust(), "a.rs",
		"mod a { pub struct Foo; impl Foo { pub fn run() {} } }\n"+
			"mod b { pub struct Foo; impl Foo { pub fn run() {} } }\n")
	checkPaths(t, nodes, edges, []pathCase{{"Foo/run", 0}, {"a/Foo/run", 0}})
}

// TestTopologyPath_PackageNodes: a package node that encloses a declaration by
// span — an Elixir module — is a parent like any other. One that only links the
// file's declarations to itself by containment edge — a C# file-scoped namespace
// (Go's package is covered by the receiver-forms test) — is the file's scope, not
// a parent a name_path addresses.
func TestTopologyPath_PackageNodes(t *testing.T) {
	nodes, edges := extractGraph(t, treesitter.NewElixir(), "a.ex",
		"defmodule Foo do\n  def run, do: 1\n  defp helper, do: 2\nend\ndefmodule Bar do\n  def run, do: 3\nend\n")
	checkPaths(t, nodes, edges, []pathCase{
		{"Foo/run", 2}, {"Bar/run", 6}, {"Foo/helper", 3}, {"Bar/helper", 0},
	})
	nodes, edges = extractGraph(t, treesitter.NewCSharp(), "a.cs",
		"namespace N;\npublic class Foo {\n    public void Run() {}\n}\n")
	checkPaths(t, nodes, edges, []pathCase{{"Foo/Run", 3}, {"N/Foo", 0}, {"N/Foo/Run", 0}})
}

// goReceiverFormsSrc has a generic type's pointer-receiver method (line 6) and a
// plain type's value-receiver one (line 7).
const goReceiverFormsSrc = "package p\n\n" +
	"type S[T any] struct{}\n" +
	"type A struct{}\n\n" +
	"func (s *S[T]) Run() {}\n" +
	"func (a A) Run() {}\n"

// TestTopologyPath_GoReceiverFormsAreNormalised: *S/Run, (*S)/Run and S[T]/Run
// name S's method however the receiver is spelled — the forms findSymbolByPath
// already accepts for the language server's flat symbols.
func TestTopologyPath_GoReceiverFormsAreNormalised(t *testing.T) {
	nodes, edges := extractGraph(t, goext.New(), "a.go", goReceiverFormsSrc)
	checkPaths(t, nodes, edges, []pathCase{
		{"S/Run", 6},
		{"*S/Run", 6},
		{"(*S)/Run", 6},
		{"S[T]/Run", 6},
		{"*S[T]/Run", 6},
		{"(*S[T])/Run", 6},
		{"A/Run", 7},
		{"*A/Run", 7},
		{"(A)/Run", 7},
		{"(*A)/Run", 7},
		{"B/Run", 0},
		{"*B/Run", 0},
		{"()/Run", 0},
		// Garbage is not a spelling of a receiver: an unclosed bracket is cut
		// nowhere, so it names no type.
		{"S[/Run", 0},
		{"S[T/Run", 0},
		{"[]S/Run", 0},
		// The package is the file's scope, not a parent a name_path addresses.
		{"p/Run", 0},
		{"p/A", 0},
	})
}

// TestGoReceiverForms_BothTiersAgree: the language server's flat symbols and the
// tree-sitter nodes resolve the same set of spellings, so a path that works warm
// keeps working when the server does not answer, and a refusal reads the same.
func TestGoReceiverForms_BothTiersAgree(t *testing.T) {
	nodes, edges := extractGraph(t, goext.New(), "a.go", goReceiverFormsSrc)
	method := func(name string, line uint32) protocol.DocumentSymbol {
		return protocol.DocumentSymbol{
			Name: name, Kind: protocol.SKMethod,
			SelectionRange: protocol.Range{Start: protocol.Position{Line: line}},
		}
	}
	syms := []protocol.DocumentSymbol{
		{Name: "S", Kind: protocol.SKStruct},
		{Name: "A", Kind: protocol.SKStruct},
		method("(*S[T]).Run", 5), method("(A).Run", 6),
	}
	for _, path := range []string{
		"S/Run", "*S/Run", "(*S)/Run", "S[T]/Run", "*S[T]/Run", "(*S[T])/Run",
		"A/Run", "*A/Run", "(A)/Run", "B/Run", "S[/Run", "S[T/Run", "[]S/Run", "S/Nope",
	} {
		lsp := findSymbolByPath(syms, path) != nil
		topo := soleNode(nodes, edges, path) != nil
		if lsp != topo {
			t.Errorf("%q: language-server tier resolves it = %v, tree-sitter tier = %v", path, lsp, topo)
		}
	}
}

// TestStripTypeParams: a type-parameter list is cut only when it is a complete,
// closed suffix. An unclosed bracket is garbage that must not be trimmed into a
// valid-looking type name ("S[" -> "S" made S[/Run resolve to S's method).
func TestStripTypeParams(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"S", "S"},
		{"S[T]", "S"},
		{"M[K, V]", "M"},
		{"S[A[B]]", "S"},
		{"S[", "S["},
		{"S[T", "S[T"},
		{"S[A[B]", "S[A[B]"},
		{"S[T]x", "S[T]x"},
		{"S[T][U]", "S[T][U]"},
		{"[]T", "[]T"},
		{"", ""},
	} {
		if got := stripTypeParams(tc.in); got != tc.want {
			t.Errorf("stripTypeParams(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
