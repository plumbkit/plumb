package treesitter

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var csharpSrc = []byte(`using System;
using static System.Math;
using Grid = System.Collections.Generic.Dictionary<string, int>;
global using System.Linq;

namespace Acme.Billing
{
    /// <summary>Issues invoices.</summary>
    [Serializable]
    public partial class Invoice<T> : Document, IPrintable where T : class, new()
    {
        private const int MaxLines = 500;
        private readonly ILogger _log;
        private int _counter, _retries;

        public string Number { get; set; }
        public decimal Total { get; }
        public DateTime Issued { get; init; }
        public string Label => "invoice";
        public event EventHandler Paid;

        public Invoice(ILogger log) { _log = log; }
        ~Invoice() { }

        public async Task<bool> Submit()
        {
            int Retry(int n) => n + 1;
            Retry(1);
            return Validate();
        }

        private bool Validate() => true;

        public static Invoice<T> operator +(Invoice<T> a, Invoice<T> b) => a;
        public static explicit operator string(Invoice<T> i) => "";
        public string this[int line] => "";

        public delegate void Handler(int code);

        private class Line { public int Qty; }
    }

    public interface IPrintable { void Print(); }

    public enum Status { Draft, Sent = 2, Paid }

    public record Receipt(int Id);

    public struct Money { public decimal Amount; }
}

public static class Extensions
{
    public static int Words(this string s) => s.Length;
}

public class InvoiceTests
{
    [Fact]
    public void SubmitsCleanly() { }

    [Theory]
    [InlineData(1)]
    public void HandlesData(int n) { }

    [Xunit.Fact]
    public void Qualified() { }

    public void Helper() { }
}
`)

func csharpExtract(t *testing.T, path string, src []byte) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewCSharp().Extract(context.Background(), path, src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func csharpNode(t *testing.T, nodes []topology.Node, kind topology.NodeKind, name string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return n
		}
	}
	t.Fatalf("kind=%s name=%q not found; got %v", kind, name, names(nodes, kind))
	return topology.Node{}
}

func TestCSharp_KindsExtracted(t *testing.T) {
	nodes, _ := csharpExtract(t, "src/Invoice.cs", csharpSrc)
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "System"},
		{topology.KindImport, "System.Math"},                                        // using static
		{topology.KindImport, "System.Collections.Generic.Dictionary<string, int>"}, // alias target
		{topology.KindImport, "System.Linq"},                                        // global using
		{topology.KindPackage, "Acme.Billing"},                                      // namespace → package
		{topology.KindClass, "Invoice"},
		{topology.KindClass, "Line"},    // nested type
		{topology.KindClass, "Status"},  // enum → class
		{topology.KindClass, "Receipt"}, // record → class
		{topology.KindClass, "Money"},   // struct → class
		{topology.KindType, "IPrintable"},
		{topology.KindType, "Handler"}, // delegate → type
		{topology.KindMethod, "Submit"},
		{topology.KindMethod, "Invoice"},         // constructor
		{topology.KindMethod, "~Invoice"},        // destructor
		{topology.KindMethod, "operator +"},      // operator
		{topology.KindMethod, "operator string"}, // conversion operator
		{topology.KindMethod, "this[]"},          // indexer
		{topology.KindMethod, "Print"},           // interface member
		{topology.KindMethod, "Words"},           // extension method
		{topology.KindFunction, "Retry"},         // local function
		{topology.KindConstant, "MaxLines"},      // const field
		{topology.KindConstant, "_log"},          // readonly field
		{topology.KindVariable, "_counter"},      // multi-declarator field
		{topology.KindVariable, "_retries"},      // …second declarator
		{topology.KindVariable, "Number"},        // get/set property
		{topology.KindConstant, "Total"},         // get-only property
		{topology.KindConstant, "Issued"},        // init-only property
		{topology.KindConstant, "Label"},         // expression-bodied property
		{topology.KindVariable, "Paid"},          // event
		{topology.KindConstant, "Draft"},         // enum member
		{topology.KindConstant, "Sent"},          // enum member with value
		{topology.KindTest, "SubmitsCleanly"},    // [Fact]
		{topology.KindTest, "HandlesData"},       // [Theory]
		{topology.KindTest, "Qualified"},         // [Xunit.Fact]
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

// A member of a code type is never KindField — that kind belongs to keys of a
// data-format file (a SQL column, a TOML key). See topology.KindField's doc.
func TestCSharp_NoFieldKind(t *testing.T) {
	nodes, _ := csharpExtract(t, "Invoice.cs", csharpSrc)
	for _, n := range nodes {
		if n.Kind == topology.KindField {
			t.Errorf("node %q emitted as KindField; code-type members are constant/variable", n.Name)
		}
	}
}

