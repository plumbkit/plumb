// Package typescript is the plumb adapter for typescript-language-server, the
// TypeScript/JavaScript language server (tsserver wrapper).
//
// Validation status: validated — unit-tested with a mocked JSON-RPC transport
// and exercised against a real typescript-language-server 5.3.0 binary via the
// integration tests (gated with the "integration" build tag) against
// testdata/typescript-fixture/: document-symbol extraction and the
// DidChangeWatchedFiles + DidOpen → diagnostics round-trip both pass.
//
// Diagnostics gotcha: typescript-language-server publishes NOTHING unless the
// client advertises the textDocument.publishDiagnostics capability — it does not
// implement pull diagnostics (textDocument/diagnostic returns -32601 and it
// advertises no diagnosticProvider). plumb's DefaultClientCapabilities now
// declares publishDiagnostics, so the existing push pipeline carries TypeScript
// diagnostics. (The earlier "uses pull diagnostics" hypothesis was wrong; the
// pull protocol types and the adapter's Diagnostic method remain as dormant,
// unadvertised infrastructure for servers that genuinely require pull.)
//
// Install with: npm install -g typescript-language-server typescript
//
// This adapter provides the semantic GPS for TypeScript and JavaScript; the
// structural Map is the regex TS extractor (.ts/.tsx/.jsx) and the tree-sitter
// JavaScript extractor (.js/.mjs/.cjs).
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/typescript/
package typescript
