package treesitter

import "github.com/plumbkit/plumb/internal/topology"

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
}

func allExtractorCases() []extractorCase {
	return []extractorCase{
		{
			"python", func() topology.Extractor { return NewPython() }, "a.py",
			"def f():\n    return g()\n\n\ndef g():\n    pass\n",
		},
		{
			"javascript", func() topology.Extractor { return NewJavaScript() }, "a.js",
			"export function f() { return g(); }\nfunction g() { return 1; }\n",
		},
		{
			"typescript", func() topology.Extractor { return NewTypeScript() }, "a.ts",
			"export const add = (a: number, b: number): number => a + b;\nexport function go(): number { return add(1, 2); }\n",
		},
		{
			"tsx", func() topology.Extractor { return NewTSX() }, "a.tsx",
			"type P = { t: string };\nexport const W = ({ t }: P) => <h1>{t}</h1>;\n",
		},
		{
			"rust", func() topology.Extractor { return NewRust() }, "a.rs",
			"pub fn f() -> i32 { g() }\n\nfn g() -> i32 { 1 }\n",
		},
		{
			"zig", func() topology.Extractor { return NewZig() }, "a.zig",
			"pub fn f() void {}\n\npub fn g() void {}\n",
		},
		{
			"kotlin", func() topology.Extractor { return NewKotlin() }, "a.kt",
			"class C {\n    fun go() {}\n}\n",
		},
		{
			"swift", func() topology.Extractor { return NewSwift() }, "a.swift",
			"class VC {\n    var m: Manager!\n    func go() {}\n}\n",
		},
		{
			"java", func() topology.Extractor { return NewJava() }, "A.java",
			"class A {\n    void go() {}\n}\n",
		},
		{
			"bash", func() topology.Extractor { return NewBash() }, "a.sh",
			"#!/bin/sh\nf() {\n  echo hi\n}\nf\n",
		},
		{
			"hcl", func() topology.Extractor { return NewHCL() }, "a.tf",
			"resource \"aws_s3_bucket\" \"b\" {\n  bucket = \"x\"\n}\n",
		},
		{
			"sql", func() topology.Extractor { return NewSQL() }, "a.sql",
			"CREATE TABLE t (id INT);\n",
		},
		{
			"dockerfile", func() topology.Extractor { return NewDockerfile() }, "Dockerfile",
			"FROM alpine:3\nRUN echo hi\n",
		},
		{
			"toml", func() topology.Extractor { return NewTOML() }, "a.toml",
			"[section]\nkey = \"value\"\n",
		},
		{
			"yaml", func() topology.Extractor { return NewYAML() }, "a.yaml",
			"key: value\nlist:\n  - one\n",
		},
		{
			"markdown", func() topology.Extractor { return NewMarkdown() }, "a.md",
			"# Title\n\nText.\n\n## Section\n\nMore.\n",
		},
		{
			"html", func() topology.Extractor { return NewHTML() }, "a.html",
			"<html><body><div id=\"x\">hi</div></body></html>\n",
		},
	}
}
