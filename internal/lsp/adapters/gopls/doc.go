// Package gopls is the plumb adapter for gopls, the official Go language server.
//
// Validation status: validated against gopls v0.x.
// Integration tests in this package spawn a real gopls binary against
// testdata/go-fixture/ and are gated with the "integration" build tag.
// Run them with: go test -tags=integration ./internal/lsp/adapters/gopls/
package gopls
