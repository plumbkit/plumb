package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// routingProxy implements lsp.LSPClient by dispatching each call to the gopls
// instance for the workspace containing the URI in the method's params.
//
// Methods without a natural URI argument (Initialize, Shutdown, WorkspaceSymbols,
// Subscribe, Capabilities) fall back to the connection's "primary" workspace —
// the first one resolved for the connection. This preserves the existing
// behaviour for workspace-wide queries while making URI-bearing tools
// multi-workspace aware: a single MCP connection can query and edit symbols
// in any number of projects without pre-declaring an "active" one.
//
// Pool acquisition is idempotent and fast (map lookup + mutex) for workspaces
// already started; new workspaces incur a one-time gopls startup cost.
//
// Concurrency: all methods are safe for concurrent use.
type routingProxy struct {
	pool *workspacePool

	mu          sync.RWMutex
	primaryRoot string
	primary     *clientProxy
}

func newRoutingProxy(pool *workspacePool) *routingProxy {
	return &routingProxy{
		pool:    pool,
		primary: &clientProxy{},
	}
}

// setPrimary records the connection's primary workspace. Idempotent — only
// the first call wins so the fallback target stays stable across the
// connection's lifetime.
func (r *routingProxy) setPrimary(root string, p *clientProxy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primaryRoot == "" {
		r.primaryRoot = root
		r.primary = p
	}
}

// primaryClient returns the primary workspace's adapter or an error.
func (r *routingProxy) primaryClient() (lsp.LSPClient, error) {
	r.mu.RLock()
	p := r.primary
	r.mu.RUnlock()
	if c := p.get(); c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("LSP server not yet ready")
}

// route returns the LSPClient responsible for the workspace containing uri.
// Falls back to the primary if uri is empty or workspace resolution fails.
func (r *routingProxy) route(ctx context.Context, uri string) (lsp.LSPClient, error) {
	if uri == "" {
		return r.primaryClient()
	}
	path := strings.TrimPrefix(uri, "file://")
	root, language, err := r.pool.Detect(filepath.Dir(path))
	if err != nil {
		return r.primaryClient()
	}

	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primary := r.primary
	r.mu.RUnlock()

	if root == primaryRoot {
		if c := primary.get(); c != nil {
			return c, nil
		}
	}
	e, err := r.pool.acquireLang(ctx, root, language)
	if err != nil {
		return nil, fmt.Errorf("acquiring %s for %s: %w", language, root, err)
	}
	if c := e.proxy.get(); c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("LSP server not yet ready for %s", root)
}

// ─── lsp.LSPClient implementation ─────────────────────────────────────────

// Workspace-wide / lifecycle methods stick to the primary.
func (r *routingProxy) Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error) {
	c, err := r.primaryClient()
	if err != nil {
		return nil, err
	}
	return c.Initialize(ctx, params)
}
func (r *routingProxy) Initialized(ctx context.Context) error {
	c, err := r.primaryClient()
	if err != nil {
		return err
	}
	return c.Initialized(ctx)
}
func (r *routingProxy) Shutdown(ctx context.Context) error {
	c, err := r.primaryClient()
	if err != nil {
		return err
	}
	return c.Shutdown(ctx)
}
func (r *routingProxy) Exit(ctx context.Context) error {
	c, err := r.primaryClient()
	if err != nil {
		return err
	}
	return c.Exit(ctx)
}
func (r *routingProxy) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	c, err := r.primaryClient()
	if err != nil {
		return nil, err
	}
	return c.WorkspaceSymbols(ctx, params)
}
func (r *routingProxy) Capabilities() *protocol.ServerCapabilities {
	c, err := r.primaryClient()
	if err != nil {
		return nil
	}
	return c.Capabilities()
}
func (r *routingProxy) Subscribe(handler func(string, json.RawMessage)) func() {
	c, err := r.primaryClient()
	if err != nil {
		return func() {}
	}
	return c.Subscribe(handler)
}

// URI-bearing document methods route by URI.
func (r *routingProxy) DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return err
	}
	return c.DidOpen(ctx, params)
}
func (r *routingProxy) DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return err
	}
	return c.DidChange(ctx, params)
}
func (r *routingProxy) DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return err
	}
	return c.DidClose(ctx, params)
}
func (r *routingProxy) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.DocumentSymbols(ctx, params)
}
func (r *routingProxy) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.Definition(ctx, params)
}
func (r *routingProxy) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.References(ctx, params)
}
func (r *routingProxy) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.Hover(ctx, params)
}
func (r *routingProxy) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.PrepareRename(ctx, params)
}
func (r *routingProxy) Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.Rename(ctx, params)
}
func (r *routingProxy) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.PrepareCallHierarchy(ctx, params)
}
func (r *routingProxy) IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.IncomingCalls(ctx, params)
}
func (r *routingProxy) OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.OutgoingCalls(ctx, params)
}
func (r *routingProxy) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.PrepareTypeHierarchy(ctx, params)
}
func (r *routingProxy) Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.Supertypes(ctx, params)
}
func (r *routingProxy) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.Subtypes(ctx, params)
}

var _ lsp.LSPClient = (*routingProxy)(nil)

// ─── routingInvProxy ─────────────────────────────────────────────────────

// routingInvProxy is a diagnosticsSource that dispatches Diagnostics(uri)
// to the invalidator of whichever workspace contains the URI. AllDiagnostics()
// returns the primary workspace's aggregate, since merging across unrelated
// projects would obscure provenance.
//
// Routing only inspects workspaces already acquired (pool.lookup). New
// workspaces are spun up by the routingProxy when a tool call lands on them;
// diagnostics for a never-touched workspace return empty rather than blocking
// to start gopls.
type routingInvProxy struct {
	pool *workspacePool

	mu          sync.RWMutex
	primaryRoot string
	primary     *cache.Invalidator
}

func newRoutingInvProxy(pool *workspacePool) *routingInvProxy {
	return &routingInvProxy{pool: pool}
}

func (r *routingInvProxy) setPrimary(root string, inv *cache.Invalidator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primaryRoot == "" {
		r.primaryRoot = root
		r.primary = inv
	}
}

func (r *routingInvProxy) Diagnostics(uri string) []protocol.Diagnostic {
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primary := r.primary
	r.mu.RUnlock()

	if uri == "" {
		if primary == nil {
			return nil
		}
		return primary.Diagnostics(uri)
	}
	path := strings.TrimPrefix(uri, "file://")
	root, _, err := r.pool.Detect(filepath.Dir(path))
	if err != nil || root == primaryRoot {
		if primary == nil {
			return nil
		}
		return primary.Diagnostics(uri)
	}
	if e := r.pool.lookup(root); e != nil {
		return e.inv.Diagnostics(uri)
	}
	return nil
}

func (r *routingInvProxy) AllDiagnostics() map[string][]protocol.Diagnostic {
	r.mu.RLock()
	p := r.primary
	r.mu.RUnlock()
	if p == nil {
		return nil
	}
	return p.AllDiagnostics()
}
