package treesitter

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// extractorCase is one pure-Go extractor in this package plus a minimal source
// sample that must yield at least one symbol.
//
// This is the single enumeration of the package's extractors, shared by the
// per-extractor invariant guards: lazy grammar decode
// (TestExtractorConstructionDecodesNoGrammar) and parse-arena recycling
// (TestExtractReleasesArenaForReuse). Both guards previously kept their own
// hand-written list, and both lists went stale the same way — the pure-Go TS/TSX
// extractor was in neither, so it shipped decoding its grammar eagerly AND
// leaking a parse arena per file while both guards stayed green. Add a new
// extractor to this list and every guard covers it at once.
type extractorCase struct {
	name string
	ctor func() topology.Extractor
	path string
	src  string
	// wantImport is the KindImport name the sample must yield, for the
	// languages that have an import concept at all. It keeps the samples honest
	// for TestExtractorsEmitNonInvertedRanges: the inverted-range defect lived
	// exclusively in import nodes, so a sample that quietly lost its import
	// would make that guard vacuous without failing anything.
	wantImport string
}

func allExtractorCases() []extractorCase {
	return []extractorCase{
		{
			// The sample carries a constant, an attr_* member and an RSpec block
			// as well as the import and the defs: Ruby emits from six sites and
			// only three were exercised, so deleting setSpan from addConstant or
			// addAttrs left TestExtractorsEmitByteSpans green.
			"ruby", func() topology.Extractor { return NewRuby() }, "a.rb",
			"require 'json'\n\nclass Invoice\n  RATE = 1\n  attr_accessor :total\n\n  def sum\n    helper\n  end\n\n  def helper\n    1\n  end\nend\n\nRSpec.describe Invoice do\n  it \"sums\" do\n    expect(1).to eq(1)\n  end\nend\n", "json",
		},
		{
			"c", func() topology.Extractor { return NewC() }, "a.c",
			"#include <stdio.h>\n\nstruct P { int x; };\n\nstatic int helper(int v) { return v; }\n\nint add(int a) { return helper(a); }\n", "stdio.h",
		},
		{
			// JSON has no import concept, so wantImport is deliberately empty.
			"json", func() topology.Extractor { return NewJSON() }, "a.json",
			"{\n  \"name\": \"pkg\",\n  \"scripts\": { \"build\": \"vite build\" }\n}\n", "",
		},
		{
			"css", func() topology.Extractor { return NewCSS() }, "a.css",
			"@import \"base.css\";\n\n.btn {\n  color: red;\n}\n", "base.css",
		},
		{
			"python", func() topology.Extractor { return NewPython() }, "a.py",
			"import os\n\n\ndef f():\n    return g()\n\n\ndef g():\n    pass\n", "os",
		},
		{
			"javascript", func() topology.Extractor { return NewJavaScript() }, "a.js",
			"import { readFile } from 'fs';\nexport function f() { return g(); }\nfunction g() { return 1; }\n", "fs",
		},
		{
			"typescript", func() topology.Extractor { return NewTypeScript() }, "a.ts",
			"import { readFile } from 'fs';\nexport const add = (a: number, b: number): number => a + b;\nexport function go(): number { return add(1, 2); }\n", "fs",
		},
		{
			"tsx", func() topology.Extractor { return NewTSX() }, "a.tsx",
			"import React from 'react';\ntype P = { t: string };\nexport const W = ({ t }: P) => <h1>{t}</h1>;\n", "react",
		},
		{
			"rust", func() topology.Extractor { return NewRust() }, "a.rs",
			"use std::fmt;\n\npub fn f() -> i32 { g() }\n\nfn g() -> i32 { 1 }\n", "std::fmt",
		},
		{
			"zig", func() topology.Extractor { return NewZig() }, "a.zig",
			"const std = @import(\"std\");\n\npub fn f() void {}\n\npub fn g() void {}\n", "std",
		},
		{
			"kotlin", func() topology.Extractor { return NewKotlin() }, "a.kt",
			"import com.example.User\n\nclass C {\n    fun go() {}\n}\n", "com.example.User",
		},
		{
			"swift", func() topology.Extractor { return NewSwift() }, "a.swift",
			"import Foundation\n\nclass VC {\n    var m: Manager!\n    func go() {}\n}\n", "Foundation",
		},
		{
			"java", func() topology.Extractor { return NewJava() }, "A.java",
			"import java.util.List;\n\nclass A {\n    void go() {}\n}\n", "java.util.List",
		},
		{
			"bash", func() topology.Extractor { return NewBash() }, "a.sh",
			"#!/bin/sh\nsource ./lib/common.sh\nf() {\n  echo hi\n}\nf\n", "./lib/common.sh",
		},
		{
			"hcl", func() topology.Extractor { return NewHCL() }, "a.tf",
			"module \"vpc\" {\n  source = \"./vpc\"\n}\n\nresource \"aws_s3_bucket\" \"b\" {\n  bucket = \"x\"\n}\n", "vpc",
		},
		{
			"sql", func() topology.Extractor { return NewSQL() }, "a.sql",
			"CREATE TABLE t (id INT);\n", "",
		},
		{
			"dockerfile", func() topology.Extractor { return NewDockerfile() }, "Dockerfile",
			"FROM alpine:3\nRUN echo hi\n", "",
		},
		{
			"toml", func() topology.Extractor { return NewTOML() }, "a.toml",
			"[section]\nkey = \"value\"\n", "",
		},
		{
			"yaml", func() topology.Extractor { return NewYAML() }, "a.yaml",
			"key: value\nlist:\n  - one\n", "",
		},
		{
			"markdown", func() topology.Extractor { return NewMarkdown() }, "a.md",
			"# Title\n\nText.\n\n## Section\n\nMore.\n", "",
		},
		{
			"html", func() topology.Extractor { return NewHTML() }, "a.html",
			"<html><head><script src=\"/js/app.js\"></script></head>\n<body><div id=\"x\">hi</div></body></html>\n",
			"/js/app.js",
		},
	}
}

