// Package conformance runs a reusable adapter contract against lsptest.
package conformance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Factory constructs the adapter under test from a fake jsonrpc.Caller.
type Factory func(jsonrpc.Caller) lsp.Client

// InitParamsFactory builds an adapter's push-first default InitializeParams
// for rootURI — each adapter's conformance_test.go passes its own package's
// DefaultInitParams (gopls.DefaultInitParams, zig.DefaultInitParams, …).
// Routing negotiation through the adapter's OWN params (rather than a
// generic literal built inline here) is what makes negotiation part of what
// this harness proves: for a scenario with Mode Pull or Hybrid, newAdapter
// (below) applies the SAME two-step swap internal/cli/pool_adapters.go's
// initParamsFor performs at runtime — an lsp.PullInitializer adapter (gopls)
// customises its own params via EnablePullDiagnostics; every other adapter
// gets the generic protocol.ClientCapabilitiesFor(true) capability swap.
// conformance cannot import internal/cli to reuse initParamsFor directly:
// internal/cli sits above internal/lsp in the layer stack.
type InitParamsFactory func(rootURI string) protocol.InitializeParams

// pullClient is the optional document-pull surface an adapter may expose.
type pullClient interface {
	SupportsPullDiagnostics() bool
	Diagnostic(context.Context, protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

// workspacePullClient is the optional workspace-pull surface an adapter may
// expose (only gopls implements it today).
type workspacePullClient interface {
	WorkspaceDiagnostic(context.Context, protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error)
}

// adapterFactory builds a fresh adapter + fake server pair from sc, fully
// initialized (Initialize + Initialized already sent). Subtests that need a
// scenario variant (e.g. forcing method-not-found) build a local copy of the
// outer scenario and pass it here instead of reusing the shared one, so each
// subtest's fake state is isolated even when scenarios share most fields.
type adapterFactory func(t *testing.T, sc lsptest.Scenario) (lsp.Client, *lsptest.Caller, context.Context)

// RunConformance runs independent subtests so one failure never suppresses
// the remainder of the adapter contract. Each subtest is guarded on the
// scenario fields it needs (e.g. "pull-only" skips when Mode is Push) and
// t.Skip states why; when a subtest's prerequisites ARE met it runs to
// completion with real assertions — see the per-subtest functions in this
// file and conformance_pull.go for the branching this comment summarises.
func RunConformance(t *testing.T, factory Factory, initParams InitParamsFactory, s lsptest.Scenario) {
	t.Helper()
	newAdapter := func(t *testing.T, sc lsptest.Scenario) (lsp.Client, *lsptest.Caller, context.Context) {
		t.Helper()
		server := lsptest.NewCaller(sc)
		adapter := factory(server)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		t.Cleanup(func() {
			if u := server.UnexpectedNotifications(); len(u) > 0 {
				t.Errorf("adapter sent unexpected notification method(s) %v", u)
			}
		})
		params := initParams(sc.RootURI)
		if sc.Mode == lsptest.Pull || sc.Mode == lsptest.Hybrid {
			if pi, ok := adapter.(lsp.PullInitializer); ok {
				pi.EnablePullDiagnostics(&params)
			} else {
				params.Capabilities = protocol.ClientCapabilitiesFor(true)
			}
		}
		if _, err := adapter.Initialize(ctx, params); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if err := adapter.Initialized(ctx); err != nil {
			t.Fatalf("Initialized: %v", err)
		}
		return adapter, server, ctx
	}

	t.Run("lifecycle", func(t *testing.T) { runLifecycleSubtest(t, s, newAdapter) })
	t.Run("document lifecycle and queries", func(t *testing.T) { runDocumentQueriesSubtest(t, s, newAdapter) })
	t.Run("push baseline", func(t *testing.T) { runPushBaselineSubtest(t, s, newAdapter) })
	t.Run("pull-only", func(t *testing.T) { runPullOnlySubtest(t, s, newAdapter) })
	t.Run("hybrid", func(t *testing.T) { runHybridSubtest(t, s, newAdapter) })
	t.Run("unchanged/result-ID", func(t *testing.T) { runUnchangedResultIDSubtest(t, s, newAdapter) })
	t.Run("related documents", func(t *testing.T) { runRelatedDocumentsSubtest(t, s, newAdapter) })
	t.Run("workspace pull", func(t *testing.T) { runWorkspacePullSubtest(t, s, newAdapter) })
	t.Run("refresh", func(t *testing.T) { runRefreshSubtest(t, s, newAdapter) })
	t.Run("method-not-found downgrade", func(t *testing.T) { runMethodNotFoundDowngradeSubtest(t, s, newAdapter) })
	if s.RegisterWatch {
		t.Run("dynamic watcher registration", func(t *testing.T) { runDynamicWatcherSubtest(t, s, newAdapter) })
	}
}

func runLifecycleSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	adapter, _, ctx := newAdapter(t, s)
	if err := adapter.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := adapter.Exit(ctx); err != nil {
		t.Fatalf("Exit: %v", err)
	}
}

func runDocumentQueriesSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	adapter, _, ctx := newAdapter(t, s)
	doc := protocol.TextDocumentItem{URI: s.DocumentURI, LanguageID: s.LanguageID, Version: 1, Text: s.Source}
	if err := adapter.DidOpen(ctx, protocol.DidOpenTextDocumentParams{TextDocument: doc}); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	if _, err := adapter.DocumentSymbols(ctx, protocol.DocumentSymbolParams{TextDocument: protocol.TextDocumentIdentifier{URI: s.DocumentURI}}); err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if _, err := adapter.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "sample"}); err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if err := adapter.DidClose(ctx, protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: s.DocumentURI}}); err != nil {
		t.Fatalf("DidClose: %v", err)
	}
}

func runPushBaselineSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	if s.Mode != lsptest.Push && s.Mode != lsptest.Hybrid {
		t.Skip("scenario does not advertise push")
	}
	adapter, server, ctx := newAdapter(t, s)
	got := make(chan protocol.PublishDiagnosticsParams, 1)
	unsubscribe := adapter.Subscribe(func(method string, raw json.RawMessage) {
		if method != protocol.MethodPublishDiagnostics {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if json.Unmarshal(raw, &p) == nil {
			got <- p
		}
	})
	defer unsubscribe()
	if err := server.PushDiagnostics(); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		assertDiagnostics(t, p.Diagnostics, s.AllDiagnostics())
	case <-ctx.Done():
		t.Fatal("push diagnostic not delivered")
	}
}

func runPullOnlySubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	if s.Mode != lsptest.Pull && s.Mode != lsptest.Hybrid {
		t.Skip("scenario does not advertise pull")
	}
	adapter, _, ctx := newAdapter(t, s)
	pull, ok := adapter.(pullClient)
	if !ok || !pull.SupportsPullDiagnostics() {
		t.Fatal("adapter did not expose advertised pull diagnostics")
	}
	if s.DiagnosticOptions != nil {
		opts, enabled := adapter.Capabilities().DiagnosticOptions()
		if !enabled || opts == nil || *opts != *s.DiagnosticOptions {
			t.Fatalf("DiagnosticOptions() = %#v, %v, want %#v, true", opts, enabled, s.DiagnosticOptions)
		}
	}
	report, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: protocol.TextDocumentIdentifier{URI: s.DocumentURI}})
	if err != nil {
		t.Fatal(err)
	}
	assertDiagnostics(t, report.Items, s.AllDiagnostics())
}

func runHybridSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	if s.Mode != lsptest.Hybrid {
		t.Skip("scenario is not hybrid")
	}
	adapter, server, ctx := newAdapter(t, s)
	got := make(chan protocol.PublishDiagnosticsParams, 1)
	unsubscribe := adapter.Subscribe(func(method string, raw json.RawMessage) {
		if method != protocol.MethodPublishDiagnostics {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if json.Unmarshal(raw, &p) == nil {
			got <- p
		}
	})
	defer unsubscribe()
	if err := server.PushDiagnostics(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal("hybrid connection did not deliver its push notification")
	}
	pull, ok := adapter.(pullClient)
	if !ok || !pull.SupportsPullDiagnostics() {
		t.Fatal("hybrid connection did not also expose pull diagnostics")
	}
	if _, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: protocol.TextDocumentIdentifier{URI: s.DocumentURI}}); err != nil {
		t.Fatalf("hybrid connection's pull call failed: %v", err)
	}
}

func runDynamicWatcherSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	_, server, ctx := newAdapter(t, s)
	if err := server.RegisterWatcher(ctx); err != nil {
		t.Fatal(err)
	}
}

// assertDiagnostics fails t when got does not carry exactly want's messages,
// in order. Shared by every subtest that compares a delivered/pulled
// diagnostic set against Scenario.AllDiagnostics().
func assertDiagnostics(t *testing.T, got, want []protocol.Diagnostic) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("diagnostics = %#v, want %d item(s)", got, len(want))
	}
	for i, d := range want {
		if got[i].Message != d.Message {
			t.Fatalf("diagnostics[%d] = %#v, want message %q", i, got[i], d.Message)
		}
	}
}
