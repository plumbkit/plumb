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

type Factory func(jsonrpc.Caller) lsp.Client

type pullClient interface {
	SupportsPullDiagnostics() bool
	Diagnostic(context.Context, protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

// RunConformance runs independent subtests so one failure never suppresses the
// remainder of the adapter contract. Its branching mirrors the scenario
// matrix; keeping it together makes the public test helper readable.
//
//nolint:gocyclo
func RunConformance(t *testing.T, factory Factory, s lsptest.Scenario) {
	t.Helper()
	newAdapter := func(t *testing.T) (lsp.Client, *lsptest.Caller, context.Context) {
		t.Helper()
		server := lsptest.NewCaller(s)
		adapter := factory(server)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		if _, err := adapter.Initialize(ctx, protocol.InitializeParams{RootURI: s.RootURI, Capabilities: protocol.DefaultClientCapabilities()}); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		if err := adapter.Initialized(ctx); err != nil {
			t.Fatalf("Initialized: %v", err)
		}
		return adapter, server, ctx
	}

	t.Run("lifecycle", func(t *testing.T) {
		adapter, _, ctx := newAdapter(t)
		if err := adapter.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if err := adapter.Exit(ctx); err != nil {
			t.Fatalf("Exit: %v", err)
		}
	})

	t.Run("document lifecycle and queries", func(t *testing.T) {
		adapter, _, ctx := newAdapter(t)
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
	})

	t.Run("diagnostics", func(t *testing.T) {
		adapter, server, ctx := newAdapter(t)
		switch s.Mode {
		case lsptest.Push:
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
				if len(p.Diagnostics) != 1 || p.Diagnostics[0].Message != s.Diagnostic.Message {
					t.Fatalf("diagnostics = %#v", p.Diagnostics)
				}
			case <-ctx.Done():
				t.Fatal("push diagnostic not delivered")
			}
		case lsptest.Pull:
			pull, ok := adapter.(pullClient)
			if !ok || !pull.SupportsPullDiagnostics() {
				t.Fatal("adapter did not expose advertised pull diagnostics")
			}
			report, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: protocol.TextDocumentIdentifier{URI: s.DocumentURI}})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Items) != 1 || report.Items[0].Message != s.Diagnostic.Message {
				t.Fatalf("report = %#v", report)
			}
		}
	})

	if s.RegisterWatch {
		t.Run("dynamic watcher registration", func(t *testing.T) {
			_, server, ctx := newAdapter(t)
			if err := server.RegisterWatcher(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
