// Package html is the plumb adapter for vscode-html-language-server, the HTML
// language server extracted from VS Code (built on the vscode-languageserver
// framework, the same base as the TypeScript and CSS servers).
//
// Validation status: experimental — unit-tested with a mocked JSON-RPC
// transport. An integration test (gated with the "integration" build tag)
// spawns a real vscode-html-language-server binary against
// testdata/html-fixture/ and confirms document-symbol extraction plus a
// DidChangeWatchedFiles + DidOpen → diagnostics round-trip, but the binary is
// not installed on the validation machine, so that test skips until it is on
// PATH. Promote to "validated" once it has run green against a real server.
//
// Install with: npm install -g vscode-langservers-extracted
// (provides vscode-html-language-server, vscode-css-language-server, …).
//
// This adapter provides the semantic GPS for HTML; the structural Map is the
// tree-sitter HTML extractor (.html/.htm). The server's strengths are
// document-symbol outlines, hover, completion, and validation of embedded
// CSS/JavaScript; it does not implement workspace/symbol, call hierarchy, or
// type hierarchy, so those forward to the server and return its empty/
// unsupported response — the Client interface is satisfied structurally.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/html/
package html
