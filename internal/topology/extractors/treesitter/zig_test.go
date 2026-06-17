package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var zigSrc = []byte(`const std = @import("std");
const builtin = @import("builtin");

pub const Point = struct {
    x: f64,
    y: f64,

    pub fn norm(self: Point) f64 {
        return self.x;
    }
};

const Color = enum { red, green };
const Tagged = union(enum) { a: i32, b: f64 };

pub fn add(a: i32, b: i32) i32 {
    return helper(a) + b;
}

fn helper(a: i32) i32 {
    return a;
}

var counter: u32 = 0;
const LIMIT: u32 = 100;

test "addition works" {
    try std.testing.expect(add(1, 2) == 3);
}
`)

func TestZig_KindsExtracted(t *testing.T) {
	nodes, _, err := NewZig().Extract(context.Background(), "src/geo.zig", zigSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "std"},
		{topology.KindImport, "builtin"},
		{topology.KindType, "Point"},
		{topology.KindType, "Color"},
		{topology.KindType, "Tagged"},
		{topology.KindMethod, "norm"},
		{topology.KindFunction, "add"},
		{topology.KindFunction, "helper"},
		{topology.KindVariable, "counter"},
		{topology.KindConstant, "LIMIT"},
		{topology.KindTest, "addition works"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestZig_MethodContainmentCertain(t *testing.T) {
	nodes, edges, err := NewZig().Extract(context.Background(), "geo.zig", zigSrc)
	if err != nil {
		t.Fatal(err)
	}
	var pointIdx, normIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindType && n.Name == "Point":
			pointIdx = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "norm":
			normIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == pointIdx && e.ToID == normIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge Point→norm; edges=%v", edges)
}

func TestZig_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewZig().Extract(context.Background(), "geo.zig", zigSrc)
	if err != nil {
		t.Fatal(err)
	}
	var addIdx, helperIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "add":
			addIdx = int64(i)
		case "helper":
			helperIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == addIdx && e.ToID == helperIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge add→helper; edges=%v", edges)
}

// TestZig_AmbiguousCallEdgeDownWeighted is the regression for #30: when more
// than one callable in the file shares the callee's name,
// a name-only (receiver-blind) call resolution is ambiguous, so the edge is
// down-weighted to 0.5/heuristic-ambiguous rather than asserting a confident
// 0.8 edge to an arbitrary same-named target. A uniquely-named call in the same
// file is unaffected.
func TestZig_AmbiguousCallEdgeDownWeighted(t *testing.T) {
	src := []byte(`const A = struct {
    fn deinit(self: *A) void {}
};
const B = struct {
    fn deinit(self: *B) void {}
    fn release(self: *B) void {
        self.deinit();
        cleanup();
    }
};
fn cleanup() void {}
`)
	nodes, edges, err := NewZig().Extract(context.Background(), "res.zig", src)
	if err != nil {
		t.Fatal(err)
	}
	var gotAmbiguous, gotUnique bool
	for _, e := range edges {
		if e.Kind != topology.EdgeCalls {
			continue
		}
		switch nodes[e.ToID].Name {
		case "deinit": // two deinit methods in the file → ambiguous
			gotAmbiguous = true
			if e.Confidence != 0.5 || e.Source != "heuristic-ambiguous" {
				t.Errorf("ambiguous call edge →deinit conf=%v src=%q, want 0.5/heuristic-ambiguous", e.Confidence, e.Source)
			}
		case "cleanup": // unique name → confident
			gotUnique = true
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("unique call edge →cleanup conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
		}
	}
	if !gotAmbiguous {
		t.Errorf("no call edge to an ambiguous deinit; edges=%v", edges)
	}
	if !gotUnique {
		t.Errorf("no call edge to the unique cleanup; edges=%v", edges)
	}
}

func TestZig_ConstVsVar(t *testing.T) {
	nodes, _, err := NewZig().Extract(context.Background(), "geo.zig", zigSrc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "LIMIT") {
		t.Error("LIMIT should be a constant")
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "counter") {
		t.Error("counter should be a variable")
	}
	// A type-bound const must NOT also appear as a plain constant binding.
	if slices.Contains(names(nodes, topology.KindConstant), "Point") {
		t.Error("Point is a type, not a constant binding")
	}
}

func TestZig_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewZig().Extract(context.Background(), "geo.zig", zigSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindType && n.Name == "Point" {
			if n.EndLine <= n.StartLine {
				t.Errorf("Point EndLine=%d should exceed StartLine=%d", n.EndLine, n.StartLine)
			}
			return
		}
	}
	t.Fatal("Point type node not found")
}

func TestZig_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n// more\n")} {
		nodes, edges, err := NewZig().Extract(context.Background(), "e.zig", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestZig_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewZig().Extract(context.Background(), "src/geo.zig", zigSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "zig" {
			t.Errorf("node %q language=%q, want zig", n.Name, n.Language)
		}
		if n.Path != "src/geo.zig" {
			t.Errorf("node %q path=%q, want src/geo.zig", n.Name, n.Path)
		}
	}
}

func TestZig_Extensions(t *testing.T) {
	if !slices.Contains(NewZig().Extensions(), ".zig") {
		t.Error(".zig missing from Zig Extensions()")
	}
}

func TestZig_ContainerFieldsAndEnumMembers(t *testing.T) {
	src := []byte(`const Point = struct {
    x: i32,
    y: i32,
    pub fn init() Point { return Point{}; }
};

const Color = enum { red, green };
`)
	nodes, edges, err := NewZig().Extract(context.Background(), "p.zig", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"x", "y"} {
		if !slices.Contains(names(nodes, topology.KindVariable), f) {
			t.Errorf("struct field %q not a variable; vars=%v", f, names(nodes, topology.KindVariable))
		}
		if conf, ok := containedAt(nodes, edges, f); !ok || conf != 1.0 {
			t.Errorf("field %q should be contained at 1.0; got conf=%v ok=%v", f, conf, ok)
		}
	}
	for _, c := range []string{"red", "green"} {
		if !slices.Contains(names(nodes, topology.KindConstant), c) {
			t.Errorf("enum member %q not a constant; consts=%v", c, names(nodes, topology.KindConstant))
		}
	}
}
