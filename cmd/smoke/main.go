// Package smoke contains the plumb end-to-end integration smoke test.
// It exercises the full stack — MCP wire protocol, daemon, gopls, write
// pipeline, and diagnostics — without a GUI client.
//
// Run: go test -tags=integration -timeout=3m ./cmd/smoke/
package smoke
