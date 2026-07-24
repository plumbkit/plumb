// Package typescript provides a regex-based topology extractor for TypeScript
// and JavaScript source files (.ts, .tsx, .js, .jsx, .mjs, .cjs).
//
// Extraction is heuristic (line-by-line regex scanning, no parser or AST).
// Confidence on all edges is 0.7 — lower than the Python extractor's 0.8
// because JavaScript/TypeScript syntax is noisier without a grammar.
//
// Not wired into topology_pool's extractorCtors directly: TypeScript (.ts) and
// TSX/JSX (.tsx/.jsx) are structurally parsed by the canonical tree-sitter
// grammar compiled to WASM (see internal/topology/extractors/wasmts,
// wasmts.NewTypeScript/NewTSX), and plain JavaScript (.js/.mjs/.cjs) by the
// pure-Go gotreesitter extractor (internal/topology/extractors/treesitter,
// treesitter.NewJavaScript). This package now survives only as the
// wasm-init-failure fallback that wasmts.Extractor.Extract degrades to.
//
// Validation status: unit-tested with fixture files.
package typescript
