package treesitter

import (
	"context"
	"slices"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/golimpio/plumb/internal/topology"
)

// fidelityCase is one extractor's idiomatic sample plus the symbol that sits
// last in the file. A grammar cascade (the gotreesitter lex-states class of bug
// the TypeScript guard catches) drops every symbol after the break, so asserting
// the trailing symbol survives is a recall-preserving fidelity guard.
type fidelityCase struct {
	name        string
	grammar     func() *tsg.Language
	extractor   func() topology.Extractor
	src         string
	wantTrailer string // a declaration near EOF that a cascade would swallow
}

func fidelityCases() []fidelityCase {
	return []fidelityCase{
		{"python", grammars.PythonLanguage, func() topology.Extractor { return NewPython() }, string(pySrc), "helper_func"},
		{"bash", grammars.BashLanguage, func() topology.Extractor { return NewBash() }, `#!/usr/bin/env bash
set -euo pipefail

readonly NAME="plumb"

log() {
  echo "$1"
}

deploy() {
  log "deploying ${NAME}"
}
`, "deploy"},
		{"dockerfile", grammars.DockerfileLanguage, func() topology.Extractor { return NewDockerfile() }, `FROM golang:1.22 AS builder
ENV CGO_ENABLED=0
ARG VERSION=dev

FROM alpine:3.19 AS runtime
ENV PORT=8080
`, "runtime"},
		{"hcl", grammars.HclLanguage, func() topology.Extractor { return NewHCL() }, `variable "region" {
  default = "us-east-1"
}

resource "aws_instance" "web" {
  ami = "ami-123"
}

output "ip" {
  value = "1.2.3.4"
}
`, "ip"},
		{"sql", grammars.SqlLanguage, func() topology.Extractor { return NewSQL() }, `CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE INDEX idx_users_name ON users(name);
`, "idx_users_name"},
		{"toml", grammars.TomlLanguage, func() topology.Extractor { return NewTOML() }, `[server]
host = "localhost"
port = 8080

[database]
url = "postgres://localhost"
`, "database"},
		{"yaml", grammars.YamlLanguage, func() topology.Extractor { return NewYAML() }, `services:
  web:
    image: nginx
  db:
    image: postgres
`, "db"},
		{"markdown", grammars.MarkdownLanguage, func() topology.Extractor { return NewMarkdown() }, `# Title

## Section A

Some text.

## Section B
`, "Section B"},
		{"java", grammars.JavaLanguage, func() topology.Extractor { return NewJava() }, `package demo;

import java.util.List;

public class Service {
    private final String name = "x";

    public String greet() {
        return name;
    }
}

interface Handler {
    void handle();
}
`, "Handler"},
		{"swift", grammars.SwiftLanguage, func() topology.Extractor { return NewSwift() }, `import Foundation

struct Point {
    let x: Int
    var y: Int
}

protocol Drawable {
    func draw()
}
`, "Drawable"},
		{"kotlin", grammars.KotlinLanguage, func() topology.Extractor { return NewKotlin() }, `package demo

import kotlin.math.max

class Service {
    val name = "x"
    fun greet(): String = name
}

interface Handler {
    fun handle()
}
`, "Handler"},
		{"rust", grammars.RustLanguage, func() topology.Extractor { return NewRust() }, `use std::fmt;

pub struct Point {
    pub x: i32,
    pub y: i32,
}

impl Point {
    pub fn origin() -> Point {
        Point { x: 0, y: 0 }
    }
}

pub trait Drawable {
    fn draw(&self);
}
`, "Drawable"},
		{"zig", grammars.ZigLanguage, func() topology.Extractor { return NewZig() }, `const std = @import("std");

const Point = struct {
    x: i32,
    y: i32,

    pub fn init() Point {
        return Point{ .x = 0, .y = 0 };
    }
};

fn helper() void {}
`, "helper"},
		{"javascript", grammars.JavascriptLanguage, func() topology.Extractor { return NewJavaScript() }, `import { readFile } from "fs";

const MAX = 10;

export function add(a, b) {
  return a + b;
}

class Service {
  greet() {
    return "hi";
  }
}
`, "Service"},
		{"typescript", grammars.TypescriptLanguage, func() topology.Extractor { return NewTypeScript() }, `import { readFile } from "fs";

export interface Point {
  x: number;
  y: number;
}

export class Service {
  greet(name: string): string {
    return ` + "`hi ${name}`" + `;
  }
}

export function add(a: number, b: number): number {
  return a + b;
}
`, "add"},
	}
}

