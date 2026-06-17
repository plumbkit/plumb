// Package zig is the plumb adapter for zls, the Zig language server.
//
// Validation status: validated — unit-tested with a mocked JSON-RPC transport,
// and the integration test (gated with the "integration" build tag) runs green
// against a real zls: it spawns the binary against testdata/zig-fixture/ and
// confirms document-symbol extraction AND the DidChangeWatchedFiles + DidOpen →
// publishDiagnostics round-trip. The round-trip began passing once plumb
// advertised the textDocument.publishDiagnostics client capability (the same
// fix that validated typescript-language-server) — the earlier "zls is pull-only
// so it never pushes" hypothesis was wrong. Last green: 2026-06-17 on zls 0.16.
//
// Per-document queries (documentSymbol, definition, references, hover, the
// rename/call/type-hierarchy prepares) open the file via textDocument/didOpen
// before querying: zls resolves nothing for an unopened document. plumb's
// external-edit model uses didChangeWatchedFiles rather than the open-document
// lifecycle, so the adapter opens lazily and keeps the file open, closing it on
// a watched-file change. Mirrors the html adapter.
//
// Install zls from https://github.com/zigtools/zls (or `brew install zls`).
// Note: zls and tree-sitter-zig track the Zig language version — a real ongoing
// maintenance surface, as Zig is pre-1.0.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/zig/
package zig
