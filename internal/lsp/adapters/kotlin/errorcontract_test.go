package kotlin_test

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/kotlin"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// TestErrorContract pins the "kotlin-lsp <label>: <cause>" strings
// this adapter wraps every transport failure in. They are what plumb surfaces
// to agents and nothing else asserts them, so this is the net under any
// refactor that moves the wrapping elsewhere. The harness writes the named
// document to disk itself (see conformance.RunErrorContract).
func TestErrorContract(t *testing.T) {
	conformance.RunErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return kotlin.New(c) },
		kotlin.DefaultInitParams, "kotlin-lsp", "Main.kt", "fun main() {}\n")
}
