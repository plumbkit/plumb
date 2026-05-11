package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// clientProxy delegates lsp.LSPClient calls to the currently live adapter.
// The serve command updates the proxy each time the supervisor (re)starts the
// LSP process, so tools remain valid across crashes without being recreated.
//
// Concurrency: all methods are safe for concurrent use.
type clientProxy struct {
	mu  sync.RWMutex
	cur lsp.LSPClient
}

func (p *clientProxy) set(c lsp.LSPClient) {
	p.mu.Lock()
	p.cur = c
	p.mu.Unlock()
}

func (p *clientProxy) get() lsp.LSPClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cur
}

func (p *clientProxy) getOrErr() (lsp.LSPClient, error) {
	c := p.get()
	if c == nil {
		return nil, fmt.Errorf("LSP server not yet ready")
	}
	return c, nil
}

func (p *clientProxy) Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.Initialize(ctx, params)
}
func (p *clientProxy) Initialized(ctx context.Context) error {
	c, err := p.getOrErr()
	if err != nil {
		return err
	}
	return c.Initialized(ctx)
}
func (p *clientProxy) Shutdown(ctx context.Context) error {
	c, err := p.getOrErr()
	if err != nil {
		return err
	}
	return c.Shutdown(ctx)
}
func (p *clientProxy) Exit(ctx context.Context) error {
	c, err := p.getOrErr()
	if err != nil {
		return err
	}
	return c.Exit(ctx)
}
func (p *clientProxy) DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error {
	c, err := p.getOrErr()
	if err != nil {
		return err
	}
	return c.DidOpen(ctx, params)
}
func (p *clientProxy) DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error {
	c, err := p.getOrErr()
	if err != nil {
		return err
	}
	return c.DidChange(ctx, params)
}
func (p *clientProxy) DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error {
	c, err := p.getOrErr()
	if err != nil {
		return err
	}
	return c.DidClose(ctx, params)
}
func (p *clientProxy) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.DocumentSymbols(ctx, params)
}
func (p *clientProxy) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.WorkspaceSymbols(ctx, params)
}
func (p *clientProxy) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.Definition(ctx, params)
}
func (p *clientProxy) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.References(ctx, params)
}
func (p *clientProxy) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.Hover(ctx, params)
}
func (p *clientProxy) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.PrepareRename(ctx, params)
}
func (p *clientProxy) Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.Rename(ctx, params)
}
func (p *clientProxy) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.PrepareCallHierarchy(ctx, params)
}
func (p *clientProxy) IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.IncomingCalls(ctx, params)
}
func (p *clientProxy) OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.OutgoingCalls(ctx, params)
}
func (p *clientProxy) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.PrepareTypeHierarchy(ctx, params)
}
func (p *clientProxy) Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.Supertypes(ctx, params)
}
func (p *clientProxy) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := p.getOrErr()
	if err != nil {
		return nil, err
	}
	return c.Subtypes(ctx, params)
}
func (p *clientProxy) Capabilities() *protocol.ServerCapabilities {
	c := p.get()
	if c == nil {
		return nil
	}
	return c.Capabilities()
}
func (p *clientProxy) Subscribe(handler func(string, json.RawMessage)) func() {
	c := p.get()
	if c == nil {
		return func() {}
	}
	return c.Subscribe(handler)
}

// ensure clientProxy satisfies the interface at compile time.
var _ lsp.LSPClient = (*clientProxy)(nil)

// invProxy is a session-level indirection to a shared workspace Invalidator.
// It starts nil (no workspace determined yet) and is set once the workspace root
// is resolved, allowing tools registered before workspace discovery to work correctly.
//
// Concurrency: all methods are safe for concurrent use.
type invProxy struct {
	mu  sync.RWMutex
	cur *cache.Invalidator
}

func (p *invProxy) set(inv *cache.Invalidator) {
	p.mu.Lock()
	p.cur = inv
	p.mu.Unlock()
}

func (p *invProxy) Diagnostics(uri string) []protocol.Diagnostic {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cur == nil {
		return nil
	}
	return p.cur.Diagnostics(uri)
}

func (p *invProxy) AllDiagnostics() map[string][]protocol.Diagnostic {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cur == nil {
		return nil
	}
	return p.cur.AllDiagnostics()
}
