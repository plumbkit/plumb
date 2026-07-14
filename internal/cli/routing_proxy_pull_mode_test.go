package cli

// routing_proxy_pull_mode_test.go — the Task 5 routing surface: per-URI
// DiagnosticsMode, the -32601 downgrade, DiagnosticCapabilities, workspace
// pulls, the refresh server-request wrapper, and the routingInvProxy
// pull-state routing.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// wsPullStubClient extends pullStubClient with capabilities and the
// workspace-pull surface, so DiagnosticCapabilities and WorkspaceDiagnostic
// can be exercised without a real server.
type wsPullStubClient struct {
	pullStubClient
	caps     *protocol.ServerCapabilities
	wsReport *protocol.WorkspaceDiagnosticReport
	wsErr    error

	mu      sync.Mutex
	wsCalls []protocol.WorkspaceDiagnosticParams
}

func (w *wsPullStubClient) Capabilities() *protocol.ServerCapabilities { return w.caps }

func (w *wsPullStubClient) WorkspaceDiagnostic(_ context.Context, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	w.mu.Lock()
	w.wsCalls = append(w.wsCalls, params)
	w.mu.Unlock()
	return w.wsReport, w.wsErr
}

func setEntryDiagMode(pool *workspacePool, root, mode string) {
	pool.mu.Lock()
	pool.entries[poolKey{root, "go"}].diagMode = mode
	pool.mu.Unlock()
}

func TestRoutingProxy_DiagnosticsMode_RoutesPerURI(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})
	installEntry(pool, rootB, &stubClient{id: "B"})
	setEntryDiagMode(pool, rootA, diagModePull)
	setEntryDiagMode(pool, rootB, diagModePush)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	if got := rp.DiagnosticsMode("file://" + filepath.Join(rootA, "main.go")); got != diagModePull {
		t.Errorf("rootA file mode = %q, want pull", got)
	}
	if got := rp.DiagnosticsMode("file://" + filepath.Join(rootB, "main.go")); got != diagModePush {
		t.Errorf("rootB file mode = %q, want push", got)
	}
	// No URI: the primary's mode.
	if got := rp.DiagnosticsMode(""); got != diagModePull {
		t.Errorf("primary mode = %q, want pull", got)
	}
}

func TestRoutingProxy_DiagnosticsMode_NoEntry(t *testing.T) {
	pool := newTestPool()
	rp := newRoutingProxy(pool)
	if got := rp.DiagnosticsMode("file:///nowhere/main.go"); got != "" {
		t.Errorf("expected \"\" for an unpooled URI, got %q", got)
	}
}

func TestRoutingProxy_Diagnostic_MethodNotFound_Downgrades(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	client := &pullStubClient{
		supports: true,
		err:      fmt.Errorf("zls diagnostic: %w", &jsonrpc.MethodNotFoundError{Method: protocol.MethodDiagnostic}),
	}
	installEntry(pool, rootA, client)
	setEntryDiagMode(pool, rootA, diagModePull)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	uri := "file://" + filepath.Join(rootA, "main.go")
	_, err := rp.Diagnostic(context.Background(), protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err == nil {
		t.Fatal("expected the -32601 error to be returned")
	}
	if got := pool.diagModeFor(rootA, "go"); got != diagModePush {
		t.Errorf("entry mode after -32601 = %q, want push (downgraded)", got)
	}
	// Idempotent: a second failure keeps push, no flapping.
	_, _ = rp.Diagnostic(context.Background(), protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if got := pool.diagModeFor(rootA, "go"); got != diagModePush {
		t.Errorf("entry mode after repeat = %q, want push", got)
	}
}

func TestRoutingProxy_Diagnostic_OtherErrorKeepsMode(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	client := &pullStubClient{supports: true, err: fmt.Errorf("gopls diagnostic: connection reset")}
	installEntry(pool, rootA, client)
	setEntryDiagMode(pool, rootA, diagModePull)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	_, err := rp.Diagnostic(context.Background(), protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(rootA, "main.go")},
	})
	if err == nil {
		t.Fatal("expected the error to be returned")
	}
	if got := pool.diagModeFor(rootA, "go"); got != diagModePull {
		t.Errorf("a non--32601 error must not downgrade: mode = %q, want pull", got)
	}
}

func TestPool_DowngradeDiagMode_OnlyFlipsNegotiatedPull(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})
	for _, tc := range []struct{ from, want string }{
		{diagModePull, diagModePush},
		{diagModeHybrid, diagModePush},
		{diagModePush, diagModePush},
		{diagModePullUnavailable, diagModePullUnavailable},
		{"", ""},
	} {
		setEntryDiagMode(pool, rootA, tc.from)
		pool.downgradeDiagMode(rootA, "go")
		if got := pool.diagModeFor(rootA, "go"); got != tc.want {
			t.Errorf("downgrade from %q = %q, want %q", tc.from, got, tc.want)
		}
	}
}

