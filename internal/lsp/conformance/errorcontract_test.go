package conformance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// driftedClient wraps a real gopls adapter and reports one method's failure
// with a reworded, unwrapped error — precisely the regression RunErrorContract
// exists to catch. A refactor that hoists the wrapping into a shared base
// package and reorders the label looks exactly like this.
type driftedClient struct {
	*gopls.Adapter
}

func (d *driftedClient) Supertypes(context.Context, protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, errors.New("gopls supertypes: transport unavailable")
}

// unwrappedClient is the OTHER half of the same regression, and the one the
// rendered-string comparison cannot see: it re-renders the real adapter's
// message byte for byte and only cuts the %w link — a `%v` where the adapter
// meant `%w`, which is what a base-package extraction would most plausibly get
// wrong. Deriving the text from the real error rather than hardcoding it is
// deliberate: the string is guaranteed to match, so only errors.Is can reject
// this client.
type unwrappedClient struct {
	*gopls.Adapter
}

func (u *unwrappedClient) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	items, err := u.Adapter.Subtypes(ctx, params)
	if err == nil {
		return items, nil
	}
	return nil, errors.New(err.Error())
}

// TestRunErrorContract_FailsOnDriftedLabel proves the harness's assertions are
// load-bearing: if RunErrorContract ever stopped comparing the rendered string,
// or stopped checking errors.Is, one of the two doomed runs below would turn
// green and this meta-test red. runIsolated is defined in conformance_test.go;
// the inner "--- FAIL: …" lines it prints are expected and do not affect this
// verdict.
func TestRunErrorContract_FailsOnDriftedLabel(t *testing.T) {
	newGopls := func(c jsonrpc.Caller) lsp.Client { return gopls.New(c) }

	// Positive control first: the plain adapter passes the same contract, so a
	// failure below is attributable ONLY to the drift each doomed client
	// introduces — and a RunErrorContract that no adapter could satisfy fails
	// here rather than masquerading as a working net.
	clean := runIsolated("control-clean-gopls", func(t *testing.T) {
		conformance.RunErrorContract(t, newGopls, gopls.DefaultInitParams, "gopls", "main.go", "package main\n")
	})
	if !clean {
		t.Fatal("control run with a well-behaved adapter failed — cannot attribute the doomed runs' failures to the injected drift")
	}

	relabelled := runIsolated("doomed-relabelled-error", func(t *testing.T) {
		conformance.RunErrorContract(t,
			func(c jsonrpc.Caller) lsp.Client { return &driftedClient{Adapter: gopls.New(c)} },
			gopls.DefaultInitParams, "gopls", "main.go", "package main\n")
	})
	if relabelled {
		t.Fatal("RunErrorContract passed an adapter whose typeHierarchy/supertypes label drifted; the harness is not pinning error strings")
	}

	unwrapped := runIsolated("doomed-unwrapped-error", func(t *testing.T) {
		conformance.RunErrorContract(t,
			func(c jsonrpc.Caller) lsp.Client { return &unwrappedClient{Adapter: gopls.New(c)} },
			gopls.DefaultInitParams, "gopls", "main.go", "package main\n")
	})
	if unwrapped {
		t.Fatal("RunErrorContract passed an adapter whose typeHierarchy/subtypes error renders correctly but no longer unwraps; the errors.Is half of the harness is gone")
	}
}
