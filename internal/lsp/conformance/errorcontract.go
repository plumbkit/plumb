package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// errTransport is the sentinel failure every fake transport in this file
// returns. Asserting against a sentinel (rather than an anonymous error) lets
// each assertion prove BOTH halves of the contract at once: the rendered
// message carries the adapter's label, and errors.Is still reaches the cause,
// so the %w wrapping cannot silently degrade to %v.
var errTransport = errors.New("conformance: transport unavailable")

// failingCaller is a jsonrpc.Caller whose two directions fail independently.
// jsonrpc.MockCaller cannot serve this purpose: its Notify never returns an
// error, so the notification labels would be unreachable.
//
// Concurrency: the harness drives one adapter from a single goroutine; only
// the request handler is guarded, because adapters install it from New.
type failingCaller struct {
	failCall   bool
	failNotify bool

	mu        sync.Mutex
	onRequest jsonrpc.RequestHandler
}

func (c *failingCaller) Call(_ context.Context, _ string, _, _ any) error {
	if c.failCall {
		return errTransport
	}
	return nil
}

func (c *failingCaller) Notify(_ context.Context, _ string, _ any) error {
	if c.failNotify {
		return errTransport
	}
	return nil
}

func (c *failingCaller) SetNotificationHandler(_ func(string, json.RawMessage)) {}

func (c *failingCaller) SetRequestHandler(fn jsonrpc.RequestHandler) {
	c.mu.Lock()
	c.onRequest = fn
	c.mu.Unlock()
}

func (c *failingCaller) Close() error { return nil }

// registerWatchGlob drives a client/registerCapability request through the
// adapter's OWN server-request handler — the same path a real server takes —
// so the adapter's watcher filter ends up holding glob. Without a registered
// glob the filter passes every event through unchanged, which is a different
// branch of DidChangeWatchedFiles than the one this pins.
func (c *failingCaller) registerWatchGlob(ctx context.Context, id, glob string) error {
	c.mu.Lock()
	fn := c.onRequest
	c.mu.Unlock()
	if fn == nil {
		return errors.New("conformance: adapter installed no server-request handler")
	}
	params := json.RawMessage(fmt.Sprintf(
		`{"registrations":[{"id":%q,"method":%q,"registerOptions":{"watchers":[{"globPattern":%q}]}}]}`,
		id, protocol.MethodDidChangeWatchedFiles, glob))
	if _, err := fn(ctx, protocol.MethodRegisterCapability, params); err != nil {
		return fmt.Errorf("conformance: registering watcher glob %q: %w", glob, err)
	}
	return nil
}

// labelledCall pairs one adapter method with the error label it must emit when
// the transport underneath it fails.
type labelledCall struct {
	label string
	call  func() error
}

// RunErrorContract drives every lsp.Client method against a transport that
// always fails and asserts the adapter wrapped the failure as
// "<server> <label>: <cause>". These strings are the ones plumb surfaces to
// agents; nothing else in the tree pins them, so a refactor that hoists the
// wrapping into a shared base package could otherwise reword every diagnostic
// plumb emits without turning a single test red.
//
// docURI MUST point at a file that exists on disk: the sourcekit-lsp, zls and
// vscode-html-language-server adapters lazily os.ReadFile the document and
// send didOpen before their per-document queries, so a missing file pins their
// "open <uri>" label instead of the request label under test.
//
// Two passes are needed because notifications and requests fail through
// different halves of jsonrpc.Caller and an adapter that only ever sends
// notifications on one path would otherwise go unpinned.
func RunErrorContract(t *testing.T, factory Factory, initParams InitParamsFactory, server, docURI string) {
	t.Helper()
	t.Run("notifications", func(t *testing.T) { runNotifyErrorContract(t, factory, server, docURI) })
	t.Run("requests", func(t *testing.T) { runCallErrorContract(t, factory, initParams, server, docURI) })
}

// assertWrapped fails t unless err renders exactly as "<server> <label>: <cause>"
// and still unwraps to the sentinel.
func assertWrapped(t *testing.T, err error, server, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %q", server+" "+label+": "+errTransport.Error())
	}
	if want := server + " " + label + ": " + errTransport.Error(); err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, errTransport) {
		t.Fatalf("error %q does not unwrap to the transport cause — the %%w wrapping was lost", err)
	}
}