func TestRoutingProxy_DiagnosticCapabilities(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	full := &wsPullStubClient{caps: &protocol.ServerCapabilities{
		DiagnosticProvider: &protocol.BoolOrOptions{
			Enabled: true,
			Raw:     json.RawMessage(`{"interFileDependencies":true,"workspaceDiagnostics":true}`),
		},
	}}
	installEntry(pool, rootA, full)
	installEntry(pool, rootB, &stubClient{id: "B"}) // nil caps

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	interFile, wsPull := rp.DiagnosticCapabilities("file://" + filepath.Join(rootA, "main.go"))
	if !interFile || !wsPull {
		t.Errorf("rootA capabilities = (%v, %v), want (true, true)", interFile, wsPull)
	}
	interFile, wsPull = rp.DiagnosticCapabilities("file://" + filepath.Join(rootB, "main.go"))
	if interFile || wsPull {
		t.Errorf("rootB (nil caps) = (%v, %v), want (false, false)", interFile, wsPull)
	}
	// No URI: the primary's capabilities.
	if _, wsPull = rp.DiagnosticCapabilities(""); !wsPull {
		t.Error("primary capabilities expected workspaceDiagnostics true")
	}
}

func TestRoutingProxy_WorkspaceDiagnostic_DelegatesAndDowngrades(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	client := &wsPullStubClient{
		wsReport: &protocol.WorkspaceDiagnosticReport{
			Items: []protocol.WorkspaceDocumentDiagnosticReport{{Kind: protocol.DiagnosticReportFull, URI: "file:///x.go"}},
		},
	}
	installEntry(pool, rootA, client)
	setEntryDiagMode(pool, rootA, diagModePull)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	rep, err := rp.WorkspaceDiagnostic(context.Background(), "", protocol.WorkspaceDiagnosticParams{
		PreviousResultIDs: []protocol.PreviousResultID{{URI: "file:///x.go", Value: "r1"}},
	})
	if err != nil || rep == nil || len(rep.Items) != 1 {
		t.Fatalf("WorkspaceDiagnostic = (%v, %v), want the stub report", rep, err)
	}
	client.mu.Lock()
	calls := len(client.wsCalls)
	prev := client.wsCalls[0].PreviousResultIDs
	client.mu.Unlock()
	if calls != 1 || len(prev) != 1 || prev[0].Value != "r1" {
		t.Errorf("expected one delegated call carrying the previous result IDs")
	}

	// A -32601 downgrades exactly like the document pull.
	client.wsErr = fmt.Errorf("gopls workspace diagnostic: %w", &jsonrpc.MethodNotFoundError{Method: protocol.MethodWorkspaceDiagnostic})
	client.wsReport = nil
	if _, err := rp.WorkspaceDiagnostic(context.Background(), "", protocol.WorkspaceDiagnosticParams{}); err == nil {
		t.Fatal("expected the error returned")
	}
	if got := pool.diagModeFor(rootA, "go"); got != diagModePush {
		t.Errorf("mode after workspace -32601 = %q, want push", got)
	}
}

func TestRoutingProxy_WorkspaceDiagnostic_UnsupportedAdapter(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	if _, err := rp.WorkspaceDiagnostic(context.Background(), "", protocol.WorkspaceDiagnosticParams{}); err == nil {
		t.Error("expected an error for an adapter without the workspace-pull surface")
	}
}

// ─── workspace/diagnostic/refresh (the server-request wrapper) ───────────────

func TestPool_WrapServerRequest_RefreshClearsPullState(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)
	inv.RecordPullFull("file:///p/a.go", "r1", []protocol.Diagnostic{{Severity: protocol.SevError, Message: "x"}})

	pool := newTestPool()
	e := &poolEntry{root: "/p", language: "go", inv: inv}

	innerCalled := ""
	inner := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		innerCalled = method
		return "inner-result", nil
	}
	handler := pool.wrapServerRequest(e, inner)

	// Refresh: answered promptly with null, pull state dropped, inner untouched.
	res, err := handler(context.Background(), protocol.MethodWorkspaceDiagnosticRefresh, nil)
	if res != nil || err != nil {
		t.Errorf("refresh must answer (nil, nil), got (%v, %v)", res, err)
	}
	if _, ok := inv.PullResultID("file:///p/a.go"); ok {
		t.Error("refresh must clear the stored pull result IDs")
	}
	if innerCalled != "" {
		t.Errorf("refresh must not reach the adapter handler, reached %q", innerCalled)
	}

	// Any other method delegates to the adapter's handler unchanged.
	res, err = handler(context.Background(), protocol.MethodRegisterCapability, nil)
	if err != nil || res != "inner-result" {
		t.Errorf("delegation = (%v, %v), want the inner result", res, err)
	}
	if innerCalled != protocol.MethodRegisterCapability {
		t.Errorf("inner handler saw %q, want registerCapability", innerCalled)
	}
}

