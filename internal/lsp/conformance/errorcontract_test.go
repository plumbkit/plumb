package conformance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// driftedClient wraps a real gopls adapter and reports one method's failure
// with a reworded, unwrapped error — precisely the regression RunErrorContract
// exists to catch. A refactor that hoists the wrapping into a shared base
// package and reorders the label, or drops the %w, looks exactly like this.
type driftedClient struct {
	*gopls.Adapter
}

func (d *driftedClient) Supertypes(context.Context, protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, errors.New("gopls supertypes: transport unavailable")
}

// TestRunErrorContract_FailsOnDriftedLabel proves the harness's assertions are
// load-bearing: if RunErrorContract ever stopped comparing the rendered string
// (or stopped checking errors.Is), this doomed run would turn green and this
// meta-test red. runIsolated is defined in conformance_test.go; the inner
// "--- FAIL: …" lines it prints are expected and do not affect this verdict.
func TestRunErrorContract_FailsOnDriftedLabel(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(doc, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	docURI := paths.PathToURI(doc)

	ok := runIsolated("doomedErrorContract", func(t *testing.T) {
		conformance.RunErrorContract(t,
			func(c jsonrpc.Caller) lsp.Client { return &driftedClient{Adapter: gopls.New(c)} },
			gopls.DefaultInitParams, "gopls", docURI)
	})
	if ok {
		t.Fatal("RunErrorContract passed an adapter whose typeHierarchy/supertypes label drifted; the harness is not pinning error strings")
	}
}
