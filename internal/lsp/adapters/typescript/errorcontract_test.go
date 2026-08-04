package typescript_test

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/typescript"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// TestErrorContract pins the "typescript-language-server <label>: <cause>"
// strings this adapter wraps every transport failure in. They are what plumb
// surfaces to agents and nothing else asserts them, so this is the net under
// any refactor that moves the wrapping elsewhere. The harness writes the named
// document to disk itself (see conformance.RunErrorContract).
func TestErrorContract(t *testing.T) {
	conformance.RunErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return typescript.New(c) },
		typescript.DefaultInitParams, "typescript-language-server", "main.ts", "export const x = 1;\n")
}
