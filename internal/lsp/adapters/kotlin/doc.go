// Package kotlin is the plumb adapter for kotlin-language-server (the
// fwcd/kotlin-language-server project).
//
// Validation status: experimental — unit-tested with a mocked JSON-RPC
// transport. An integration test (gated with the "integration" build tag)
// spawns a real kotlin-language-server binary against testdata/kotlin-fixture/
// and confirms document-symbol extraction plus a DidChangeWatchedFiles +
// DidOpen → diagnostics round-trip, but the binary is not installed on the
// validation machine, so that test skips until it is on PATH. Promote to
// "validated" once it has run green against a real server.
//
// Install with `brew install kotlin-language-server` or build from
// https://github.com/fwcd/kotlin-language-server (needs a JDK).
//
// Run integration tests with: go test -tags=integration ./internal/lsp/adapters/kotlin/
package kotlin
