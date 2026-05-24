// Package typescript is the plumb adapter for typescript-language-server, the
// TypeScript/JavaScript language server (tsserver wrapper).
//
// Validation status: experimental — unit-tested with a mocked JSON-RPC
// transport. An integration test (gated with the "integration" build tag)
// spawns a real typescript-language-server binary against
// testdata/typescript-fixture/ and confirms document-symbol extraction plus a
// DidChangeWatchedFiles + DidOpen → diagnostics round-trip, but the binary is
// not installed on the validation machine, so that test skips until it is on
// PATH. Promote to "validated" once it has run green against a real server.
//
// Install with: npm install -g typescript-language-server typescript
//
// This adapter provides the semantic GPS for TypeScript and JavaScript; the
// structural Map is the regex TS extractor (.ts/.tsx/.jsx) and the tree-sitter
// JavaScript extractor (.js/.mjs/.cjs).
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/typescript/
package typescript
