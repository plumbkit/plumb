package kotlin

import (
	"context"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Adapter implements lsp.Client for JetBrains' kotlin-lsp.
//
// kotlin-lsp expects a rootUri pointing at the project root (a Gradle or Maven
// module) and resolves the classpath from the build files, so it needs a project
// that genuinely resolves — a bare directory of .kt files yields nothing. It may
// register file watchers dynamically via client/registerCapability, which the
// embedded base answers so DidChangeWatchedFiles events are filtered to the
// registered globs.
//
// The 23 lsp.Client methods come from base.Adapter, which labels each error
// "kotlin-lsp <label>: <cause>". This package overrides only what this server
// does differently, all of it measured against a real binary rather than assumed
// (see doc.go for the build and date):
//
//   - DocumentSymbols needs the document open first. Unopened, the server fails
//     it outright with "no stub serializer for kotlin.PACKAGE_DIRECTIVE" — an
//     internal error, not an empty result. It is the ONLY method that needs
//     this: definition, references, hover, prepareCallHierarchy and the pull
//     diagnostics all answer correctly for a document plumb never opened, so
//     the ensure-open set is deliberately a single method and must not be
//     widened to match another adapter's.
//   - The optional document-pull surface (SupportsPullDiagnostics + Diagnostic),
//     because this server does not push at all. Declared HERE, not on the base,
//     because an exported base method is promoted into all nine adapters at once
//     (see the base package doc).
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct {
	*base.Adapter

	// open is held as a NAMED field, never embedded: embedding it would promote
	// Ensure and Refresh into this adapter's exported surface, which plumb
	// resolves structurally (see the base package doc).
	open *base.OpenTracker
}

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	b := base.New(conn, "kotlin-lsp")
	return &Adapter{Adapter: b, open: base.NewOpenTracker(b, "kotlin")}
}

// DefaultInitParams returns InitializeParams suitable for kotlin-lsp. rootURI
// must be a file:// URI pointing to the project root. No initialization options
// are required for plumb's use.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
	}
}

// ── Document lifecycle ───────────────────────────────────────────────────────

// DidChangeWatchedFiles notifies kotlin-lsp that one or more files changed on
// disk, first dropping any lazily-opened copy of a changed document so the next
// query reopens it with fresh content. The tracker sees the UNFILTERED changes:
// plumb's copy is stale whatever the server's watcher globs say. The base then
// filters and forwards, unaffected by this — params is a value.
func (a *Adapter) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	a.open.Refresh(ctx, params.Changes)
	return a.Adapter.DidChangeWatchedFiles(ctx, params)
}

// ── Diagnostics (pull) ───────────────────────────────────────────────────────

// SupportsPullDiagnostics reports whether kotlin-lsp advertised the
// textDocument/diagnostic pull model at initialize. It does, and unlike every
// other plumb adapter it does NOT also push: measured against a real binary on a
// file with two genuine errors, zero publishDiagnostics notifications arrived in
// 75 s with the client capability advertised, while textDocument/diagnostic
// answered in under a second. Kotlin is therefore the first adapter that
// genuinely requires the pull path rather than merely tolerating it. Still gated
// on the advertised capability, never a guess. Returns false before Initialize.
func (a *Adapter) SupportsPullDiagnostics() bool {
	caps := a.Capabilities()
	return caps != nil && caps.PullDiagnosticsEnabled()
}

// Diagnostic requests diagnostics for a single document via the LSP 3.17 pull
// model (textDocument/diagnostic). Callers should gate this on
// SupportsPullDiagnostics; a server that only pushes returns an error here.
func (a *Adapter) Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	return base.CallPtr[protocol.DocumentDiagnosticReport](ctx, a.Adapter, "diagnostic", protocol.MethodDiagnostic, params)
}

// ── Queries ──────────────────────────────────────────────────────────────────

// DocumentSymbols returns all symbols in the document, opening it first if
// plumb has not already. This is the one method that needs it: the server
// answers an unopened document with an internal "no stub serializer" error
// rather than an empty result.
func (a *Adapter) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.DocumentSymbols(ctx, params)
}
