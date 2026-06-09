package wasmts

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

func names(nodes []topology.Node, kind topology.NodeKind) []string {
	var out []string
	for _, n := range nodes {
		if n.Kind == kind {
			out = append(out, n.Name)
		}
	}
	return out
}

func has(nodes []topology.Node, kind topology.NodeKind, name string) bool {
	for _, n := range names(nodes, kind) {
		if n == name {
			return true
		}
	}
	return false
}

// TestTSX_TypedArrowNoCascade is the motivating regression: typed arrow params
// in TSX cascade ERROR nodes under gotreesitter, dropping trailing symbols. The
// canonical grammar must extract every symbol, including the trailing interface.
func TestTSX_TypedArrowNoCascade(t *testing.T) {
	src := []byte(`import React from 'react';
export const make = (x: number): number => x;
export const App: React.FC = () => <div className="a">{make(1)}</div>;
export const Row = (p: { id: number }): JSX.Element => <li>{p.id}</li>;
export interface Props { id: number; }
`)
	nodes, _, err := NewTSX().Extract(context.Background(), "App.tsx", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"make", "App", "Row"} {
		if !has(nodes, topology.KindFunction, want) {
			t.Errorf("missing function %q; functions=%v", want, names(nodes, topology.KindFunction))
		}
	}
	if !has(nodes, topology.KindType, "Props") {
		t.Errorf("trailing interface Props dropped (cascade?); types=%v", names(nodes, topology.KindType))
	}
	if !has(nodes, topology.KindImport, "react") {
		t.Errorf("missing import react; imports=%v", names(nodes, topology.KindImport))
	}
}

func TestTSX_Generics(t *testing.T) {
	src := []byte("export function wrap<T>(x: T): T[] { return [x]; }\nexport const id = <T,>(x: T): T => x;\n")
	nodes, _, err := NewTSX().Extract(context.Background(), "g.tsx", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !has(nodes, topology.KindFunction, "wrap") || !has(nodes, topology.KindFunction, "id") {
		t.Errorf("generics dropped; functions=%v", names(nodes, topology.KindFunction))
	}
}

func TestTypeScript_TypedArrowNoCascade(t *testing.T) {
	src := []byte("export const add = (a: number, b: number): number => a + b;\nexport const clamp = (x: number): number => x;\nexport interface P { x: number; }\n")
	nodes, _, err := NewTypeScript().Extract(context.Background(), "u.ts", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"add", "clamp"} {
		if !has(nodes, topology.KindFunction, want) {
			t.Errorf("missing function %q; functions=%v", want, names(nodes, topology.KindFunction))
		}
	}
	if !has(nodes, topology.KindType, "P") {
		t.Errorf("trailing interface P dropped; types=%v", names(nodes, topology.KindType))
	}
}

func TestTSX_KindsAndContainment(t *testing.T) {
	src := []byte(`export class Widget {
  id: number;
  readonly tag: string;
  render(): string { return this.tag; }
}
export interface Shape { area(): number; }
export type ID = string;
export enum Color { Red, Green }
const PI = 3.14;
let mutable = 1;
describe("Widget", () => { it("renders", () => {}); });
`)
	nodes, edges, err := NewTSX().Extract(context.Background(), "w.tsx", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	checks := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindClass, "Widget"},
		{topology.KindMethod, "render"},
		{topology.KindType, "Shape"},
		{topology.KindType, "ID"},
		{topology.KindType, "Color"},
		{topology.KindConstant, "Red"},
		{topology.KindConstant, "PI"},
		{topology.KindVariable, "mutable"},
		{topology.KindConstant, "tag"}, // readonly field
		{topology.KindVariable, "id"},  // mutable field
		{topology.KindTest, "Widget"},
		{topology.KindTest, "renders"},
	}
	for _, c := range checks {
		if !has(nodes, c.kind, c.name) {
			t.Errorf("missing %v %q", c.kind, c.name)
		}
	}
	// Widget → render containment edge present (1.0/extractor).
	var contained bool
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.Confidence == 1.0 {
			contained = true
		}
	}
	if !contained {
		t.Error("expected a containment edge (Widget→render)")
	}
}

func TestExtractor_Extensions(t *testing.T) {
	if got := NewTypeScript().Extensions(); len(got) != 1 || got[0] != ".ts" {
		t.Errorf("TypeScript extensions = %v", got)
	}
	if got := NewTSX().Extensions(); len(got) != 2 || got[0] != ".tsx" || got[1] != ".jsx" {
		t.Errorf("TSX extensions = %v", got)
	}
	// Both report "typescript" so .ts and .tsx symbols search together (tsx alias).
	if NewTypeScript().Language() != "typescript" || NewTSX().Language() != "typescript" {
		t.Error("unexpected Language()")
	}
}

func TestExtractor_EmptyAndGarbage(t *testing.T) {
	for _, src := range [][]byte{nil, []byte(""), []byte("\x00\x01 not really ts <<<"), []byte("// only a comment\n")} {
		if _, _, err := NewTSX().Extract(context.Background(), "x.tsx", src); err != nil {
			t.Errorf("Extract(%q) error: %v", src, err)
		}
	}
}

func TestExtractor_FunctionLocalNotSurfaced(t *testing.T) {
	cases := []struct {
		ex        *Extractor
		file, src string
	}{
		{NewTypeScript(), "f.ts", "function f(): void { const localc = 3; let lv = 4; }\n"},
		{NewTSX(), "f.tsx", "export const C = () => { const localc = 3; return <div/>; };\n"},
	}
	for _, c := range cases {
		nodes, _, err := c.ex.Extract(context.Background(), c.file, []byte(c.src))
		if err != nil {
			t.Fatalf("%s: Extract: %v", c.file, err)
		}
		for _, n := range nodes {
			if n.Name == "localc" || n.Name == "lv" {
				t.Errorf("%s: function-local %q must not be surfaced", c.file, n.Name)
			}
		}
	}
}

// TestTSX_PrivateStaticFields confirms accessibility/static modifiers don't hide
// a class field, and readonly still wins for const classification.
func TestTSX_PrivateStaticFields(t *testing.T) {
	src := []byte("class C {\n  private id: number = 0;\n  static readonly KIND = \"c\";\n  protected name = \"x\";\n}\n")
	nodes, _, err := NewTSX().Extract(context.Background(), "c.tsx", src)
	if err != nil {
		t.Fatal(err)
	}
	if !has(nodes, topology.KindVariable, "id") {
		t.Errorf("private field id should be KindVariable; vars=%v", names(nodes, topology.KindVariable))
	}
	if !has(nodes, topology.KindVariable, "name") {
		t.Errorf("protected field name should be KindVariable; vars=%v", names(nodes, topology.KindVariable))
	}
	if !has(nodes, topology.KindConstant, "KIND") {
		t.Errorf("static readonly KIND should be KindConstant; consts=%v", names(nodes, topology.KindConstant))
	}
}