func TestCSharp_FileScopedNamespaceScopesSiblings(t *testing.T) {
	src := []byte("namespace Acme.Api;\n\npublic class Handler { public void Run() { } }\n")
	nodes, edges := csharpExtract(t, "Handler.cs", src)
	ns := csharpNode(t, nodes, topology.KindPackage, "Acme.Api")
	if ns.Name != "Acme.Api" {
		t.Fatalf("namespace name %q", ns.Name)
	}
	// The class is a SIBLING of the file-scoped namespace in the tree, so the
	// containment edge only exists if sibling scoping works.
	var nsIdx, clsIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindPackage:
			nsIdx = int64(i)
		case n.Kind == topology.KindClass && n.Name == "Handler":
			clsIdx = int64(i)
		}
	}
	if !hasContains(edges, nsIdx, clsIdx) {
		t.Errorf("no contains edge namespace→Handler; edges=%v", edges)
	}
}

func TestCSharp_ContainmentCertain(t *testing.T) {
	nodes, edges := csharpExtract(t, "Invoice.cs", csharpSrc)
	// Validate is unique to Invoice, so it pins the edge unambiguously.
	var clsIdx, memIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindClass && n.Name == "Invoice":
			clsIdx = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "Validate":
			memIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == clsIdx && e.ToID == memIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge Invoice→Validate")
}

func hasContains(edges []topology.Edge, from, to int64) bool {
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == from && e.ToID == to && e.Confidence == 1.0 {
			return true
		}
	}
	return false
}

func TestCSharp_CallEdgeIntraFile(t *testing.T) {
	nodes, edges := csharpExtract(t, "Invoice.cs", csharpSrc)
	var submitIdx, validateIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "Submit":
			submitIdx = int64(i)
		case "Validate":
			validateIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == submitIdx && e.ToID == validateIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge Submit→Validate; edges=%v", edges)
}

func TestCSharp_LocalFunctionIsFunctionNotMethod(t *testing.T) {
	nodes, _ := csharpExtract(t, "Invoice.cs", csharpSrc)
	if slices.Contains(names(nodes, topology.KindMethod), "Retry") {
		t.Error("local function Retry must not be a method")
	}
	if !slices.Contains(names(nodes, topology.KindFunction), "Retry") {
		t.Error("local function Retry should be a function")
	}
}

// A top-level local function sits under a file-scoped namespace, which is the
// enclosing scope of every top-level statement — it must still be a function.
func TestCSharp_TopLevelStatements(t *testing.T) {
	src := []byte("using System;\nnamespace App;\nConsole.WriteLine(\"hi\");\nint Twice(int n) => n * 2;\nTwice(3);\n")
	nodes, _ := csharpExtract(t, "Program.cs", src)
	if !slices.Contains(names(nodes, topology.KindFunction), "Twice") {
		t.Errorf("top-level local function Twice should be a function; got %v", nodes)
	}
	if slices.Contains(names(nodes, topology.KindMethod), "Twice") {
		t.Error("top-level local function must not be a method")
	}
}

func TestCSharp_LocalsNotExtracted(t *testing.T) {
	src := []byte("class C {\n  void M() {\n    const int limit = 5;\n    var tmp = 1;\n  }\n}\n")
	nodes, _ := csharpExtract(t, "C.cs", src)
	for _, kind := range []topology.NodeKind{topology.KindConstant, topology.KindVariable} {
		for _, n := range []string{"limit", "tmp"} {
			if slices.Contains(names(nodes, kind), n) {
				t.Errorf("local %q inside a method body must not be extracted as %s", n, kind)
			}
		}
	}
}

func TestCSharp_TestDetectionRequiresAttribute(t *testing.T) {
	nodes, _ := csharpExtract(t, "Invoice.cs", csharpSrc)
	tests := names(nodes, topology.KindTest)
	if slices.Contains(tests, "Helper") {
		t.Error("Helper() has no test attribute and must not be a test")
	}
	if !slices.Contains(names(nodes, topology.KindMethod), "Helper") {
		t.Error("Helper() should be a method")
	}
	for _, attr := range []string{"Test", "TestCase", "TestMethod", "Fact", "Theory", "FactAttribute", "PropertyTest"} {
		src := []byte("class T {\n  [" + attr + "]\n  public void M() { }\n}\n")
		nodes, _ := csharpExtract(t, "T.cs", src)
		if !slices.Contains(names(nodes, topology.KindTest), "M") {
			t.Errorf("[%s] should mark M as a test", attr)
		}
	}
}

func TestCSharp_SignatureCarriesGenericsAndConstraints(t *testing.T) {
	nodes, _ := csharpExtract(t, "Invoice.cs", csharpSrc)
	cls := csharpNode(t, nodes, topology.KindClass, "Invoice")
	for _, want := range []string{"class Invoice<T>", ": Document, IPrintable", "where T : class"} {
		if !strings.Contains(cls.Signature, want) {
			t.Errorf("class signature %q missing %q", cls.Signature, want)
		}
	}
	if strings.Contains(cls.Signature, "Serializable") {
		t.Errorf("attributes should be stripped from the signature: %q", cls.Signature)
	}
	if strings.Contains(cls.Signature, "{") {
		t.Errorf("body should be stripped from the signature: %q", cls.Signature)
	}
	m := csharpNode(t, nodes, topology.KindMethod, "Submit")
	if !strings.Contains(m.Signature, "async Task<bool> Submit()") {
		t.Errorf("method signature %q missing async/generic return", m.Signature)
	}
}

