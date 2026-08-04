// Package typescript provides a regex-based topology extractor for TypeScript
// and JavaScript source files (.ts, .tsx, .js, .jsx, .mjs, .cjs).
//
// Extraction is heuristic (line-by-line regex scanning, no parser or AST).
// Confidence on all edges is 0.7 — lower than the Python extractor's 0.8
// because JavaScript/TypeScript syntax is noisier without a grammar.
//
// Not wired into topology_pool's extractorCtors: TypeScript (.ts), TSX/JSX
// (.tsx/.jsx) and plain JavaScript (.js/.mjs/.cjs) are all structurally parsed
// by the pure-Go gotreesitter extractors (internal/topology/extractors/
// treesitter) since the per-language TS flip. This package is no longer
// reachable in production — it was the wasmts TS init-failure and parse-fault
// fallback, and the wasmts TS path itself is now only the parity-sweep
// reference — and it is retained solely for that harness until wasmts retires
// with the Swift flip.
//
// Validation status: unit-tested with fixture files.
package typescript
