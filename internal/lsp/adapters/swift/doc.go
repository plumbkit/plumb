// Package swift is the plumb adapter for sourcekit-lsp, Apple's language server
// for Swift (and C/C++/Objective-C).
//
// Validation status: validated against sourcekit-lsp.
// Unit tests use a mocked JSON-RPC transport. Integration tests (gated with the
// "integration" build tag) spawn a real sourcekit-lsp binary against
// testdata/swift-fixture/ (a SwiftPM package) and confirm both document-symbol
// extraction and that the DidChangeWatchedFiles + DidOpen pipeline causes
// sourcekit-lsp to publish diagnostics — the same proof the other adapters
// carry.
//
// Per-document queries (documentSymbol, definition, references, hover, the
// rename/call/type-hierarchy prepares) open the file via textDocument/didOpen
// before querying: sourcekit-lsp serves these only for opened documents and
// otherwise replies -32001 "No language service for <uri> found". plumb's
// external-edit model uses didChangeWatchedFiles rather than the open-document
// lifecycle, so the adapter opens lazily and keeps the file open, closing it on
// a watched-file change. Mirrors the html adapter.
//
// sourcekit-lsp ships with the Swift toolchain (Xcode or a standalone
// toolchain); on macOS it lives at /usr/bin/sourcekit-lsp.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/swift/
package swift
