package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
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
