package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var jsSrc = []byte(`import { readFile } from 'fs/promises';
import defaultExport, { named as alias } from './util.js';
const path = require('path');

const MAX_RETRIES = 3;
let counter = 0;
var legacy = true;

function greet(name) {
  return ` + "`hello ${name}`" + `;
}

async function fetchData(url) {
  const res = await fetch(url);
  return greet(res);
}

const add = (a, b) => a + b;
const square = function (x) { return x * x; };

export function exported() {
  return add(1, 2);
}

export const helper = () => greet('x');

class Animal {
  constructor(name) {
    this.name = name;
  }
  speak() {
    return greet(this.name);
  }
  static create(name) {
    return new Animal(name);
  }
}

describe('greet', () => {
  it('greets by name', () => {
    expect(greet('world')).toBe('hello world');
  });
  test('adds numbers', () => {
    expect(add(1, 2)).toBe(3);
  });
});
`)

func TestJavaScript_KindsExtracted(t *testing.T) {
	nodes, _, err := NewJavaScript().Extract(context.Background(), "src/app.js", jsSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "fs/promises"},
		{topology.KindImport, "./util.js"},
		{topology.KindImport, "path"}, // require(...)
		{topology.KindConstant, "MAX_RETRIES"},
		{topology.KindVariable, "counter"},
		{topology.KindVariable, "legacy"},
		{topology.KindFunction, "greet"},
		{topology.KindFunction, "fetchData"},
		{topology.KindFunction, "add"},    // arrow binding
		{topology.KindFunction, "square"}, // function-expression binding
		{topology.KindFunction, "exported"},
		{topology.KindFunction, "helper"},
		{topology.KindClass, "Animal"},
		{topology.KindMethod, "speak"},
		{topology.KindMethod, "create"},
		{topology.KindTest, "greet"},
		{topology.KindTest, "greets by name"},
		{topology.KindTest, "adds numbers"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestJavaScript_ConstVsVar(t *testing.T) {
	nodes, _, err := NewJavaScript().Extract(context.Background(), "app.js", jsSrc)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(nodes, topology.KindVariable), "MAX_RETRIES") {
		t.Error("MAX_RETRIES is const → should be a constant, not a variable")
	}
	if slices.Contains(names(nodes, topology.KindConstant), "counter") {
		t.Error("counter is let → should be a variable, not a constant")
	}
	// An arrow/function binding must not surface as a constant or variable.
	if slices.Contains(names(nodes, topology.KindConstant), "add") {
		t.Error("add is an arrow function → should be a function, not a constant")
	}
}

func TestJavaScript_MethodContainment(t *testing.T) {
	nodes, edges, err := NewJavaScript().Extract(context.Background(), "app.js", jsSrc)
	if err != nil {
		t.Fatal(err)
	}
	var animalIdx, speakIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindClass && n.Name == "Animal":
			animalIdx = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "speak":
			speakIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == animalIdx && e.ToID == speakIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("containment edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no containment edge Animal→speak; edges=%v", edges)
}

func TestJavaScript_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewJavaScript().Extract(context.Background(), "app.js", jsSrc)
	if err != nil {
		t.Fatal(err)
	}
	var fetchIdx, greetIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "fetchData":
			fetchIdx = int64(i)
		case "greet":
			if n.Kind == topology.KindFunction {
				greetIdx = int64(i)
			}
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == fetchIdx && e.ToID == greetIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge fetchData→greet; edges=%v", edges)
}

func TestJavaScript_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewJavaScript().Extract(context.Background(), "app.js", jsSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindFunction && n.Name == "greet" {
			if n.EndLine <= n.StartLine {
				t.Errorf("greet EndLine=%d should exceed StartLine=%d", n.EndLine, n.StartLine)
			}
			return
		}
	}
	t.Fatal("greet function node not found")
}

func TestJavaScript_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n// more\n")} {
		nodes, edges, err := NewJavaScript().Extract(context.Background(), "e.js", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestJavaScript_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewJavaScript().Extract(context.Background(), "src/app.js", jsSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "javascript" {
			t.Errorf("node %q language=%q, want javascript", n.Name, n.Language)
		}
		if n.Path != "src/app.js" {
			t.Errorf("node %q path=%q, want src/app.js", n.Name, n.Path)
		}
	}
}

func TestJavaScript_Extensions(t *testing.T) {
	exts := NewJavaScript().Extensions()
	for _, want := range []string{".js", ".mjs", ".cjs"} {
		if !slices.Contains(exts, want) {
			t.Errorf("%s missing from JavaScript Extensions()", want)
		}
	}
}

func TestJavaScript_ClassFields(t *testing.T) {
	src := []byte(`class Service {
  count = 0;
  #secret = 1;
  static MAX = 9;
  greet() { return this.count; }
}
`)
	nodes, edges, err := NewJavaScript().Extract(context.Background(), "s.js", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"count", "#secret", "MAX"} {
		if !slices.Contains(names(nodes, topology.KindVariable), f) {
			t.Errorf("class field %q not a variable; vars=%v", f, names(nodes, topology.KindVariable))
		}
	}
	if conf, ok := containedAt(nodes, edges, "count"); !ok || conf != 1.0 {
		t.Errorf("field count should be contained at 1.0; got conf=%v ok=%v", conf, ok)
	}
}

