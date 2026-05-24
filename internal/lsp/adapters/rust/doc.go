// Package rust is the plumb adapter for rust-analyzer, the official Rust
// language server.
//
// Validation status: validated against rust-analyzer.
// Unit tests use a mocked JSON-RPC transport. Integration tests (gated with the
// "integration" build tag) spawn a real rust-analyzer binary against
// testdata/rust-fixture/ and confirm both document-symbol extraction and that
// the DidChangeWatchedFiles + DidOpen pipeline causes rust-analyzer to publish
// diagnostics — the same proof the gopls and pyright adapters carry.
//
// Cold-start warning: rust-analyzer loads the sysroot and runs `cargo metadata`
// on first attach; on a large workspace this can take minutes. The adapter
// itself does nothing special for this — it is the canonical "unavailability"
// case the structural (tree-sitter) layer covers while the server warms.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/rust/
package rust
