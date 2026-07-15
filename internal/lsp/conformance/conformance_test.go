package conformance_test

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// rogueClient wraps a real adapter and additionally emits one notification
// method the lsptest fake does not recognise as part of the standard LSP
// client lifecycle, immediately after Initialized — simulating an adapter
// bug where plumb starts sending a server something it never should. The
// strictness contract says RunConformance must FAIL such an exchange after
// the fact (notifications have no error return a fake could reject with).
type rogueClient struct {
	*gopls.Adapter
	caller jsonrpc.Caller
}

func (r *rogueClient) Initialized(ctx context.Context) error {
	if err := r.Adapter.Initialized(ctx); err != nil {
		return err
	}
	return r.caller.Notify(ctx, "$/rogueNotification", nil)
}

func metaScenario() lsptest.Scenario {
	return lsptest.Scenario{
		Name: "strictness meta scenario", RootURI: "file:///workspace/meta",
		DocumentURI: "file:///workspace/meta/main.go", LanguageID: "go",
		Source: "package main", Mode: lsptest.Push,
		Diagnostic: protocol.Diagnostic{Severity: protocol.SevError, Message: "boom"},
	}
}

// runIsolated executes fn as its own top-level test in a private runner and
// reports whether it passed, WITHOUT the inner verdict propagating to the
// calling test — the stdlib way to assert that a test helper's t.Errorf
// wiring actually fails a test (an ordinary nested t.Run cannot: a failing
// subtest always fails its parent). A failing inner run prints its
// "--- FAIL: …" lines into this package's output; that is expected and does
// not affect the outer verdict.
func runIsolated(name string, fn func(*testing.T)) (ok bool) {
	return testing.RunTests(
		func(_, _ string) (bool, error) { return true, nil },
		[]testing.InternalTest{{Name: name, F: fn}},
	)
}

// TestRunConformance_FailsOnUnexpectedNotification proves the strictness
// wiring in RunConformance's newAdapter — the t.Cleanup that t.Errorf's when
// the fake recorded an unexpected notification — actually fails a test. A
// future refactor that downgrades the Errorf to a log (or drops the cleanup)
// turns the doomed inner run green and this meta-test red.
func TestRunConformance_FailsOnUnexpectedNotification(t *testing.T) {
	// Positive control first: the identical scenario through the plain
	// adapter passes, so a failure below is attributable ONLY to the rogue
	// notification.
	clean := runIsolated("control-clean-adapter", func(inner *testing.T) {
		conformance.RunConformance(inner, func(c jsonrpc.Caller) lsp.Client { return gopls.New(c) }, gopls.DefaultInitParams, metaScenario())
	})
	if !clean {
		t.Fatal("control run with a well-behaved adapter failed — cannot attribute the doomed run's failure to the rogue notification")
	}

	doomed := runIsolated("doomed-rogue-notification", func(inner *testing.T) {
		conformance.RunConformance(inner, func(c jsonrpc.Caller) lsp.Client {
			return &rogueClient{Adapter: gopls.New(c), caller: c}
		}, gopls.DefaultInitParams, metaScenario())
	})
	if doomed {
		t.Fatal("RunConformance passed although the adapter sent a disallowed notification — the unexpected-notification cleanup no longer fails the test")
	}
}
