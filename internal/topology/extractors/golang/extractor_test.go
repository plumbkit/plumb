package golang

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var goSrc = []byte(`package mypackage

import "fmt"

// Greet says hello.
func Greet(name string) string {
	return fmt.Sprintf("hello, %s", name)
}

type Server struct{}

func (s *Server) Start() error { return nil }

func TestGreet(t interface{}) {}

const MaxConns = 100

var defaultTimeout = 30
`)

func TestExtract_FunctionsMethodsTests(t *testing.T) {
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "internal/foo/foo.go", goSrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if len(edges) == 0 {
		t.Error("expected edges, got 0")
	}

	byKind := map[string][]string{}
	for _, n := range nodes {
		byKind[string(n.Kind)] = append(byKind[string(n.Kind)], n.Name)
	}

	cases := []struct{ kind, name string }{
		{"package", "mypackage"},
		{"import", "fmt"},
		{"function", "Greet"},
		{"type", "Server"},
		{"method", "Start"},
		{"test", "TestGreet"},
		{"constant", "MaxConns"},
		{"variable", "defaultTimeout"},
	}
	for _, c := range cases {
		found := slices.Contains(byKind[c.kind], c.name)
		if !found {
			t.Errorf("kind=%q name=%q not found; got %v", c.kind, c.name, byKind[c.kind])
		}
	}
}

func TestExtract_DocComment(t *testing.T) {
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "foo.go", goSrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	for _, n := range nodes {
		if n.Name == "Greet" {
			if n.Docstring == "" {
				t.Error("Greet should have a docstring")
			}
			return
		}
	}
	t.Error("Greet node not found")
}

func TestExtract_EmptyFile(t *testing.T) {
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "empty.go", []byte("package empty"))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	// Only a package node expected
	if len(nodes) != 1 {
		t.Errorf("expected 1 node (package), got %d", len(nodes))
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for empty file, got %d", len(edges))
	}
}

func TestExtract_MalformedFile_NoPanic(t *testing.T) {
	ext := New()
	// Malformed Go that triggers a parse error — should not panic, should return nil
	nodes, edges, err := ext.Extract(context.Background(), "bad.go", []byte("this is not go code @@@"))
	if err != nil {
		t.Fatalf("unexpected error on malformed input: %v", err)
	}
	_ = nodes
	_ = edges
}

func TestExtract_LanguageAndPath(t *testing.T) {
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "pkg/foo.go", goSrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	for _, n := range nodes {
		if n.Language != "go" {
			t.Errorf("node %q has language=%q, want go", n.Name, n.Language)
		}
		if n.Path != "pkg/foo.go" {
			t.Errorf("node %q has path=%q, want pkg/foo.go", n.Name, n.Path)
		}
	}
}

func TestExtract_MethodQualifiedIncludesReceiver(t *testing.T) {
	src := []byte(`package p

type Server struct{}

func (s *Server) Start() error { return nil }

func (v Value) String() string { return "" }
`)
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "p.go", src)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	for _, n := range nodes {
		switch n.Name {
		case "Start":
			if n.Qualified != "(*Server).Start" {
				t.Errorf("Start.Qualified = %q, want (*Server).Start", n.Qualified)
			}
		case "String":
			if n.Qualified != "(Value).String" {
				t.Errorf("String.Qualified = %q, want (Value).String", n.Qualified)
			}
		}
	}
}

func TestExtract_BenchAndExampleAreTest(t *testing.T) {
	src := []byte(`package p

func BenchmarkFoo(b interface{}) {}

func ExampleBar() {}

func TestBaz(t interface{}) {}
`)
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "bench.go", src)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	for _, n := range nodes {
		switch n.Name {
		case "BenchmarkFoo", "ExampleBar", "TestBaz":
			if string(n.Kind) != "test" {
				t.Errorf("%s.Kind = %q, want test", n.Name, n.Kind)
			}
		}
	}
}

func TestExtract_InterfaceTypeDistinctFromStruct(t *testing.T) {
	src := []byte(`package p

type Writer interface{ Write([]byte) (int, error) }

type Buffer struct{ data []byte }
`)
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "iface.go", src)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	byName := map[string]string{}
	for _, n := range nodes {
		if n.Name == "Writer" || n.Name == "Buffer" {
			byName[n.Name] = string(n.Kind)
		}
	}
	if k := byName["Writer"]; k != "type" {
		t.Errorf("Writer.Kind = %q, want type", k)
	}
	if k := byName["Buffer"]; k != "type" {
		t.Errorf("Buffer.Kind = %q, want type", k)
	}
}

