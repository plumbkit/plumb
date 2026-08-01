package conformance

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/adapters/html"
	"github.com/plumbkit/plumb/internal/lsp/adapters/jdtls"
	"github.com/plumbkit/plumb/internal/lsp/adapters/kotlin"
	"github.com/plumbkit/plumb/internal/lsp/adapters/pyright"
	"github.com/plumbkit/plumb/internal/lsp/adapters/rust"
	"github.com/plumbkit/plumb/internal/lsp/adapters/swift"
	"github.com/plumbkit/plumb/internal/lsp/adapters/typescript"
	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// This file is an INTERNAL test (package conformance, not conformance_test) so
// it can assert against the real pullClient and workspacePullClient declared in
// conformance.go. Asserting against local copies would let the copies drift
// from the shapes actually resolved at runtime, and a drifted copy fails open.
//
// internal/cli keeps its own copies of both shapes (routing_proxy_pull.go's
// pullCapableClient / workspacePullCapableClient). Those cannot be reached from
// here — internal/cli sits above internal/lsp in the layer stack — so this test
// pins two of the three structural call sites, not all three.

// TestAdapters_OptionalInterfaceSurface pins which adapters expose which
// optional capability, in both directions.
//
// Adapter capabilities are resolved STRUCTURALLY — by type assertion, never by
// declaration — at three sites: internal/cli/pool_adapters.go (asserts
// lsp.PullInitializer to call EnablePullDiagnostics, else applies the generic
// capability swap), internal/cli/routing_proxy_pull.go (routes diagnostics via
// pullCapableClient / workspacePullCapableClient), and
// internal/lsp/conformance/conformance.go (the same two shapes). Nothing in an
// adapter declares its participation, so a capability can be switched on with
// no visible diff at any call site.
//
// The negative assertions are the point. Should the adapters ever share an
// embedded base type, every exported method on that base is promoted into all
// nine embedders at once: one stray SupportsPullDiagnostics or
// WorkspaceDiagnostic there would silently opt six language servers into pull
// diagnostics — servers that answer -32601 to textDocument/diagnostic. That
// regression compiles, changes no call site, and would otherwise only surface
// as broken diagnostics at runtime. Here it fails a test.
//
// Update this table only alongside a deliberate, reviewed capability change.
func TestAdapters_OptionalInterfaceSurface(t *testing.T) {
	cases := []struct {
		name string
		// newAdapter builds the adapter through its own package's New, so the
		// assertions below run against the exact type the pool constructs.
		newAdapter func(jsonrpc.Caller) lsp.Client
		// pullInitializer: implements lsp.PullInitializer (EnablePullDiagnostics).
		pullInitializer bool
		// documentPull: SupportsPullDiagnostics + Diagnostic.
		documentPull bool
		// workspacePull: WorkspaceDiagnostic.
		workspacePull bool
	}{
		{
			name:            "gopls",
			newAdapter:      func(c jsonrpc.Caller) lsp.Client { return gopls.New(c) },
			pullInitializer: true,
			documentPull:    true,
			workspacePull:   true,
		},
		{
			name:         "zig",
			newAdapter:   func(c jsonrpc.Caller) lsp.Client { return zig.New(c) },
			documentPull: true,
		},
		{
			name:         "typescript",
			newAdapter:   func(c jsonrpc.Caller) lsp.Client { return typescript.New(c) },
			documentPull: true,
		},
		{name: "pyright", newAdapter: func(c jsonrpc.Caller) lsp.Client { return pyright.New(c) }},
		{name: "jdtls", newAdapter: func(c jsonrpc.Caller) lsp.Client { return jdtls.New(c) }},
		{name: "rust", newAdapter: func(c jsonrpc.Caller) lsp.Client { return rust.New(c) }},
		{name: "swift", newAdapter: func(c jsonrpc.Caller) lsp.Client { return swift.New(c) }},
		{name: "kotlin", newAdapter: func(c jsonrpc.Caller) lsp.Client { return kotlin.New(c) }},
		{name: "html", newAdapter: func(c jsonrpc.Caller) lsp.Client { return html.New(c) }},
	}

	// A new adapter package must be added to the table above, with its
	// capabilities stated explicitly, rather than inheriting them silently.
	if want := 9; len(cases) != want {
		t.Fatalf("table covers %d adapters, want %d — add the new adapter and state its capabilities", len(cases), want)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := tc.newAdapter(jsonrpc.NewMockCaller())

			check := func(iface string, got, want bool) {
				t.Helper()
				switch {
				case want && !got:
					t.Errorf("%s no longer implements %s; a method signature has drifted, "+
						"or the capability was removed", tc.name, iface)
				case !want && got:
					t.Errorf("%s now implements %s but must not; a promoted or newly added "+
						"method has silently switched this capability on", tc.name, iface)
				}
			}

			_, isPullInit := adapter.(lsp.PullInitializer)
			check("lsp.PullInitializer", isPullInit, tc.pullInitializer)

			_, isDocPull := adapter.(pullClient)
			check("the document-pull surface (SupportsPullDiagnostics + Diagnostic)", isDocPull, tc.documentPull)

			_, isWorkspacePull := adapter.(workspacePullClient)
			check("the workspace-pull surface (WorkspaceDiagnostic)", isWorkspacePull, tc.workspacePull)

			// Workspace pull without document pull would be an incoherent
			// surface: the routing proxy reaches workspace/diagnostic only on
			// a connection already negotiated onto the pull path.
			if tc.workspacePull && !tc.documentPull {
				t.Fatalf("%s: table is inconsistent — workspace pull requires document pull", tc.name)
			}
		})
	}
}
