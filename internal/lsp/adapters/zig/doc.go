// Package zig is the plumb adapter for zls, the Zig language server.
//
// Validation status: experimental — unit-tested with a mocked JSON-RPC
// transport. An integration test (gated with the "integration" build tag)
// spawns a real zls binary against testdata/zig-fixture/ and confirms
// document-symbol extraction plus a DidChangeWatchedFiles + DidOpen →
// diagnostics round-trip, but zls is not installed on the validation machine,
// so that test skips until the binary is on PATH. Promote to "validated" once
// it has run green against a real zls.
//
// Install zls from https://github.com/zigtools/zls (or `brew install zls`).
// Note: zls and tree-sitter-zig track the Zig language version — a real ongoing
// maintenance surface, as Zig is pre-1.0.
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/zig/
package zig
