package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var pySrc = []byte(`import os
from pathlib import Path

class MyService:
    """A service."""

    def __init__(self):
        pass

    @property
    def label(self):
        return "x"

    async def run(self):
        pass

async def background_task():
    pass

def test_my_service():
    pass

def helper_func():
    pass
`)

func names(nodes []topology.Node, kind topology.NodeKind) []string {
	var out []string
	for _, n := range nodes {
		if n.Kind == kind {
			out = append(out, n.Name)
		}
	}
	return out
}

func TestPython_KindsExtracted(t *testing.T) {
	nodes, _, err := NewPython().Extract(context.Background(), "pkg/service.py", pySrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindClass, "MyService"},
		{topology.KindMethod, "__init__"},
		{topology.KindMethod, "label"}, // decorated method
		{topology.KindMethod, "run"},   // async method
		{topology.KindFunction, "background_task"},
		{topology.KindTest, "test_my_service"},
		{topology.KindFunction, "helper_func"},
		{topology.KindImport, "os"},
		{topology.KindImport, "pathlib"}, // from-import records the module
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestPython_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewPython().Extract(context.Background(), "svc.py", pySrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindClass && n.Name == "MyService" {
			if n.EndLine <= n.StartLine {
				t.Errorf("MyService EndLine=%d should exceed StartLine=%d", n.EndLine, n.StartLine)
			}
			return
		}
	}
	t.Fatal("MyService class node not found")
}

func TestPython_ContainmentEdgeCertain(t *testing.T) {
	src := []byte("class S:\n    def run(self):\n        pass\n")
	nodes, edges, err := NewPython().Extract(context.Background(), "s.py", src)
	if err != nil {
		t.Fatal(err)
	}
	var classIdx, runIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindClass && n.Name == "S":
			classIdx = int64(i)
		case n.Name == "run":
			runIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == classIdx && e.ToID == runIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge S→run; edges=%v", edges)
}

func TestPython_NestedFuncNotMethod(t *testing.T) {
	src := []byte("def make():\n    def inner():\n        pass\n    return inner\n")
	nodes, _, err := NewPython().Extract(context.Background(), "n.py", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Name == "inner" && n.Kind != topology.KindFunction {
			t.Errorf("inner kind=%s, want function (nested funcs are not methods)", n.Kind)
		}
	}
}

func TestPython_CallEdgeIntraFile(t *testing.T) {
	src := []byte("def helper():\n    pass\n\ndef caller():\n    helper()\n")
	nodes, edges, err := NewPython().Extract(context.Background(), "c.py", src)
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
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == callerIdx && e.ToID == helperIdx {
			return
		}
	}
	t.Errorf("no EdgeCalls caller→helper; edges=%v", edges)
}

func TestPython_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("# just a comment\n# more\n")} {
		nodes, edges, err := NewPython().Extract(context.Background(), "e.py", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestPython_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewPython().Extract(context.Background(), "pkg/service.py", pySrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "python" {
			t.Errorf("node %q language=%q, want python", n.Name, n.Language)
		}
		if n.Path != "pkg/service.py" {
			t.Errorf("node %q path=%q, want pkg/service.py", n.Name, n.Path)
		}
	}
}

func TestPython_ModuleAndClassBindings(t *testing.T) {
	src := []byte(`MAX_SIZE = 100
threshold = 0.5

class Service:
    LIMIT = 10
    name = "svc"

    def run(self):
        local = 1
        return local
`)
	nodes, edges, err := NewPython().Extract(context.Background(), "svc.py", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"MAX_SIZE", "LIMIT"} {
		if !slices.Contains(names(nodes, topology.KindConstant), c) {
			t.Errorf("ALL_CAPS %q should be a constant; consts=%v", c, names(nodes, topology.KindConstant))
		}
	}
	for _, v := range []string{"threshold", "name"} {
		if !slices.Contains(names(nodes, topology.KindVariable), v) {
			t.Errorf("%q should be a variable; vars=%v", v, names(nodes, topology.KindVariable))
		}
	}
	if slices.Contains(names(nodes, topology.KindVariable), "local") ||
		slices.Contains(names(nodes, topology.KindConstant), "local") {
		t.Error("function-local binding 'local' must not be extracted")
	}
	if conf, ok := containedAt(nodes, edges, "LIMIT"); !ok || conf != 1.0 {
		t.Errorf("class attr LIMIT should be contained at 1.0; got conf=%v ok=%v", conf, ok)
	}
}
