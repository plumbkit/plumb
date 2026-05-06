// Package pyright is the plumb adapter for pyright-langserver, the Microsoft
// Python type checker and language server.
//
// Validation status: implementation complete, not validated against pyright binary.
// Unit tests in this package use a mocked JSON-RPC transport and do not require
// a pyright binary on PATH. No real pyright binary is needed in CI.
//
// To promote this adapter to validated, add integration tests (gated with the
// "integration" build tag) that spawn a real pyright-langserver binary against
// testdata/python-fixture/ and update this doc comment accordingly.
package pyright
