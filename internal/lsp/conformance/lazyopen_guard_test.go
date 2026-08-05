package conformance

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/html"
	"github.com/plumbkit/plumb/internal/lsp/adapters/swift"
	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// This file guards the NEGATIVE half of the lazy-open design: what the three
// lazy-open adapters must NOT do. The positive half — that an ensure-open
// works, caches, retries and refreshes — is covered by base's opentracker_test
// and by each adapter's own suite; nothing there fails when an adapter grows an
// ensure-open it never needed, hands its tracker the wrong languageId, or
// embeds the tracker instead of holding it as a named field. Those are exactly
// the regressions that compile, read as tidying, and only show up as different
// traffic to a real language server.

// lazyOpenAdapter is one of the three adapters that open a document lazily
// (sourcekit-lsp, zls, vscode-html-language-server).
type lazyOpenAdapter struct {
	name string
	// languageID is the LSP languageId its tracker must tag every didOpen with:
	// servers key their parser off it, and handing one adapter's tracker another
	// language's ID is a silent, compiling mistake.
	languageID string
	// newAdapter builds the adapter through its own package's New, so every
	// assertion runs against the exact type the pool constructs.
	newAdapter func(jsonrpc.Caller) lsp.Client
	// doc is a real document on disk: Ensure reads the file itself, so a
	// fictional URI would fail every ensure-open before it reached the transport
	// and turn each expected didOpen into a silent zero.
	doc protocol.TextDocumentIdentifier
	// extraExported are the exported methods this adapter declares beyond the 23
	// of lsp.Client. Only zig has any — its optional document-pull surface.
	extraExported []string
}

// lazyOpenAdapters builds the three adapters under test, each with its fixture
// document already on disk.
func lazyOpenAdapters(t *testing.T) []lazyOpenAdapter {
	t.Helper()
	root := t.TempDir()
	swiftDoc := filepath.Join(root, "Greeter.swift")
	zigDoc := filepath.Join(root, "main.zig")
	htmlDoc := filepath.Join(root, "index.html")
	WriteFixture(t, map[string]string{
		swiftDoc: "struct Greeter {}\n",
		zigDoc:   "pub fn main() void {}\n",
		htmlDoc:  "<p>hello</p>\n",
	})

	adapters := []lazyOpenAdapter{
		{
			name:       "swift",
			languageID: "swift",
			newAdapter: func(c jsonrpc.Caller) lsp.Client { return swift.New(c) },
			doc:        protocol.TextDocumentIdentifier{URI: paths.PathToURI(swiftDoc)},
		},
		{
			name:          "zig",
			languageID:    "zig",
			newAdapter:    func(c jsonrpc.Caller) lsp.Client { return zig.New(c) },
			doc:           protocol.TextDocumentIdentifier{URI: paths.PathToURI(zigDoc)},
			extraExported: []string{"SupportsPullDiagnostics", "Diagnostic"},
		},
		{
			name:       "html",
			languageID: "html",
			newAdapter: func(c jsonrpc.Caller) lsp.Client { return html.New(c) },
			doc:        protocol.TextDocumentIdentifier{URI: paths.PathToURI(htmlDoc)},
		},
	}

	// A fourth lazy-open adapter must be added here, with its ensure-open matrix
	// and languageId stated explicitly, rather than joining the family silently.
	if want := 3; len(adapters) != want {
		t.Fatalf("table covers %d lazy-open adapters, want %d — add the new adapter and state its matrix", len(adapters), want)
	}
	return adapters
}

// lazyOpenMethod is one lsp.Client method and the didOpen count it must produce
// on each lazy-open adapter.
type lazyOpenMethod struct {
	// method is the LSP request the call makes; it names the subtest and keys
	// the canned reply.
	method string
	// reply is what the fake server answers, so the request itself succeeds and
	// the only thing that can fail is the ensure-open under test.
	reply  any
	invoke func(context.Context, lsp.Client, protocol.TextDocumentIdentifier) error
	// opens maps adapter name → expected textDocument/didOpen count.
	opens map[string]int
}

