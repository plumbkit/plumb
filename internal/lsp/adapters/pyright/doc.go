// Package pyright is the plumb adapter for pyright-langserver, the Microsoft
// Python type checker and language server.
//
// Validation status: validated against pyright-langserver.
// Unit tests use a mocked JSON-RPC transport. Integration tests (gated with the
// "integration" build tag) spawn a real pyright-langserver binary against
// testdata/python-fixture/ and confirm that DidChangeWatchedFiles causes pyright
// to publish diagnostics — the same proof that the gopls adapter carries.
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/pyright/
package pyright