func TestCSharp_XmlDocSpan(t *testing.T) {
	nodes, _ := csharpExtract(t, "Invoice.cs", csharpSrc)
	cls := csharpNode(t, nodes, topology.KindClass, "Invoice")
	if !cls.HasDocSpan() {
		t.Fatal("Invoice should carry an xmldoc span")
	}
	doc := string(csharpSrc[cls.DocStartByte:cls.DocEndByte])
	if !strings.Contains(doc, "Issues invoices") {
		t.Errorf("doc span = %q, want the /// summary", doc)
	}
}

func TestCSharp_SpansStamped(t *testing.T) {
	nodes, _ := csharpExtract(t, "Invoice.cs", csharpSrc)
	if len(nodes) == 0 {
		t.Fatal("no nodes")
	}
	for _, n := range nodes {
		if !n.HasBytes {
			t.Errorf("node %q (%s) has no byte span", n.Name, n.Kind)
			continue
		}
		if n.StartByte < 0 || n.EndByte > len(csharpSrc) || n.StartByte >= n.EndByte {
			t.Errorf("node %q span [%d,%d) out of range for %d bytes", n.Name, n.StartByte, n.EndByte, len(csharpSrc))
		}
		if n.EndLine < n.StartLine {
			t.Errorf("node %q EndLine=%d < StartLine=%d", n.Name, n.EndLine, n.StartLine)
		}
	}
}

// A file the grammar cannot parse cleanly still yields the declarations around
// the damage. Two mechanisms do that work and both are asserted here: the walk
// descends into recovery subtrees rather than stopping at them, and declName
// falls back to the child list when the parse error knocks out the `name` field
// of every class in the file (see declName's comment).
//
// The recovery is partial, and the limit is the grammar's, not the walk's: the
// unparseable member takes the rest of ITS enclosing declaration_list with it,
// and — because the enclosing class_declaration's span then runs past its last
// reachable child — everything after the damaged class becomes unreachable
// through the public node API too. `After` is therefore expected to be missing;
// the point of the test is that Good and Broken's own intact members are not.
func TestCSharp_RecoversAroundParseError(t *testing.T) {
	src := []byte("public class Good { public void A() { } }\n" +
		"public class Broken { public void B( { } public void C() { } }\n" +
		"public class After { public void D() { } }\n")
	nodes, _ := csharpExtract(t, "Broken.cs", src)
	for _, want := range []string{"Good", "Broken"} {
		if !slices.Contains(names(nodes, topology.KindClass), want) {
			t.Errorf("class %q lost to error recovery; got %v", want, names(nodes, topology.KindClass))
		}
	}
	for _, want := range []string{"A", "C"} {
		if !slices.Contains(names(nodes, topology.KindMethod), want) {
			t.Errorf("method %q lost to error recovery; got %v", want, names(nodes, topology.KindMethod))
		}
	}
	// C is inside Broken and only reachable by descending past the damage.
	if !slices.Contains(names(nodes, topology.KindMethod), "C") {
		t.Error("descent past a recovery node is what recovers C")
	}
	for _, n := range nodes {
		if n.StartByte < 0 || n.EndByte > len(src) || n.StartByte >= n.EndByte {
			t.Errorf("recovered node %q has an invalid span [%d,%d)", n.Name, n.StartByte, n.EndByte)
		}
	}
}

func TestCSharp_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n/// <summary>x</summary>\n")} {
		nodes, edges := csharpExtract(t, "e.cs", src)
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestCSharp_LanguageAndPath(t *testing.T) {
	nodes, _ := csharpExtract(t, "src/Invoice.cs", csharpSrc)
	for _, n := range nodes {
		if n.Language != "csharp" {
			t.Errorf("node %q language=%q, want csharp", n.Name, n.Language)
		}
		if n.Path != "src/Invoice.cs" {
			t.Errorf("node %q path=%q", n.Name, n.Path)
		}
	}
}

func TestCSharp_Metadata(t *testing.T) {
	e := NewCSharp()
	if e.Language() != "csharp" {
		t.Errorf("Language()=%q", e.Language())
	}
	if got := e.Extensions(); len(got) != 1 || got[0] != ".cs" {
		t.Errorf("Extensions()=%v", got)
	}
}

func TestCSharp_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := NewCSharp().Extract(ctx, "x.cs", csharpSrc); err == nil {
		t.Error("a cancelled context should not start a parse")
	}
}