// TestJavaScript_ExportedDeclarationsCarryDocSpans pins the doc-comment anchor
// across the `export` wrapper. An exported declaration is a CHILD of its
// export_statement, so scanning the declaration's own previous siblings only
// ever reaches the `export`/`default` keywords — every exported symbol lost its
// doc span while the unexported control kept one, which is the asymmetry this
// table proves is gone.
func TestJavaScript_ExportedDeclarationsCarryDocSpans(t *testing.T) {
	src := []byte(`/** Adds two numbers. */
export function add(a, b) {
  return a + b;
}

/** A widget. */
export class Widget {}

/** The default one. */
export default function main() {}

/** Not exported. */
function helper() {}
`)
	nodes, _, err := NewJavaScript().Extract(context.Background(), "d.js", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertDocSpans(t, src, nodes, map[string]string{
		"add":    "/** Adds two numbers. */",
		"Widget": "/** A widget. */",
		"main":   "/** The default one. */",
		"helper": "/** Not exported. */",
	})
}

// TestJavaScript_BannerAcrossBlankLineIsNotADocSpan pins the adjacency half of
// docSpanBefore, on the export path that made it reachable. A grammar emits no
// node for a blank line, so a bare previous-sibling scan walks straight past
// one and a file-leading licence banner becomes the first export's "doc
// comment" — which every consumer then treats as part of the symbol
// (replace_symbol_body / move_symbol with include_doc_comment both prefer the
// topology span), i.e. a silently deleted licence header. `g` is the mixed case
// the walk has to split correctly: banner, blank line, real doc block.
func TestJavaScript_BannerAcrossBlankLineIsNotADocSpan(t *testing.T) {
	src := []byte(`// Copyright 2026 The Plumb Authors.
// SPDX-License-Identifier: Apache-2.0

export function f(a) {
  return a;
}

// A banner-ish note, detached.

/** Documents g. */
export function g(b) {
  return b;
}
`)
	nodes, _, err := NewJavaScript().Extract(context.Background(), "banner.js", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertNoDocSpan(t, src, nodes, "f")
	assertDocSpans(t, src, nodes, map[string]string{"g": "/** Documents g. */"})
}

// TestJavaScript_CommentInsideExportWrapperKeepsItsSpan is the other side of
// the export anchor: a comment written between `export` and the declaration is
// a sibling of the DECLARATION, never of the export_statement, so climbing to
// the wrapper unconditionally would lose it. jsDocSpan scans the declaration
// first and only climbs on the empty sentinel.
func TestJavaScript_CommentInsideExportWrapperKeepsItsSpan(t *testing.T) {
	src := []byte(`export /** Documents Inner. */ class Inner {}
`)
	nodes, _, err := NewJavaScript().Extract(context.Background(), "inner.js", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertDocSpans(t, src, nodes, map[string]string{"Inner": "/** Documents Inner. */"})
}

// TestJavaScript_MultiLineCommentRunIsCollectedWhole pins the backward half of
// the run-walk for JavaScript, where a previous-sibling chain cannot find it.
// In this grammar every comment ahead of the first non-comment top-level node
// reports a nil Parent — the whole leading run, not just its first line — so
// PrevSibling returns nil from the first hop on any of them and a `//` doc
// block collapsed to its LAST line. A run further down the file chains fine,
// which is why `third` was never affected. Nothing caught it because no test
// used a multi-comment run in JavaScript, and every other grammar chains fine.
//
// A collapsed span is not merely short: include_doc_comment (default TRUE on
// move_symbol) starts the edit at the doc span, so replacing `add` would have
// left `// Adds two numbers.` orphaned above the new declaration. `third` is
// the case the index walk must still get right — with the whole child list in
// hand it is just as easy to walk back past the blank line as to stop at it, so
// the flushness check is what has to hold, not the traversal.
func TestJavaScript_MultiLineCommentRunIsCollectedWhole(t *testing.T) {
	src := []byte(`// Adds two numbers.
// Both arguments are coerced to Number.
export function add(a, b) {
  return a + b;
}

// Not exported.
// Two lines of it.
function helper() {}

// Detached banner note.
// Second banner line.

// Documents third.
// Second doc line.
export function third(c) {
  return c;
}
`)
	nodes, _, err := NewJavaScript().Extract(context.Background(), "run.js", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	assertDocSpans(t, src, nodes, map[string]string{
		"add":    "// Adds two numbers.\n// Both arguments are coerced to Number.",
		"helper": "// Not exported.\n// Two lines of it.",
		"third":  "// Documents third.\n// Second doc line.",
	})
}

// docNodeNamed returns the first extracted node called name, or nil.
func docNodeNamed(nodes []topology.Node, name string) *topology.Node {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

// assertDocSpans checks that each named node carries exactly the given doc
// comment, and that the span precedes the declaration it documents.
func assertDocSpans(t *testing.T, src []byte, nodes []topology.Node, want map[string]string) {
	t.Helper()
	for name, doc := range want {
		n := docNodeNamed(nodes, name)
		if n == nil {
			t.Errorf("%q not extracted", name)
			continue
		}
		if !n.HasDocSpan() {
			t.Errorf("%q carries no doc span", name)
			continue
		}
		if got := string(src[n.DocStartByte:n.DocEndByte]); got != doc {
			t.Errorf("%q doc span = %q, want %q", name, got, doc)
		}
		if n.DocStartByte >= n.StartByte {
			t.Errorf("%q doc span start %d should precede decl start %d", name, n.DocStartByte, n.StartByte)
		}
	}
}

// assertNoDocSpan checks that the named node was extracted and claims no doc
// comment at all — the sentinel a detached comment block must produce.
func assertNoDocSpan(t *testing.T, src []byte, nodes []topology.Node, name string) {
	t.Helper()
	n := docNodeNamed(nodes, name)
	if n == nil {
		t.Errorf("%q not extracted", name)
		return
	}
	if n.HasDocSpan() {
		t.Errorf("%q claims the detached comment %q as its doc span; a blank line separates them",
			name, string(src[n.DocStartByte:n.DocEndByte]))
	}
}
