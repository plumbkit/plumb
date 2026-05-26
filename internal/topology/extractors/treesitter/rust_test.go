package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var rustSrc = []byte(`use std::collections::HashMap;
use crate::foo::Bar;

pub struct Point {
    x: f64,
    y: f64,
}

pub enum Color {
    Red,
    Green,
}

pub trait Shape {
    fn area(&self) -> f64;
}

type Alias = Point;
const MAX: i32 = 10;
static NAME: &str = "plumb";

impl Point {
    pub fn norm(&self) -> f64 {
        self.x
    }
}

pub fn free_fn(a: i32) -> i32 {
    helper(a)
}

fn helper(a: i32) -> i32 {
    a
}

#[test]
fn test_area() {
    assert_eq!(1, 1);
}

#[tokio::test]
async fn test_async() {}
`)

func TestRust_KindsExtracted(t *testing.T) {
	nodes, _, err := NewRust().Extract(context.Background(), "src/geo.rs", rustSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindType, "Point"},
		{topology.KindType, "Color"},
		{topology.KindType, "Shape"},
		{topology.KindType, "Alias"},
		{topology.KindConstant, "MAX"},
		{topology.KindVariable, "NAME"},
		{topology.KindMethod, "norm"},
		{topology.KindFunction, "free_fn"},
		{topology.KindFunction, "helper"},
		{topology.KindMethod, "area"}, // trait signature method
		{topology.KindTest, "test_area"},
		{topology.KindTest, "test_async"},
		{topology.KindImport, "std::collections::HashMap"},
		{topology.KindImport, "crate::foo::Bar"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestRust_TestAttrNotCfgTest(t *testing.T) {
	// #[cfg(test)] must NOT classify the function as a test — only #[test] does.
	src := []byte("#[cfg(test)]\nfn not_a_test() {}\n\n#[test]\nfn real_test() {}\n")
	nodes, _, err := NewRust().Extract(context.Background(), "a.rs", src)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(nodes, topology.KindTest), "not_a_test") {
		t.Error("#[cfg(test)] function was wrongly classified as a test")
	}
	if !slices.Contains(names(nodes, topology.KindTest), "real_test") {
		t.Error("#[test] function was not classified as a test")
	}
}

func TestRust_ImplContainsHeuristic(t *testing.T) {
	nodes, edges, err := NewRust().Extract(context.Background(), "geo.rs", rustSrc)
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
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("impl contains edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no impl contains edge Point→norm; edges=%v", edges)
}

func TestRust_TraitContainsCertain(t *testing.T) {
	src := []byte("pub trait T {\n    fn m(&self) -> i32;\n}\n")
	nodes, edges, err := NewRust().Extract(context.Background(), "t.rs", src)
	if err != nil {
		t.Fatal(err)
	}
	var traitIdx, mIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindType && n.Name == "T":
			traitIdx = int64(i)
		case n.Name == "m":
			mIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == traitIdx && e.ToID == mIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("trait contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no trait contains edge T→m; edges=%v", edges)
}

func TestRust_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewRust().Extract(context.Background(), "geo.rs", rustSrc)
	if err != nil {
		t.Fatal(err)
	}
	var freeIdx, helperIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "free_fn":
			freeIdx = int64(i)
		case "helper":
			helperIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == freeIdx && e.ToID == helperIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge free_fn→helper; edges=%v", edges)
}

func TestRust_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewRust().Extract(context.Background(), "geo.rs", rustSrc)
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

func TestRust_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n// more\n")} {
		nodes, edges, err := NewRust().Extract(context.Background(), "e.rs", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestRust_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewRust().Extract(context.Background(), "src/geo.rs", rustSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "rust" {
			t.Errorf("node %q language=%q, want rust", n.Name, n.Language)
		}
		if n.Path != "src/geo.rs" {
			t.Errorf("node %q path=%q, want src/geo.rs", n.Name, n.Path)
		}
	}
}

func TestRust_Extensions(t *testing.T) {
	if !slices.Contains(NewRust().Extensions(), ".rs") {
		t.Error(".rs missing from Rust Extensions()")
	}
}

func TestRust_StructFieldsAndImplAssoc(t *testing.T) {
	src := []byte(`pub struct Point {
    pub x: i32,
    y: i32,
}

impl Point {
    const ORIGIN: i32 = 0;
    type Unit = i32;
    fn area(&self) -> i32 { 0 }
}
`)
	nodes, edges, err := NewRust().Extract(context.Background(), "p.rs", src)
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
	if !slices.Contains(names(nodes, topology.KindConstant), "ORIGIN") {
		t.Errorf("impl associated const ORIGIN missing; consts=%v", names(nodes, topology.KindConstant))
	}
	if !slices.Contains(names(nodes, topology.KindType), "Unit") {
		t.Errorf("impl associated type Unit missing; types=%v", names(nodes, topology.KindType))
	}
}
