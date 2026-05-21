package python

import (
	"context"
	"slices"
	"testing"
)

var pySrc = []byte(`import os
from pathlib import Path

class MyService:
    """A service."""

    def __init__(self):
        pass

    def run(self):
        pass

async def background_task():
    pass

def test_my_service():
    pass

def helper_func():
    pass
`)

func TestExtract_ClassMethodsTests(t *testing.T) {
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "pkg/service.py", pySrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}

	byKind := map[string][]string{}
	for _, n := range nodes {
		byKind[string(n.Kind)] = append(byKind[string(n.Kind)], n.Name)
	}

	cases := []struct{ kind, name string }{
		{"class", "MyService"},
		{"method", "run"},
		{"function", "background_task"},
		{"test", "test_my_service"},
		{"import", "os"},
	}
	for _, c := range cases {
		found := slices.Contains(byKind[c.kind], c.name)
		if !found {
			t.Errorf("kind=%q name=%q not found; got %v", c.kind, c.name, byKind[c.kind])
		}
	}

	// Methods inside a class should have containment edges
	if len(edges) == 0 {
		t.Error("expected containment edges for methods inside class, got 0")
	}
}

func TestExtract_EmptyFile(t *testing.T) {
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "empty.py", []byte(""))
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes for empty file, got %d", len(nodes))
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for empty file, got %d", len(edges))
	}
}

func TestExtract_LanguageAndPath(t *testing.T) {
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "pkg/service.py", pySrc)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("expected nodes, got 0")
	}
	for _, n := range nodes {
		if n.Language != "python" {
			t.Errorf("node %q has language=%q, want python", n.Name, n.Language)
		}
		if n.Path != "pkg/service.py" {
			t.Errorf("node %q has path=%q, want pkg/service.py", n.Name, n.Path)
		}
	}
}

func TestExtract_AsyncDefIsFunction(t *testing.T) {
	ext := New()
	nodes, _, err := ext.Extract(context.Background(), "a.py", []byte("async def background():\n    pass\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Name == "background" {
			if string(n.Kind) != "function" {
				t.Errorf("async def background: kind=%q, want function", n.Kind)
			}
			return
		}
	}
	t.Error("async def background node not found")
}

func TestExtract_ContainmentEdgeConfidence(t *testing.T) {
	src := []byte(`class MyService:
    def run(self):
        pass
`)
	ext := New()
	_, edges, err := ext.Extract(context.Background(), "svc.py", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if string(e.Kind) == "contains" {
			if e.Confidence != 0.8 {
				t.Errorf("containment edge confidence=%v, want 0.8", e.Confidence)
			}
			return
		}
	}
	t.Error("no containment edge found")
}

func TestExtract_CommentOnlyFile(t *testing.T) {
	src := []byte("# just a comment\n# another comment\n")
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "comments.py", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes for comment-only file, got %d", len(nodes))
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for comment-only file, got %d", len(edges))
	}
}

func TestExtract_CallEdges_IntraFile(t *testing.T) {
	src := []byte(`def helper():
    pass

def caller():
    helper()
`)
	ext := New()
	nodes, edges, err := ext.Extract(context.Background(), "c.py", src)
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
		t.Fatalf("helper or caller not found; nodes=%v", nodes)
	}
	for _, e := range edges {
		if string(e.Kind) == "calls" && e.FromID == callerIdx && e.ToID == helperIdx {
			if e.Confidence != 0.6 {
				t.Errorf("call edge confidence=%v, want 0.6", e.Confidence)
			}
			if e.Source != "heuristic" {
				t.Errorf("call edge source=%q, want heuristic", e.Source)
			}
			return
		}
	}
	t.Errorf("no EdgeCalls(caller→helper) found; edges=%v", edges)
}

func TestExtract_FromImport(t *testing.T) {
	ext := New()
	src := []byte("from pathlib import Path\n")
	nodes, _, err := ext.Extract(context.Background(), "foo.py", src)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	// The extractor records the module name ("pathlib") for from-imports,
	// not the imported symbol — this is intentional (records the dependency).
	for _, n := range nodes {
		if n.Kind == "import" && n.Name == "pathlib" {
			return
		}
	}
	t.Error("expected import node 'pathlib' from 'from pathlib import Path'")
}