// TestExtractors_ParseFidelity is the cross-language grammar-regression guard.
// For every tree-sitter extractor it asserts (1) the grammar parses an idiomatic
// sample with no ERROR/MISSING nodes, and (2) the extractor still emits the
// trailing declaration — together catching the lex-states cascade class of bug
// generically, not just for TypeScript.
func TestExtractors_ParseFidelity(t *testing.T) {
	// TypeScript's external lex-states must be registered before its grammar is
	// loaded (NewTypeScript does this; call it explicitly so the raw parse below
	// uses the configured grammar regardless of table order).
	registerTSLexStates()

	for _, tc := range fidelityCases() {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := tsg.NewParser(tc.grammar()).Parse([]byte(tc.src))
			if err != nil || tree == nil {
				t.Fatalf("%s: parse failed: %v", tc.name, err)
			}
			if tree.RootNode().HasError() {
				t.Errorf("%s: idiomatic sample parses with ERROR/MISSING nodes — grammar fidelity regression", tc.name)
			}

			nodes, _, err := tc.extractor().Extract(context.Background(), "sample"+tc.name, []byte(tc.src))
			if err != nil {
				t.Fatalf("%s: Extract: %v", tc.name, err)
			}
			if !hasNodeNamed(nodes, tc.wantTrailer) {
				t.Errorf("%s: trailing symbol %q missing — a cascade may be dropping trailing symbols; nodes=%v",
					tc.name, tc.wantTrailer, nodeNames(nodes))
			}
		})
	}
}

// TestExtractors_MalformedInputNoPanic feeds each extractor truncated/garbage
// bytes and asserts Extract returns without panicking (a panic would fail the
// test). safeExtract is the daemon's backstop, but extractors should also be
// internally robust.
func TestExtractors_MalformedInputNoPanic(t *testing.T) {
	garbage := [][]byte{
		[]byte("\x00\x01\x02 not real source {{{"),
		[]byte("class \nfunc ( { ] ) unterminated"),
		[]byte("ENV ARG FROM ::: === <<<"),
		[]byte("\xff\xfe invalid utf8 \xc3\x28"),
	}
	for _, tc := range fidelityCases() {
		for i, g := range garbage {
			// Extract must not panic; the result is unconstrained.
			if _, _, err := tc.extractor().Extract(context.Background(), "broken"+tc.name, g); err != nil {
				t.Errorf("%s garbage[%d]: Extract returned error (want graceful nil): %v", tc.name, i, err)
			}
		}
	}
}

func hasNodeNamed(nodes []topology.Node, name string) bool {
	for _, n := range nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

// containedAt returns the confidence of a contains-edge pointing at the node
// named childName, and whether one exists. Used by the field-extraction tests
// to assert a member is attached to its enclosing type; fixtures use distinct
// names so the first match is unambiguous.
func containedAt(nodes []topology.Node, edges []topology.Edge, childName string) (float64, bool) {
	childID := int64(-1)
	for i, n := range nodes {
		if n.Name == childName {
			childID = int64(i)
			break
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.ToID == childID {
			return e.Confidence, true
		}
	}
	return 0, false
}

func nodeNames(nodes []topology.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, string(n.Kind)+":"+n.Name)
	}
	return out
}

