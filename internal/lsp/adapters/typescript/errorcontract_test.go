package typescript_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/typescript"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/paths"
)

// TestErrorContract pins the "typescript-language-server <label>: <cause>" strings this adapter
// wraps every transport failure in. They are what plumb surfaces to agents and
// nothing else asserts them, so this is the net under any refactor that moves
// the wrapping elsewhere. The document is written to disk because the harness
// requires a readable file (see conformance.RunErrorContract).
func TestErrorContract(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "main.ts")
	if err := os.WriteFile(doc, []byte("export const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	conformance.RunErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return typescript.New(c) },
		typescript.DefaultInitParams, "typescript-language-server", paths.PathToURI(doc))
}
