// Package typescript provides a regex-based topology extractor for TypeScript
// and JavaScript source files (.ts, .tsx, .js, .jsx, .mjs, .cjs).
//
// Extraction is heuristic (line-by-line regex scanning, no parser or AST).
// Confidence on all edges is 0.7 — lower than the Python extractor's 0.8
// because JavaScript/TypeScript syntax is noisier without a grammar.
//
// Validation status: unit-tested with fixture files.
package typescript