// lazyOpenMethods is the matrix itself: one row per lsp.Client method that can
// reach a document, kept as a single table so the asymmetry is readable.
func lazyOpenMethods(t *testing.T) []lazyOpenMethod {
	t.Helper()
	var (
		all  = map[string]int{"swift": 1, "zig": 1, "html": 1}
		none = map[string]int{"swift": 0, "zig": 0, "html": 0}
		// prepares: the HTML server resolves a prepare against the document it
		// already holds; the other two need it open.
		prepares = map[string]int{"swift": 1, "zig": 1, "html": 0}
		// swiftOnly: sourcekit-lsp is the only one of the three that needs the
		// document open for a rename.
		swiftOnly = map[string]int{"swift": 1, "zig": 0, "html": 0}
	)
	methods := []lazyOpenMethod{
		{protocol.MethodDocumentSymbols, []any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.DocumentSymbols(ctx, protocol.DocumentSymbolParams{TextDocument: d})
			return err
		}, all},
		{protocol.MethodDefinition, []any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.Definition(ctx, protocol.DefinitionParams{TextDocument: d})
			return err
		}, all},
		{protocol.MethodReferences, []any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.References(ctx, protocol.ReferenceParams{TextDocument: d})
			return err
		}, all},
		{protocol.MethodHover, map[string]any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.Hover(ctx, protocol.HoverParams{TextDocument: d})
			return err
		}, all},
		{protocol.MethodPrepareRename, map[string]any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.PrepareRename(ctx, protocol.PrepareRenameParams{TextDocument: d})
			return err
		}, prepares},
		{protocol.MethodPrepareCallHierarchy, []any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.PrepareCallHierarchy(ctx, protocol.PrepareCallHierarchyParams{TextDocument: d})
			return err
		}, prepares},
		{protocol.MethodPrepareTypeHierarchy, []any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.PrepareTypeHierarchy(ctx, protocol.PrepareTypeHierarchyParams{TextDocument: d})
			return err
		}, prepares},
		{protocol.MethodRename, map[string]any{}, func(ctx context.Context, a lsp.Client, d protocol.TextDocumentIdentifier) error {
			_, err := a.Rename(ctx, protocol.RenameParams{TextDocument: d, NewName: "renamed"})
			return err
		}, swiftOnly},
		{protocol.MethodWorkspaceSymbols, []any{}, func(ctx context.Context, a lsp.Client, _ protocol.TextDocumentIdentifier) error {
			_, err := a.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "sample"})
			return err
		}, none},
		{protocol.MethodCallHierarchyIncoming, []any{}, func(ctx context.Context, a lsp.Client, _ protocol.TextDocumentIdentifier) error {
			_, err := a.IncomingCalls(ctx, protocol.CallHierarchyIncomingCallsParams{})
			return err
		}, none},
		{protocol.MethodCallHierarchyOutgoing, []any{}, func(ctx context.Context, a lsp.Client, _ protocol.TextDocumentIdentifier) error {
			_, err := a.OutgoingCalls(ctx, protocol.CallHierarchyOutgoingCallsParams{})
			return err
		}, none},
		{protocol.MethodTypeHierarchySuper, []any{}, func(ctx context.Context, a lsp.Client, _ protocol.TextDocumentIdentifier) error {
			_, err := a.Supertypes(ctx, protocol.TypeHierarchySupertypesParams{})
			return err
		}, none},
		{protocol.MethodTypeHierarchySub, []any{}, func(ctx context.Context, a lsp.Client, _ protocol.TextDocumentIdentifier) error {
			_, err := a.Subtypes(ctx, protocol.TypeHierarchySubtypesParams{})
			return err
		}, none},
	}

	// A new lsp.Client method must be added here with its own row, rather than
	// inheriting whatever the base does with it.
	if want := 13; len(methods) != want {
		t.Fatalf("table covers %d methods, want %d — add the new method and state its didOpen count per adapter", len(methods), want)
	}
	return methods
}

// TestLazyOpenAdapters_DidOpenMatrix pins how many textDocument/didOpen
// notifications each lsp.Client method produces on each lazy-open adapter, in
// BOTH directions: a 0 that becomes a 1 fails as loudly as a 1 that becomes a 0.
//
// The matrix is asymmetric on purpose, and since the base.Adapter migration the
// asymmetry is carried by method ABSENCE — an adapter that does not override a
// method inherits the base's plain forward, with no ensure-open. sourcekit-lsp
// needs the document open even for Rename (it answers -32001 "No language
// service for <uri> found" otherwise); zls answers a rename from its own index,
// so zig deliberately does not override it; the HTML server answers rename and
// both hierarchy prepares from the document it already holds, so html overrides
// neither. Making the three adapters "consistent" therefore looks like tidying
// and is not: it changes what plumb puts on the wire to three real servers —
// an extra file read and an extra didOpen ahead of those requests — under cover
// of a refactor. This table is the record of what each server actually needs.
func TestLazyOpenAdapters_DidOpenMatrix(t *testing.T) {
	for _, m := range lazyOpenMethods(t) {
		t.Run(m.method, func(t *testing.T) {
			for _, a := range lazyOpenAdapters(t) {
				t.Run(a.name, func(t *testing.T) {
					want, stated := m.opens[a.name]
					if !stated {
						t.Fatalf("%s has no stated didOpen count for %s", m.method, a.name)
					}
					// A fresh adapter per method: a tracker that has already
					// opened the document would report 0 for everything after
					// the first call.
					conn := jsonrpc.NewMockCaller()
					conn.HandleOK(m.method, m.reply)
					if err := m.invoke(context.Background(), a.newAdapter(conn), a.doc); err != nil {
						t.Fatalf("%s on the %s adapter = %v, want nil error", m.method, a.name, err)
					}
					assertDidOpenCount(t, conn, m.method, a.name, want)
				})
			}
		})
	}
}

