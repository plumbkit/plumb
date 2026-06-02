package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// stubClient is a minimal lsp.Client that records which URIs it was called
// with so tests can verify routing dispatched to the expected adapter.
type stubClient struct {
	mu          sync.Mutex
	id          string
	definitions []string // captured URIs from Definition calls
}

func (s *stubClient) Initialize(context.Context, protocol.InitializeParams) (*protocol.InitializeResult, error) {
	return &protocol.InitializeResult{}, nil
}
func (s *stubClient) Initialized(context.Context) error { return nil }
func (s *stubClient) Shutdown(context.Context) error    { return nil }
func (s *stubClient) Exit(context.Context) error        { return nil }
func (s *stubClient) DidOpen(context.Context, protocol.DidOpenTextDocumentParams) error {
	return nil
}

func (s *stubClient) DidChange(context.Context, protocol.DidChangeTextDocumentParams) error {
	return nil
}

func (s *stubClient) DidClose(context.Context, protocol.DidCloseTextDocumentParams) error {
	return nil
}

func (s *stubClient) DidChangeWatchedFiles(context.Context, protocol.DidChangeWatchedFilesParams) error {
	return nil
}

func (s *stubClient) DocumentSymbols(context.Context, protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	return nil, nil
}

func (s *stubClient) WorkspaceSymbols(context.Context, protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, nil
}

func (s *stubClient) Definition(_ context.Context, p protocol.DefinitionParams) ([]protocol.Location, error) {
	s.mu.Lock()
	s.definitions = append(s.definitions, p.TextDocument.URI)
	s.mu.Unlock()
	return nil, nil
}

func (s *stubClient) References(context.Context, protocol.ReferenceParams) ([]protocol.Location, error) {
	return nil, nil
}

func (s *stubClient) Hover(context.Context, protocol.HoverParams) (*protocol.Hover, error) {
	return nil, nil
}

func (s *stubClient) PrepareRename(context.Context, protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	return nil, nil
}

func (s *stubClient) Rename(context.Context, protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, nil
}

func (s *stubClient) PrepareCallHierarchy(context.Context, protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	return nil, nil
}

func (s *stubClient) IncomingCalls(context.Context, protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return nil, nil
}

func (s *stubClient) OutgoingCalls(context.Context, protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return nil, nil
}

func (s *stubClient) PrepareTypeHierarchy(context.Context, protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, nil
}

func (s *stubClient) Supertypes(context.Context, protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, nil
}

func (s *stubClient) Subtypes(context.Context, protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	return nil, nil
}

func (s *stubClient) Capabilities() *protocol.ServerCapabilities { return nil }

func (s *stubClient) Subscribe(func(string, json.RawMessage)) func() { return func() {} }

var _ lsp.Client = (*stubClient)(nil)

// setupTwoProjects creates two go.mod-rooted project directories under a
// shared tempdir. Returns (rootA, rootB).
func setupTwoProjects(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	a := filepath.Join(base, "projA")
	b := filepath.Join(base, "projB")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "go.mod"), []byte("module test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// newTestPool constructs a workspacePool with Go enabled so Detect() can
// locate go.mod-rooted projects in tests.
func newTestPool() *workspacePool {
	return &workspacePool{
		entries: make(map[string]*poolEntry),
		baseCtx: context.Background(),
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{
				RootMarkers: []string{"go.mod"},
				Enabled:     true,
			}},
		},
	}
}

// installEntry mounts a stubClient into the pool as if it had been acquired
// via the normal flow, so routing can dispatch to it.
func installEntry(pool *workspacePool, root string, client lsp.Client) {
	cp := &clientProxy{}
	cp.set(client)
	pool.entries[root] = &poolEntry{root: root, language: "go", proxy: cp}
}

func TestRoutingProxy_RoutesByURI(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)

	pool := newTestPool()
	clientA := &stubClient{id: "A"}
	clientB := &stubClient{id: "B"}
	installEntry(pool, rootA, clientA)
	installEntry(pool, rootB, clientB)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, pool.entries[rootA].proxy)

	// Call against a file in project A → should land on clientA.
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(rootA, "main.go")},
	})
	// Call against a file in project B → should land on clientB.
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(rootB, "main.go")},
	})

	if len(clientA.definitions) != 1 {
		t.Errorf("clientA: expected 1 Definition, got %d", len(clientA.definitions))
	}
	if len(clientB.definitions) != 1 {
		t.Errorf("clientB: expected 1 Definition, got %d", len(clientB.definitions))
	}
}

func TestRoutingProxy_WorkspaceSymbolsUsesPrimary(t *testing.T) {
	rootA, _ := setupTwoProjects(t)

	pool := newTestPool()
	clientA := &stubClient{id: "A"}
	installEntry(pool, rootA, clientA)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, pool.entries[rootA].proxy)

	_, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Foo"})
	if err != nil {
		t.Errorf("WorkspaceSymbols on primary should succeed, got %v", err)
	}
}

func TestRoutingProxy_NoPrimaryReturnsNotReady(t *testing.T) {
	pool := newTestPool()
	rp := newRoutingProxy(pool)

	_, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{})
	if err == nil {
		t.Error("expected error when no primary is set")
	}
}

// TestRoutingProxy_ResetPrimaryOverridesFirstWins guards the re-pin path:
// setPrimary is first-wins (stable for the connection's lifetime), but a
// deliberate workspace switch must be able to repoint the primary. Without an
// overriding reset, LSP routing would stay welded to the original project even
// after session_start re-pins.
func TestRoutingProxy_ResetPrimaryOverridesFirstWins(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	clientA := &stubClient{id: "A"}
	clientB := &stubClient{id: "B"}
	installEntry(pool, rootA, clientA)
	installEntry(pool, rootB, clientB)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, pool.entries[rootA].proxy)
	// setPrimary is first-wins: a second setPrimary must not change the primary.
	rp.setPrimary(rootB, pool.entries[rootB].proxy)
	if rp.primaryRoot != rootA {
		t.Fatalf("setPrimary should be first-wins: got %s, want %s", rp.primaryRoot, rootA)
	}
	// resetPrimary IS allowed to switch (deliberate re-pin).
	rp.resetPrimary(rootB, pool.entries[rootB].proxy)
	if rp.primaryRoot != rootB {
		t.Fatalf("resetPrimary should override first-wins: got %s, want %s", rp.primaryRoot, rootB)
	}
	c, err := rp.primaryClient()
	if err != nil {
		t.Fatalf("primaryClient after reset: %v", err)
	}
	if c != clientB {
		t.Fatalf("primaryClient should be clientB after reset")
	}
}

// TestRoutingInvProxy_ResetPrimaryOverridesFirstWins mirrors the LSP-proxy test
// for the diagnostic-invalidator proxy.
func TestRoutingInvProxy_ResetPrimaryOverridesFirstWins(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	ri := newRoutingInvProxy(pool)
	ri.setPrimary(rootA, nil)
	ri.setPrimary(rootB, nil) // first-wins: ignored
	if ri.primaryRoot != rootA {
		t.Fatalf("setPrimary should be first-wins: got %s, want %s", ri.primaryRoot, rootA)
	}
	ri.resetPrimary(rootB, nil)
	if ri.primaryRoot != rootB {
		t.Fatalf("resetPrimary should override first-wins: got %s, want %s", ri.primaryRoot, rootB)
	}
}