func TestPool_WrapServerRequest_NilInner(t *testing.T) {
	pool := newTestPool()
	e := &poolEntry{root: "/p", language: "go"} // nil inv: refresh still safe
	handler := pool.wrapServerRequest(e, nil)

	if res, err := handler(context.Background(), protocol.MethodWorkspaceDiagnosticRefresh, nil); res != nil || err != nil {
		t.Errorf("refresh with nil inv must still answer (nil, nil), got (%v, %v)", res, err)
	}
	_, err := handler(context.Background(), "some/other", nil)
	var mnf *jsonrpc.MethodNotFoundError
	if err == nil || !asMethodNotFound(err, &mnf) {
		t.Errorf("unknown method with nil inner must answer MethodNotFoundError, got %v", err)
	}
}

// asMethodNotFound is a tiny errors.As stand-in (mirrors jsonrpc's internal
// helper) so the test does not need the errors package for one call.
func asMethodNotFound(err error, target **jsonrpc.MethodNotFoundError) bool {
	mnf, ok := err.(*jsonrpc.MethodNotFoundError)
	if ok {
		*target = mnf
	}
	return ok
}

// ─── routingInvProxy pull-state routing ──────────────────────────────────────

func newInv(t *testing.T) *cache.Invalidator {
	t.Helper()
	c := cache.New(time.Hour)
	t.Cleanup(c.Close)
	return cache.NewInvalidator(c)
}

func TestRoutingInvProxy_PullState_RoutesPerURI(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})
	installEntry(pool, rootB, &stubClient{id: "B"})
	invA := newInv(t)
	invB := newInv(t)
	pool.entries[poolKey{rootA, "go"}].inv = invA
	pool.entries[poolKey{rootB, "go"}].inv = invB

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(rootA, "go", invA)

	uriB := "file://" + filepath.Join(rootB, "main.go")
	rep := protocol.DocumentDiagnosticReport{
		Kind:     protocol.DiagnosticReportFull,
		ResultID: "rb",
		Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "b finding"}},
	}
	ri.RecordPullResult(uriB, rep)

	if _, ok := invA.PullResultID(uriB); ok {
		t.Error("rootB's report must not land in the primary invalidator")
	}
	if id, ok := invB.PullResultID(uriB); !ok || id != "rb" {
		t.Errorf("rootB's report should land in its own invalidator, got %q ok=%v", id, ok)
	}
	if id, ok := ri.PullResultID(uriB); !ok || id != "rb" {
		t.Errorf("routed PullResultID = (%q, %v), want (rb, true)", id, ok)
	}
	if !ri.RecordPullUnchanged(uriB, "rb") {
		t.Error("routed RecordPullUnchanged should validate against the owning invalidator")
	}
	if ri.RecordPullUnchanged(uriB, "nope") {
		t.Error("an unknown result ID must return false")
	}
}

func TestRoutingInvProxy_AllPullResultIDs_PrimaryScoped(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})
	installEntry(pool, rootB, &stubClient{id: "B"})
	invA := newInv(t)
	invB := newInv(t)
	pool.entries[poolKey{rootA, "go"}].inv = invA
	pool.entries[poolKey{rootB, "go"}].inv = invB

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(rootA, "go", invA)

	invA.RecordPullFull("file:///a/z.go", "ra2", nil)
	invA.RecordPullFull("file:///a/a.go", "ra1", nil)
	invB.RecordPullFull("file:///b/b.go", "rb", nil)

	ids := ri.AllPullResultIDs()
	if len(ids) != 2 || ids[0].Value != "ra1" || ids[1].Value != "ra2" {
		t.Errorf("expected the primary's IDs sorted by URI, got %#v", ids)
	}
}

func TestRoutingInvProxy_AllPullResultIDs_NoPrimary(t *testing.T) {
	ri := newRoutingInvProxy(newTestPool())
	if ids := ri.AllPullResultIDs(); ids == nil || len(ids) != 0 {
		t.Errorf("expected an empty non-nil slice, got %#v", ids)
	}
}
