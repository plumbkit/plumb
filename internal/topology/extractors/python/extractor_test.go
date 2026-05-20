package python

import (
	"context"
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
		found := false
		for _, name := range byKind[c.kind] {
			if name == c.name {
				found = true
				break
			}
		}
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