// runLabelledCalls runs each case as its own subtest so one changed label does
// not mask the rest of the set.
func runLabelledCalls(t *testing.T, cases []labelledCall, server string) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) { assertWrapped(t, tc.call(), server, tc.label) })
	}
}

// runNotifyErrorContract pins the labels an adapter attaches to a failed
// outbound notification.
func runNotifyErrorContract(t *testing.T, factory Factory, server, docURI string) {
	t.Helper()
	conn := &failingCaller{failNotify: true}
	adapter := factory(conn)
	ctx := context.Background()

	runLabelledCalls(t, []labelledCall{
		{"initialized", func() error { return adapter.Initialized(ctx) }},
		{"exit", func() error { return adapter.Exit(ctx) }},
		{"didOpen", func() error {
			return adapter.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
				TextDocument: protocol.TextDocumentItem{URI: docURI, Version: 1},
			})
		}},
		{"didChange", func() error {
			return adapter.DidChange(ctx, protocol.DidChangeTextDocumentParams{
				TextDocument: protocol.VersionedTextDocumentIdentifier{URI: docURI, Version: 2},
			})
		}},
		{"didClose", func() error {
			return adapter.DidClose(ctx, protocol.DidCloseTextDocumentParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
			})
		}},
	}, server)

	t.Run("didChangeWatchedFiles", func(t *testing.T) {
		runWatchedFilesErrorContract(t, adapter, conn, server, docURI)
	})
}

// runWatchedFilesErrorContract pins both branches of DidChangeWatchedFiles.
// The method filters its events through the adapter's watcher registrations
// and returns nil early when nothing survives, so the label is observable only
// once a glob that MATCHES the document has been registered. Pinning the
// short-circuit alongside it keeps a future refactor from "fixing" the silent
// nil into an error, which would make plumb noisy on every unwatched write.
func runWatchedFilesErrorContract(t *testing.T, adapter lsp.Client, conn *failingCaller, server, docURI string) {
	t.Helper()
	ctx := context.Background()
	params := protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: docURI, Type: protocol.FileChanged}},
	}
	if err := conn.registerWatchGlob(ctx, "contract-nomatch", "**/*.plumb-no-such-suffix"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DidChangeWatchedFiles(ctx, params); err != nil {
		t.Fatalf("DidChangeWatchedFiles with only a non-matching glob = %v, want nil (filtered out before the transport)", err)
	}
	if err := conn.registerWatchGlob(ctx, "contract-match", "**/*"); err != nil {
		t.Fatal(err)
	}
	assertWrapped(t, adapter.DidChangeWatchedFiles(ctx, params), server, "didChangeWatchedFiles")
}

// runCallErrorContract pins the labels an adapter attaches to a failed
// outbound request. Notify succeeds here so the lazy-open adapters reach the
// request under test rather than failing in their didOpen.
func runCallErrorContract(t *testing.T, factory Factory, initParams InitParamsFactory, server, docURI string) {
	t.Helper()
	conn := &failingCaller{failCall: true}
	adapter := factory(conn)
	ctx := context.Background()

	cases := lifecycleErrorCases(ctx, adapter, initParams, docURI)
	cases = append(cases, queryErrorCases(ctx, adapter, docURI)...)
	cases = append(cases, hierarchyErrorCases(ctx, adapter, docURI)...)
	cases = append(cases, pullErrorCases(ctx, adapter, docURI)...)
	runLabelledCalls(t, cases, server)
}

func lifecycleErrorCases(ctx context.Context, adapter lsp.Client, initParams InitParamsFactory, docURI string) []labelledCall {
	return []labelledCall{
		{"initialize", func() error {
			_, err := adapter.Initialize(ctx, initParams(rootURIFor(docURI)))
			return err
		}},
		{"shutdown", func() error { return adapter.Shutdown(ctx) }},
	}
}