// TestExtractors_MemberConventions enforces the cross-language contract: an
// immutable value member/binding is a KindConstant, a mutable one a
// KindVariable, and a function-local binding is never surfaced. Run across every
// code-language extractor so the conventions in extractor-conventions.md can't
// drift per-language.
func TestExtractors_MemberConventions(t *testing.T) {
	type mc struct {
		name                          string
		ex                            func() topology.Extractor
		file, src                     string
		wantConst, wantVar, localGone string
	}
	cases := []mc{
		{
			"java", func() topology.Extractor { return NewJava() }, "C.java",
			"class C {\n  final int IMMUT = 1;\n  int mut = 2;\n  void m() { int localv = 3; }\n}\n",
			"IMMUT", "mut", "localv",
		},
		{
			"swift", func() topology.Extractor { return NewSwift() }, "C.swift",
			"class C {\n  let immut = 1\n  var mut = 2\n  func m() { let localv = 3 }\n}\n",
			"immut", "mut", "localv",
		},
		{
			"kotlin", func() topology.Extractor { return NewKotlin() }, "C.kt",
			"class C {\n  val immut = 1\n  var mut = 2\n  fun m() { val localv = 3 }\n}\n",
			"immut", "mut", "localv",
		},
		{
			"rust", func() topology.Extractor { return NewRust() }, "c.rs",
			"const IMMUT: i32 = 1;\nstatic MUT: i32 = 2;\nfn f() {\n  let lv = 3;\n  const LOCALC: i32 = 4;\n}\n",
			"IMMUT", "MUT", "LOCALC",
		},
		{
			"zig", func() topology.Extractor { return NewZig() }, "c.zig",
			"const IMMUT = 1;\nvar mutv: i32 = 2;\nfn f() void {\n  const localc = 3;\n}\n",
			"IMMUT", "mutv", "localc",
		},
		{
			"javascript", func() topology.Extractor { return NewJavaScript() }, "c.js",
			"const IMMUT = 1;\nlet mutv = 2;\nfunction f() { const localc = 3; }\n",
			"IMMUT", "mutv", "localc",
		},
		{
			"typescript", func() topology.Extractor { return NewTypeScript() }, "c.ts",
			"const IMMUT = 1;\nlet mutv = 2;\nfunction f(): void { const localc = 3; }\n",
			"IMMUT", "mutv", "localc",
		},
		{
			"python", func() topology.Extractor { return NewPython() }, "c.py",
			"IMMUT_C = 1\nmutv = 2\ndef f():\n    localc = 3\n",
			"IMMUT_C", "mutv", "localc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, _, err := tc.ex().Extract(context.Background(), tc.file, []byte(tc.src))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if !slices.Contains(names(nodes, topology.KindConstant), tc.wantConst) {
				t.Errorf("immutable %q should be KindConstant; consts=%v", tc.wantConst, names(nodes, topology.KindConstant))
			}
			if !slices.Contains(names(nodes, topology.KindVariable), tc.wantVar) {
				t.Errorf("mutable %q should be KindVariable; vars=%v", tc.wantVar, names(nodes, topology.KindVariable))
			}
			if hasNodeNamed(nodes, tc.localGone) {
				t.Errorf("function-local %q must not be surfaced; nodes=%v", tc.localGone, nodeNames(nodes))
			}
		})
	}
}

// TestRust_UnionFieldsAndEnumVariants covers the Rust member edge cases: union
// fields are variables, enum variants are constants (matching other languages),
// and a struct-variant's inner field is not surfaced.
func TestRust_UnionFieldsAndEnumVariants(t *testing.T) {
	src := []byte("pub union U {\n    a: i32,\n    b: f32,\n}\n\npub enum E {\n    Plain,\n    WithData(i32),\n    Struct { inner: i32 },\n}\n")
	nodes, _, err := NewRust().Extract(context.Background(), "u.rs", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a", "b"} {
		if !slices.Contains(names(nodes, topology.KindVariable), f) {
			t.Errorf("union field %q should be KindVariable; vars=%v", f, names(nodes, topology.KindVariable))
		}
	}
	for _, v := range []string{"Plain", "WithData", "Struct"} {
		if !slices.Contains(names(nodes, topology.KindConstant), v) {
			t.Errorf("enum variant %q should be KindConstant; consts=%v", v, names(nodes, topology.KindConstant))
		}
	}
	if hasNodeNamed(nodes, "inner") {
		t.Errorf("struct-variant inner field should not be surfaced; nodes=%v", nodeNames(nodes))
	}
}

// TestTypeScript_PrivateAndStaticFields confirms accessibility/static modifiers
// don't hide a class field, and readonly still wins for const classification.
func TestTypeScript_PrivateAndStaticFields(t *testing.T) {
	src := []byte("class C {\n  private id: number = 0;\n  static readonly KIND = \"c\";\n  protected name = \"x\";\n}\n")
	nodes, _, err := NewTypeScript().Extract(context.Background(), "c.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "id") {
		t.Errorf("private field id should be KindVariable; vars=%v", names(nodes, topology.KindVariable))
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "name") {
		t.Errorf("protected field name should be KindVariable; vars=%v", names(nodes, topology.KindVariable))
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "KIND") {
		t.Errorf("static readonly field KIND should be KindConstant; consts=%v", names(nodes, topology.KindConstant))
	}
}
