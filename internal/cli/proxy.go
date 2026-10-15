package cli

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// clientProxy delegates lsp.Client calls to the currently live adapter.
// The serve command updates the proxy each time the supervisor (re)starts the
// LSP process, so tools remain valid across crashes without being recreated.
//
// Concurrency: all methods are safe for concurrent use.
type clientProxy struct {
	mu  sync.RWMutex
	cur lsp.Client
	// lastUsed is the unix-nanosecond timestamp of the most recent tool call
	// routed through this proxy. It lives here (not on poolEntry) so the routing
	// hot path can record activity lock-free — route()/primaryClient() already
	// hold the proxy, avoiding a pool-mutex map lookup per tool call. The pool's
	// idle-hibernation janitor and LRU eviction read it.
	lastUsed atomic.Int64

	// inflight counts the calls running against this proxy's server right now:
	// a call lease is taken before the call starts (begin, or an unpinned
	// acquire under the pool lock) and returned when it ends. Hibernation and
	// reaping refuse an entry whose count is non-zero, so neither stops a server
	// in the middle of a call. Lock-free on the hot path; see begin for the
	// ordering that makes the check race-free.
	inflight atomic.Int64

	// held mirrors poolEntry.refs > 0 (pins and borrows), written under the pool
	// mutex wherever refs changes. A call ending on an entry nobody holds asks the
	// pool to schedule its idle teardown (onIdle); a held entry's teardown is
	// scheduled by its last release instead, so the hot path never takes the
	// pool mutex for it.
	held atomic.Bool

	// onIdle runs when the last call lease on an unheld entry is returned. Set
	// once by the pool before the entry is published and never changed; nil for
	// proxies built outside the pool (tests), where it is skipped.
	onIdle func()
}

// begin opens a call lease and returns the live client, or (nil, nil) when the
// server is not ready. The caller must call the returned func exactly once when
// its call ends (it is idempotent, so a defer plus an early call is safe).
//
// The order — count first, then read the client — is what makes the lease
// race-free against teardown, which does the mirror image: clear the client
// first, then read the count, and restore the client if the count is non-zero.
// Go's atomics and the proxy mutex are sequentially consistent, so a begin that
// sees a live client was counted before teardown looked, and a teardown that
// saw zero cleared the client before begin read it. Either the call keeps the
// server, or the call sees "not ready"; it never holds a server being stopped.
func (p *clientProxy) begin() (lsp.Client, func()) {
	p.inflight.Add(1)
	c := p.get()
	if c == nil {
		p.release() // no call made: return the count without touching the clock
		return nil, nil
	}
	p.touch()
	return c, p.endOnce()
}

// endOnce returns an idempotent func that returns one call lease.
func (p *clientProxy) endOnce() func() {
	var once sync.Once
	return func() { once.Do(p.end) }
}

// end returns one call lease. The idle clock is touched again at the end, so a
// long call counts as activity for its whole duration, not just its start.
func (p *clientProxy) end() {
	p.touch()
	p.release()
}

// release returns one call lease. The last one on an entry nobody holds asks
// the pool to arm its idle teardown — also on begin's not-ready path, whose
// count may have made a concurrent reap stand down.
func (p *clientProxy) release() {
	if p.inflight.Add(-1) == 0 && !p.held.Load() && p.onIdle != nil {
		p.onIdle()
	}
}

// touch records that a tool call was just routed through this proxy. Lock-free;
// called on the hot path. Deliberately NOT folded into get(), which also fires
// for teardown and capability probes that must not reset the idle clock.
func (p *clientProxy) touch() {
	p.lastUsed.Store(time.Now().UnixNano())
}

func (p *clientProxy) set(c lsp.Client) {
	p.mu.Lock()
	p.cur = c
	p.mu.Unlock()
}

func (p *clientProxy) get() lsp.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cur
}

// clear drops the live adapter so get() returns nil until the next set(). Used
// when an entry is hibernated: a concurrent route() then sees the server as
// not-ready and triggers a restart instead of calling into a dying connection.
func (p *clientProxy) clear() {
	p.mu.Lock()
	p.cur = nil
	p.mu.Unlock()
}

func (p *clientProxy) getOrErr() (lsp.Client, error) {
	c := p.get()
	if c == nil {
		// Never bare: route through warmingErr so the hibernation-race path
		// carries the same cold-LSP guidance as the routing proxy.
		return nil, warmingErr(0, "")
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

func (p *clientProxy) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	c, err := p.getOrErr()
	if err != nil {
		return err
	}
	return c.DidChangeWatchedFiles(ctx, params)
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
var _ lsp.Client = (*clientProxy)(nil)
