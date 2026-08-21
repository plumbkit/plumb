// Package html is the plumb adapter for vscode-html-language-server, the HTML
// language server extracted from VS Code (built on the vscode-languageserver
// framework, the same base as the TypeScript and CSS servers).
//
// Validation status: validated — vscode-html-language-server 4.10.0
// (vscode-langservers-extracted), Linux, 2026-08-21. The integration test
// (gated with the "integration" build tag) spawns the real binary against
// testdata/html-fixture/ and confirms document-symbol extraction plus the
// DidChangeWatchedFiles + DidOpen → diagnostics round-trip; both pass, and CI
// now installs the binary so they keep running rather than skipping.
//
// Validated describes the round-trip, not the server's reach. This one has no
// filesystem access of its own: it answers only from documents the client has
// opened, so a query against an unopened file returns nothing rather than
// reading it from disk. See the capability notes below.
//
// Install with: npm install -g vscode-langservers-extracted
// (provides vscode-html-language-server, vscode-css-language-server, …).
//
// This adapter provides the semantic GPS for HTML; the structural Map is the
// tree-sitter HTML extractor (.html/.htm). The server's strengths are
// document-symbol outlines, hover, completion, and validation of embedded
// CSS/JavaScript; it does not implement workspace/symbol, call hierarchy, or
// type hierarchy, so those forward to the server and return its empty/
// unsupported response — the Client interface is satisfied structurally. Its
// rename is document-local for the same reason: the server scopes renames to
// matching tag pairs / linked editing ranges within a document, so a
// "workspace-wide" rename here only ever edits the one file.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/html/
package html