func TestExtract_CallEdges_IntraFile(t *testing.T) {
	src := []byte(`package p

func helper() {}

func caller() {
	helper()
}
`)
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var helperIdx, callerIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "helper":
			helperIdx = int64(i)
		case "caller":
			callerIdx = int64(i)
		}
	}
	if helperIdx < 0 || callerIdx < 0 {
		t.Fatalf("helper or caller node not found; nodes=%v", nodes)
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == callerIdx && e.ToID == helperIdx {
			return // found the expected edge
		}
	}
	t.Errorf("no EdgeCalls(caller→helper) found; edges=%v", edges)
}

func TestExtract_CallEdges_NoSelfLoop(t *testing.T) {
	src := []byte(`package p

func recursive() {
	recursive()
}
`)
	ext := New()
	_, edges, err := ext.Extract(context.Background(), "p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == e.ToID {
			t.Error("self-loop EdgeCalls emitted")
		}
	}
}

func TestExtract_CallEdges_CrossFileNotEmitted(t *testing.T) {
	src := []byte(`package p

import "fmt"

func greet() {
	fmt.Println("hello")
}
`)
	ext := New()
	_, edges, err := ext.Extract(context.Background(), "p.go", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls {
			t.Errorf("unexpected EdgeCalls edge (callee not in file): %+v", e)
		}
	}
}

func TestExtract_LineRanges(t *testing.T) {
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "foo.go", goSrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindFunction || n.Kind == topology.KindMethod {
			if n.StartLine <= 0 {
				t.Errorf("node %q has StartLine=%d, want > 0", n.Name, n.StartLine)
			}
			if n.EndLine < n.StartLine {
				t.Errorf("node %q EndLine %d < StartLine %d", n.Name, n.EndLine, n.StartLine)
			}
		}
	}
}

// nodeByName finds the first node with the given name, or fails the test.
func nodeByName(t *testing.T, nodes []topology.Node, name string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %q not found", name)
	return topology.Node{}
}

func TestExtract_ByteSpanReconstructsDeclaration(t *testing.T) {
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "foo.go", goSrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	// Greet's declaration span must exactly reconstruct the func declaration text.
	g := nodeByName(t, nodes, "Greet")
	if !g.HasBytes {
		t.Fatal("Greet should carry byte spans")
	}
	got := string(goSrc[g.StartByte:g.EndByte])
	const want = "func Greet(name string) string {\n\treturn fmt.Sprintf(\"hello, %s\", name)\n}"
	if got != want {
		t.Errorf("Greet decl span = %q, want %q", got, want)
	}
	// The doc-comment span must cover the // Greet line and be a strict prefix
	// (lower byte offset) of the declaration.
	if !g.HasDocSpan() {
		t.Fatal("Greet should carry a doc span")
	}
	if g.DocStartByte >= g.StartByte {
		t.Errorf("doc span start %d should precede decl start %d", g.DocStartByte, g.StartByte)
	}
	if doc := string(goSrc[g.DocStartByte:g.DocEndByte]); doc != "// Greet says hello." {
		t.Errorf("doc span = %q, want %q", doc, "// Greet says hello.")
	}
}

func TestExtract_ByteSpanMultibyte(t *testing.T) {
	// A multibyte identifier before the target proves byte offsets (not rune
	// indices) and that columns are byte columns.
	src := []byte("package p\n\n// café returns a drink.\nfunc café() string { return \"☕\" }\n")
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "m.go", src)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	n := nodeByName(t, nodes, "café")
	if !n.HasBytes {
		t.Fatal("café should carry byte spans")
	}
	if got := string(src[n.StartByte:n.EndByte]); got != "func café() string { return \"☕\" }" {
		t.Errorf("decl span = %q", got)
	}
	// The declaration starts at column 0 of its line.
	if n.StartCol != 0 {
		t.Errorf("StartCol = %d, want 0", n.StartCol)
	}
	if doc := string(src[n.DocStartByte:n.DocEndByte]); doc != "// café returns a drink." {
		t.Errorf("doc span = %q", doc)
	}
}
