package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
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

func TestPython_ByteSpanReconstructsDeclaration(t *testing.T) {
	src := []byte("# a leading comment\ndef café(x):\n    return x\n")
	nodes, _, err := NewPython().Extract(context.Background(), "m.py", src)
	if err != nil {
		t.Fatalf("Extract error: %v", err)
	}
	var fn *topology.Node
	for i := range nodes {
		if nodes[i].Name == "café" {
			fn = &nodes[i]
		}
	}
	if fn == nil {
		t.Fatal("café function not found")
	}
	if !fn.HasBytes {
		t.Fatal("café should carry byte spans")
	}
	// Byte span reconstructs the def exactly (multibyte name proves byte offsets).
	if got := string(src[fn.StartByte:fn.EndByte]); got != "def café(x):\n    return x" {
		t.Errorf("decl span = %q", got)
	}
	// Doc span covers the preceding comment and is a strict prefix of the decl.
	if !fn.HasDocSpan() {
		t.Fatal("café should carry a doc span from the leading comment")
	}
	if fn.DocStartByte >= fn.StartByte {
		t.Errorf("doc span start %d should precede decl start %d", fn.DocStartByte, fn.StartByte)
	}
	if doc := string(src[fn.DocStartByte:fn.DocEndByte]); doc != "# a leading comment" {
		t.Errorf("doc span = %q", doc)
	}
}

// TestPython_ClassBodyDeclarationsCarryDocSpans pins the doc-comment anchor
// across Python's suite boundary. A comment that precedes the FIRST statement of
// a suite is hoisted OUT of the `block` by the grammar and becomes a sibling of
// the block itself, so the declaration — now the block's first child — has no
// previous sibling at all and lost its doc span. The same declaration written
// second in the body kept one, which is why nothing caught it: whether a Python
// method is documented depended on whether something else preceded it in the
// class body.
//
// `top_level` is the control that must not regress, `second` the in-body
// control that always worked, and `bump` the case that was broken. Contrast the
// docstrings: a Python docstring lives INSIDE the declaration's own span, so it
// is not a doc span here — the span must precede the declaration, which is the
// contract every consumer of it relies on (move_symbol's include_doc_comment
// starts its edit there).
func TestPython_ClassBodyDeclarationsCarryDocSpans(t *testing.T) {
	src := []byte(`# Documents top_level.
def top_level(a):
    """Not the doc span."""
    return a


# Documents Widget.
class Widget:
    # Documents bump.
    def bump(self):
        """Not the doc span either."""
        return 1

    # Documents second.
    def second(self):
        return 2

    def undocumented(self):
        return 3
`)
	nodes, _, err := NewPython().Extract(context.Background(), "doc.py", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertDocSpans(t, src, nodes, map[string]string{
		"top_level": "# Documents top_level.",
		"Widget":    "# Documents Widget.",
		"bump":      "# Documents bump.",
		"second":    "# Documents second.",
	})
	assertNoDocSpan(t, src, nodes, "undocumented")
}

// TestPython_DecoratedDeclarationsCarryDocSpans is the other half of the same
// anchor: a decorated declaration is a CHILD of its decorated_definition, so its
// own previous sibling is the `@decorator` and the comment above the decorator
// is never reached — the exact shape the `export` wrapper produces in ES
// modules. It compounds with the suite hoisting for the commonest method form
// there is (`@property`, `@staticmethod`, `@pytest.fixture`), where the doc
// comment sits two anchors up: above a decorated_definition that is itself the
// first child of the class body's block.
func TestPython_DecoratedDeclarationsCarryDocSpans(t *testing.T) {
	src := []byte(`import functools


# Documents cached.
@functools.cache
def cached(a):
    return a


class Widget:
    # Documents area.
    @property
    def area(self):
        return 1
`)
	nodes, _, err := NewPython().Extract(context.Background(), "deco.py", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertDocSpans(t, src, nodes, map[string]string{
		"cached": "# Documents cached.",
		"area":   "# Documents area.",
	})
}

// TestPython_BannerAcrossBlankLineIsNotAClassBodyDocSpan holds the flushness
// half of the anchor at the suite boundary the fix newly reaches. Climbing to
// the block widens what the scan can see, so the blank-line stop has to keep
// holding there too — otherwise a detached note above a class body would become
// its first method's "doc comment", which move_symbol would then move.
func TestPython_BannerAcrossBlankLineIsNotAClassBodyDocSpan(t *testing.T) {
	src := []byte(`class Widget:
    # A detached note.

    def bump(self):
        return 1
`)
	nodes, _, err := NewPython().Extract(context.Background(), "banner.py", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertNoDocSpan(t, src, nodes, "bump")
}
