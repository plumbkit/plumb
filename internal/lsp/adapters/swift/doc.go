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
// sourcekit-lsp ships with the Swift toolchain (Xcode or a standalone
// toolchain); on macOS it lives at /usr/bin/sourcekit-lsp.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/swift/
package swift
