package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var tsSrc = []byte(`import { readFile } from 'fs/promises';
import type { Config } from './config';

export const MAX = 3;
let counter = 0;

export type ID = string | number;

export interface Repository<T> {
  find(id: ID): Promise<T | null>;
  save(entity: T): Promise<void>;
}

export enum Color {
  Red,
  Green,
  Blue,
}

export const add = (a: number, b: number): number => a + b;

export async function load(url: string): Promise<string> {
  const res = await fetch(url);
  return add(res.length, 1).toString();
}

@Injectable()
export class UserService implements Repository<User> {
  constructor(private readonly db: Db) {}

  async find(id: ID): Promise<User | null> {
    return this.db.get(id);
  }

  static create(): UserService {
    return new UserService(new Db());
  }
}

namespace Geometry {
  export function area(r: number): number {
    return Math.PI * r * r;
  }
}

describe('UserService', () => {
  it('finds a user', () => {
    expect(true).toBe(true);
  });
});
`)

func TestTypeScript_KindsExtracted(t *testing.T) {
	nodes, _, err := NewTypeScript().Extract(context.Background(), "src/service.ts", tsSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "fs/promises"},
		{topology.KindImport, "./config"},
		{topology.KindConstant, "MAX"},
		{topology.KindVariable, "counter"},
		{topology.KindType, "ID"},         // type alias
		{topology.KindType, "Repository"}, // interface
		{topology.KindType, "Color"},      // enum
		{topology.KindConstant, "Red"},    // enum member
		{topology.KindFunction, "add"},    // arrow binding
		{topology.KindFunction, "load"},
		{topology.KindFunction, "area"}, // inside namespace
		{topology.KindClass, "UserService"},
		{topology.KindMethod, "find"},
		{topology.KindMethod, "create"},
		{topology.KindTest, "UserService"},
		{topology.KindTest, "finds a user"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

// TestTypeScript_TypedArrowNoCascade is the GLR-regression guard. A utility
// module whose typed arrow params cascaded ERROR nodes on gotreesitter v0.20.x
// (dropping every symbol after the break) must yield ALL its symbols. If a
// future gotreesitter bump reintroduces the cascade, this fails loudly.
func TestTypeScript_TypedArrowNoCascade(t *testing.T) {
	utils := []byte(`export const add = (a: number, b: number): number => a + b;

export const greet = (name: string): string => ` + "`hello ${name}`" + `;

export const clamp = (x: number, lo: number, hi: number): number => {
  return Math.min(Math.max(x, lo), hi);
};

export function identity<T>(v: T): T {
  return v;
}

export interface Point {
  x: number;
  y: number;
}
`)
	nodes, _, err := NewTypeScript().Extract(context.Background(), "src/utils.ts", utils)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"add", "greet", "clamp", "identity"} {
		if !slices.Contains(names(nodes, topology.KindFunction), want) {
			t.Errorf("function %q missing — typed-arrow cascade likely returned; funcs=%v",
				want, names(nodes, topology.KindFunction))
		}
	}
	if !slices.Contains(names(nodes, topology.KindType), "Point") {
		t.Errorf("interface Point missing — a cascade would swallow trailing symbols")
	}
}

// TestTypeScript_DefaultArrowNoCascade guards the default-valued arrow-param
// variant (`(a = 5) => a`), a distinct v0.20.x GLR failure that needed no type
// annotation at all.
func TestTypeScript_DefaultArrowNoCascade(t *testing.T) {
	src := []byte("export const f = (a = 5) => a;\n\nexport interface After { x: number }\n")
	nodes, _, err := NewTypeScript().Extract(context.Background(), "src/f.ts", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !slices.Contains(names(nodes, topology.KindFunction), "f") {
		t.Errorf("arrow f missing; funcs=%v", names(nodes, topology.KindFunction))
	}
	if !slices.Contains(names(nodes, topology.KindType), "After") {
		t.Errorf("trailing interface After missing — cascade swallowed it")
	}
}

func TestTypeScript_MethodContainment(t *testing.T) {
	nodes, edges, err := NewTypeScript().Extract(context.Background(), "s.ts", tsSrc)
	if err != nil {
		t.Fatal(err)
	}
	var classIdx, findIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindClass && n.Name == "UserService":
			classIdx = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "find":
			findIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == classIdx && e.ToID == findIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("containment edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no containment edge UserService→find; edges=%v", edges)
}

func TestTypeScript_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewTypeScript().Extract(context.Background(), "s.ts", tsSrc)
	if err != nil {
		t.Fatal(err)
	}
	var loadIdx, addIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "load":
			loadIdx = int64(i)
		case "add":
			if n.Kind == topology.KindFunction {
				addIdx = int64(i)
			}
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == loadIdx && e.ToID == addIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge load→add; edges=%v", edges)
}

func TestTypeScript_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n// more\n")} {
		nodes, edges, err := NewTypeScript().Extract(context.Background(), "e.ts", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestTypeScript_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewTypeScript().Extract(context.Background(), "src/service.ts", tsSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "typescript" {
			t.Errorf("node %q language=%q, want typescript", n.Name, n.Language)
		}
		if n.Path != "src/service.ts" {
			t.Errorf("node %q path=%q, want src/service.ts", n.Name, n.Path)
		}
	}
}

func TestTypeScript_Extensions(t *testing.T) {
	if exts := NewTypeScript().Extensions(); !slices.Equal(exts, []string{".ts"}) {
		t.Errorf("TypeScript Extensions() = %v, want [.ts]", exts)
	}
	if exts := NewTSX().Extensions(); !slices.Equal(exts, []string{".tsx", ".jsx"}) {
		t.Errorf("TSX Extensions() = %v, want [.tsx .jsx]", exts)
	}
}

func TestTypeScript_ClassAndInterfaceFields(t *testing.T) {
	src := []byte(`export class Service {
  readonly name: string = "x";
  count = 0;
  greet(): string { return this.name; }
}

export interface Point {
  x: number;
  readonly y: number;
}
`)
	nodes, edges, err := NewTypeScript().Extract(context.Background(), "s.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "name") {
		t.Errorf("readonly field name should be a constant; consts=%v", names(nodes, topology.KindConstant))
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "count") {
		t.Errorf("mutable field count should be a variable; vars=%v", names(nodes, topology.KindVariable))
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "y") {
		t.Errorf("readonly interface prop y should be a constant; consts=%v", names(nodes, topology.KindConstant))
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "x") {
		t.Errorf("interface prop x should be a variable; vars=%v", names(nodes, topology.KindVariable))
	}
	if conf, ok := containedAt(nodes, edges, "count"); !ok || conf != 1.0 {
		t.Errorf("field count should be contained at 1.0; got conf=%v ok=%v", conf, ok)
	}
}

// TestTSX_TypedArrowNoCascade is the TSX sibling of the typed-arrow guard:
// typed arrows plus real JSX must extract every symbol. TSX nodes are labelled
// language "typescript" so .ts and .tsx symbols search together.
func TestTSX_TypedArrowNoCascade(t *testing.T) {
	src := []byte(`import React from 'react';

type Props = { title: string; count?: number };

export const Widget = ({ title, count = 0 }: Props) => {
  const label = (n: number): string => ` + "`${title}: ${n}`" + `;
  return <h1 className="w">{label(count)}</h1>;
};

export default function App({ items }: { items: string[] }) {
  return (
    <ul>
      {items.map((it: string) => <li key={it}>{it}</li>)}
    </ul>
  );
}

export interface After { x: number }
`)
	nodes, _, err := NewTSX().Extract(context.Background(), "src/widget.tsx", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"Widget", "App"} {
		if !slices.Contains(names(nodes, topology.KindFunction), want) {
			t.Errorf("component %q missing — TSX typed-arrow cascade likely returned; funcs=%v",
				want, names(nodes, topology.KindFunction))
		}
	}
	for _, want := range []string{"Props", "After"} {
		if !slices.Contains(names(nodes, topology.KindType), want) {
			t.Errorf("type %q missing; types=%v", want, names(nodes, topology.KindType))
		}
	}
	for _, n := range nodes {
		if n.Language != "typescript" {
			t.Errorf("TSX node %q language=%q, want typescript (tsx aliases to typescript)", n.Name, n.Language)
		}
	}
}