// assertDidOpenCount fails t when conn saw a number of didOpen notifications
// other than want, wording each direction as the regression it would be.
func assertDidOpenCount(t *testing.T, conn *jsonrpc.MockCaller, method, adapter string, want int) {
	t.Helper()
	got := len(didOpenItems(t, conn))
	switch {
	case got > want:
		t.Errorf("%s on the %s adapter sent %d didOpen notification(s), want %d — this method must NOT "+
			"ensure-open on this server; an added override changes the wire traffic", method, adapter, got, want)
	case got < want:
		t.Errorf("%s on the %s adapter sent %d didOpen notification(s), want %d — this server answers "+
			"the request only for an open document; the ensure-open override was lost", method, adapter, got, want)
	}
}

// TestLazyOpenAdapters_LanguageIDAndExportedSurface pins the two facts about a
// tracker that only its owning adapter can get wrong.
//
// The languageId is per-tracker state the adapter passes to base.NewOpenTracker;
// base's own tests prove the tracker forwards whatever it is handed, which is
// precisely why nothing there notices swift handing it "zig". Servers key their
// parser off it, so a wrong one degrades a real session silently.
//
// The exported surface is the never-embed rule: an embedded *base.OpenTracker
// would promote Ensure and Refresh onto the adapter, and plumb resolves optional
// adapter capabilities structurally — a surface that grows by accident is how a
// capability gets switched on with no visible diff at any call site. base's
// TestExportedSurface_IsExactlyLSPClient pins the same rule for the base itself;
// this pins it for the three adapters that hold a tracker.
func TestLazyOpenAdapters_LanguageIDAndExportedSurface(t *testing.T) {
	for _, a := range lazyOpenAdapters(t) {
		t.Run(a.name, func(t *testing.T) {
			conn := jsonrpc.NewMockCaller()
			conn.HandleOK(protocol.MethodHover, map[string]any{})
			adapter := a.newAdapter(conn)
			if _, err := adapter.Hover(context.Background(), protocol.HoverParams{TextDocument: a.doc}); err != nil {
				t.Fatalf("Hover = %v, want nil error", err)
			}

			opened := didOpenItems(t, conn)
			if len(opened) != 1 {
				t.Fatalf("Hover sent %d didOpen notification(s), want exactly 1", len(opened))
			}
			if got := opened[0].LanguageID; got != a.languageID {
				t.Errorf("didOpen languageId = %q, want %q — the tracker was handed another adapter's language", got, a.languageID)
			}

			assertExportedSurface(t, adapter, a.extraExported)
		})
	}
}

// assertExportedSurface fails t when adapter exports anything beyond lsp.Client
// plus extra, in either direction.
func assertExportedSurface(t *testing.T, adapter lsp.Client, extra []string) {
	t.Helper()
	clientType := reflect.TypeOf((*lsp.Client)(nil)).Elem()
	want := make(map[string]bool, clientType.NumMethod()+len(extra))
	for i := range clientType.NumMethod() {
		want[clientType.Method(i).Name] = true
	}
	for _, name := range extra {
		if want[name] {
			t.Fatalf("%q is declared as an extra exported method but is already an lsp.Client method", name)
		}
		want[name] = true
	}

	adapterType := reflect.TypeOf(adapter)
	for i := range adapterType.NumMethod() {
		if name := adapterType.Method(i).Name; !want[name] {
			t.Errorf("%s exports %s, which is neither an lsp.Client method nor one of its declared optional "+
				"methods — an embedded *base.OpenTracker promotes Ensure and Refresh like this", adapterType, name)
		}
	}
	if got, wantN := adapterType.NumMethod(), len(want); got != wantN {
		t.Errorf("%s exports %d methods, want exactly %d (the %d of lsp.Client plus %v)",
			adapterType, got, wantN, clientType.NumMethod(), extra)
	}
}

// didOpenItems returns the text-document item of every didOpen conn recorded,
// in order.
func didOpenItems(t *testing.T, conn *jsonrpc.MockCaller) []protocol.TextDocumentItem {
	t.Helper()
	var out []protocol.TextDocumentItem
	for _, c := range conn.Calls() {
		if c.Method != protocol.MethodDidOpen {
			continue
		}
		var params protocol.DidOpenTextDocumentParams
		if err := json.Unmarshal(c.Params, &params); err != nil {
			t.Fatalf("decoding didOpen params %s: %v", c.Params, err)
		}
		out = append(out, params.TextDocument)
	}
	return out
}