func queryErrorCases(ctx context.Context, adapter lsp.Client, docURI string) []labelledCall {
	doc := protocol.TextDocumentIdentifier{URI: docURI}
	return []labelledCall{
		{"documentSymbol", func() error {
			_, err := adapter.DocumentSymbols(ctx, protocol.DocumentSymbolParams{TextDocument: doc})
			return err
		}},
		{"workspaceSymbol", func() error {
			_, err := adapter.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "sample"})
			return err
		}},
		{"definition", func() error {
			_, err := adapter.Definition(ctx, protocol.DefinitionParams{TextDocument: doc})
			return err
		}},
		{"references", func() error {
			_, err := adapter.References(ctx, protocol.ReferenceParams{TextDocument: doc})
			return err
		}},
		{"hover", func() error {
			_, err := adapter.Hover(ctx, protocol.HoverParams{TextDocument: doc})
			return err
		}},
		{"prepareRename", func() error {
			_, err := adapter.PrepareRename(ctx, protocol.PrepareRenameParams{TextDocument: doc})
			return err
		}},
		{"rename", func() error {
			_, err := adapter.Rename(ctx, protocol.RenameParams{TextDocument: doc, NewName: "renamed"})
			return err
		}},
	}
}

// hierarchyErrorCases needs docURI on the two prepare* params: the lazy-open
// adapters ensureOpen the document there, and an empty URI would pin their
// "open <uri>" label instead of the request label.
func hierarchyErrorCases(ctx context.Context, adapter lsp.Client, docURI string) []labelledCall {
	doc := protocol.TextDocumentIdentifier{URI: docURI}
	return []labelledCall{
		{"prepareCallHierarchy", func() error {
			_, err := adapter.PrepareCallHierarchy(ctx, protocol.PrepareCallHierarchyParams{TextDocument: doc})
			return err
		}},
		{"callHierarchy/incomingCalls", func() error {
			_, err := adapter.IncomingCalls(ctx, protocol.CallHierarchyIncomingCallsParams{})
			return err
		}},
		{"callHierarchy/outgoingCalls", func() error {
			_, err := adapter.OutgoingCalls(ctx, protocol.CallHierarchyOutgoingCallsParams{})
			return err
		}},
		{"prepareTypeHierarchy", func() error {
			_, err := adapter.PrepareTypeHierarchy(ctx, protocol.PrepareTypeHierarchyParams{TextDocument: doc})
			return err
		}},
		{"typeHierarchy/supertypes", func() error {
			_, err := adapter.Supertypes(ctx, protocol.TypeHierarchySupertypesParams{})
			return err
		}},
		{"typeHierarchy/subtypes", func() error {
			_, err := adapter.Subtypes(ctx, protocol.TypeHierarchySubtypesParams{})
			return err
		}},
	}
}

// pullErrorCases adds the diagnostics labels an adapter only has when it
// implements the optional pull surfaces — branching on the same structural
// interfaces RunConformance uses, so an adapter that gains or loses pull is
// covered without editing this file.
func pullErrorCases(ctx context.Context, adapter lsp.Client, docURI string) []labelledCall {
	var out []labelledCall
	if pull, ok := adapter.(pullClient); ok {
		out = append(out, labelledCall{"diagnostic", func() error {
			_, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
			})
			return err
		}})
	}
	if wp, ok := adapter.(workspacePullClient); ok {
		// gopls spells this label with a SPACE — "gopls workspace diagnostic" —
		// where every other label is the camelCase LSP method name. The
		// inconsistency is pinned as-is: this harness records what plumb emits
		// today so the base-package refactor is provably a no-op, and changing
		// the wording is a separate, deliberate decision.
		out = append(out, labelledCall{"workspace diagnostic", func() error {
			_, err := wp.WorkspaceDiagnostic(ctx, protocol.WorkspaceDiagnosticParams{})
			return err
		}})
	}
	return out
}

// rootURIFor returns docURI's parent directory URI. Every adapter's
// DefaultInitParams stores rootURI verbatim and initialize fails at the
// transport in this pass, so the value only has to look like a workspace root.
func rootURIFor(docURI string) string {
	if i := strings.LastIndexByte(docURI, '/'); i > 0 {
		return docURI[:i]
	}
	return docURI
}
