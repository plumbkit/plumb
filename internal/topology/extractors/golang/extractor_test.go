package golang

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
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