// TestExtractorsEmitByteSpans guards the setSpan discipline: every extractor
// stamps HasBytes plus a byte-precise span on the symbols it emits. Without
// them the symbol-edit fallback (nodeToDocSymbol) degrades to line-granular
// ranges — and on a single-line declaration every member collapses onto the
// same range, so a fallback body replace would rewrite its siblings. The
// pure-Go TS/TSX extractor shipped without spans while the parity sweep stayed
// green; this table makes that miss unrepeatable for the next extractor too.
func TestExtractorsEmitByteSpans(t *testing.T) {
	for _, c := range allExtractorCases() {
		t.Run(c.name, func(t *testing.T) {
			nodes, _, err := c.ctor().Extract(context.Background(), c.path, []byte(c.src))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if len(nodes) == 0 {
				t.Fatal("no symbols extracted; the sample is wrong")
			}
			for _, n := range nodes {
				if !n.HasBytes || n.EndByte <= n.StartByte {
					t.Errorf("%s %q has no byte span (HasBytes=%v, bytes %d-%d)",
						n.Kind, n.Name, n.HasBytes, n.StartByte, n.EndByte)
				}
			}
		})
	}
}

// TestExtractorsEmitNonInvertedRanges guards the line range on every emitted
// symbol: EndLine must never precede StartLine. Import nodes were the whole gap
// — every extractor that emits one set StartLine and left EndLine at its zero
// value, so an import on line 12 was persisted as the range 12–0, inverted for
// any consumer that turns a node into a range. It survived because no sample in
// the table above contained an import, so TestExtractorsEmitByteSpans never
// looked at one; every sample that can carry an import now does, and wantImport
// keeps it that way.
func TestExtractorsEmitNonInvertedRanges(t *testing.T) {
	for _, c := range allExtractorCases() {
		t.Run(c.name, func(t *testing.T) {
			nodes, _, err := c.ctor().Extract(context.Background(), c.path, []byte(c.src))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			sawImport := false
			for _, n := range nodes {
				if n.EndLine < n.StartLine {
					t.Errorf("%s %q has an inverted line range %d-%d",
						n.Kind, n.Name, n.StartLine, n.EndLine)
				}
				if n.Kind == topology.KindImport && n.Name == c.wantImport {
					sawImport = true
				}
			}
			if c.wantImport != "" && !sawImport {
				t.Errorf("sample no longer yields import %q — the guard would go vacuous; imports=%v",
					c.wantImport, importNames(nodes))
			}
		})
	}
}

func importNames(nodes []topology.Node) []string {
	var out []string
	for _, n := range nodes {
		if n.Kind == topology.KindImport {
			out = append(out, n.Name)
		}
	}
	return out
}
