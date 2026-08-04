package zig

import (
	"context"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Adapter implements lsp.Client for zls (the Zig language server).
//
// zls expects a rootUri pointing at the project root (typically the directory
// containing build.zig) and resolves the build graph from it. It may register
// file watchers dynamically via client/registerCapability, which the embedded
// base answers so DidChangeWatchedFiles events are filtered to the registered
// globs.
//
// The 23 lsp.Client methods come from base.Adapter, which labels each error
// "zls <label>: <cause>". This package overrides the ones that need the
// document open first (an unopened file resolves to nothing) and Definition,
// whose reply is a bare single Location; on top of those it declares the
// optional document-pull surface (SupportsPullDiagnostics + Diagnostic) —
// declared HERE, not on the base, because an exported base method is promoted
// into all nine adapters at once (see the base package doc).
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
	b := base.New(conn, "zls")
	return &Adapter{Adapter: b, open: base.NewOpenTracker(b, "zig")}
}

// DefaultInitParams returns InitializeParams suitable for zls.
// rootURI must be a file:// URI pointing to the Zig project root.
// The default sends no initializationOptions — zls then surfaces only its
// ast-check syntax diagnostics. Real compile/semantic diagnostics need
// build-on-save, which the user opts into via [lsp.zig] initialization_options
// (e.g. enable_build_on_save); the pool overlays that free-form table onto these
// params verbatim (see internal/cli/pool_adapters.go's defaultInitParamsFor), so
// the adapter itself stays option-free by default.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
	}
}

// ── Document lifecycle ───────────────────────────────────────────────────────

// DidChangeWatchedFiles notifies zls that one or more files changed on disk,
// first dropping any lazily-opened copy of a changed document so the next query
// reopens it with fresh content. The tracker sees the UNFILTERED changes:
// plumb's copy is stale whatever the server's watcher globs say. The base then
// filters and forwards, unaffected by this — params is a value.
func (a *Adapter) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	a.open.Refresh(ctx, params.Changes)
	return a.Adapter.DidChangeWatchedFiles(ctx, params)
}

// ── Diagnostics (pull) ───────────────────────────────────────────────────────

// SupportsPullDiagnostics reports whether zls advertised the
// textDocument/diagnostic pull model at initialize. In practice zls (validated
// on 0.16) does NOT advertise diagnosticProvider — the earlier "zls is pull-first"
// hypothesis was disproven: zls pushes publishDiagnostics once plumb advertises
// the publishDiagnostics client capability (see doc.go), so the diagnostics tool
// stays on the push path for Zig. The method exists so the pool can route pull
// uniformly across adapters, gated on the advertised capability, never a guess.
// Returns false before Initialize.
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

// DocumentSymbols returns all symbols in the document.
func (a *Adapter) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.DocumentSymbols(ctx, params)
}

// Definition returns the definition location(s) for the symbol at pos.
func (a *Adapter) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	// zls returns a bare single Location object (not an array) for definition;
	// the spec union is Location | Location[] | LocationLink[] | null, so decode
	// the union rather than assuming []Location.
	raw, err := base.CallRaw(ctx, a.Adapter, "definition", protocol.MethodDefinition, params)
	if err != nil {
		return nil, err
	}
	locs, err := protocol.DecodeLocations(raw)
	if err != nil {
		return nil, base.Wrap(a.Adapter, "definition", err)
	}
	return locs, nil
}

// References returns all references to the symbol at pos.
func (a *Adapter) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.References(ctx, params)
}

// Hover returns hover information at pos.
func (a *Adapter) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.Hover(ctx, params)
}

// ── Edits ────────────────────────────────────────────────────────────────────

// PrepareRename checks whether rename is valid at pos.
//
// Rename itself is NOT overridden: zls answers it without the document being
// open, unlike sourcekit-lsp.
func (a *Adapter) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.PrepareRename(ctx, params)
}

// ── Call hierarchy ───────────────────────────────────────────────────────────

// PrepareCallHierarchy resolves the call-hierarchy item at pos.
func (a *Adapter) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.PrepareCallHierarchy(ctx, params)
}

// ── Type hierarchy ───────────────────────────────────────────────────────────

// PrepareTypeHierarchy resolves the type-hierarchy item at pos.
func (a *Adapter) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.PrepareTypeHierarchy(ctx, params)
}
