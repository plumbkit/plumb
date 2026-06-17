package cli

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// pullStubClient is a stubClient that also implements the pull-diagnostics
// surface (SupportsPullDiagnostics + Diagnostic). It records the URI it was
// asked to pull so tests can verify routingProxy delegated correctly.
type pullStubClient struct {
	stubClient
	supports bool
	report   *protocol.DocumentDiagnosticReport
	err      error

	mu     sync.Mutex
	pulled string
}

func (p *pullStubClient) SupportsPullDiagnostics() bool { return p.supports }

func (p *pullStubClient) Diagnostic(_ context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	p.mu.Lock()
	p.pulled = params.TextDocument.URI
	p.mu.Unlock()
	return p.report, p.err
}

var (
	_ lsp.Client        = (*pullStubClient)(nil)
	_ pullCapableClient = (*pullStubClient)(nil)
)

// TestRoutingProxy_SupportsPullDiagnostics_Delegates proves the dormancy fix:
// the proxy resolves the pull capability from its primary adapter (the URI-less
// path the diagnostics tool gates on) instead of always reporting false.
func TestRoutingProxy_SupportsPullDiagnostics_Delegates(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	client := &pullStubClient{supports: true}
	installEntry(pool, rootA, client)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	if !rp.SupportsPullDiagnostics() {
		t.Error("expected SupportsPullDiagnostics to delegate true from the primary adapter")
	}
}

// TestRoutingProxy_SupportsPullDiagnostics_AdapterReportsFalse confirms the
// proxy faithfully reports an adapter that implements pull but does not advertise
// it (the gopls/zls reality with the client capability un-advertised).
func TestRoutingProxy_SupportsPullDiagnostics_AdapterReportsFalse(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	client := &pullStubClient{supports: false}
	installEntry(pool, rootA, client)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	if rp.SupportsPullDiagnostics() {
		t.Error("expected false when the adapter implements pull but reports unsupported")
	}
}

// TestRoutingProxy_SupportsPullDiagnostics_NoPullSupport proves nil/structural
// safety: an adapter that does not implement the pull surface at all (plain
// stubClient, like gopls before this change) yields false rather than panicking.
func TestRoutingProxy_SupportsPullDiagnostics_NoPullSupport(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	if rp.SupportsPullDiagnostics() {
		t.Error("expected false for an adapter that does not implement pull diagnostics")
	}
}

// TestRoutingProxy_SupportsPullDiagnostics_NoPrimary proves the err-safe path:
// no primary attached yet → false, never an error or panic.
func TestRoutingProxy_SupportsPullDiagnostics_NoPrimary(t *testing.T) {
	pool := newTestPool()
	rp := newRoutingProxy(pool)
	if rp.SupportsPullDiagnostics() {
		t.Error("expected false when no primary is attached")
	}
}

// TestRoutingProxy_Diagnostic_RoutesByURI proves Diagnostic routes the pull
// request to the adapter owning the file's URI and delegates to it.
func TestRoutingProxy_Diagnostic_RoutesByURI(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	clientA := &pullStubClient{
		supports: true,
		report: &protocol.DocumentDiagnosticReport{
			Kind:  protocol.DiagnosticReportFull,
			Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "boom"}},
		},
	}
	clientB := &pullStubClient{supports: true, report: &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}}
	installEntry(pool, rootA, clientA)
	installEntry(pool, rootB, clientB)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	uriB := "file://" + filepath.Join(rootB, "main.go")
	rep, err := rp.Diagnostic(context.Background(), protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uriB},
	})
	if err != nil {
		t.Fatalf("Diagnostic: %v", err)
	}
	if rep == nil {
		t.Fatal("expected a report")
	}
	if clientB.pulled != uriB {
		t.Errorf("expected the pull to be routed to clientB for %q, got %q", uriB, clientB.pulled)
	}
	if clientA.pulled != "" {
		t.Errorf("clientA must not have been pulled, got %q", clientA.pulled)
	}
}

// TestRoutingProxy_Diagnostic_UnsupportedAdapter proves the graceful failure:
// when the routed adapter does not implement pull, Diagnostic returns a clear
// error so the diagnostics tool falls back to its push path.
func TestRoutingProxy_Diagnostic_UnsupportedAdapter(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].proxy)

	_, err := rp.Diagnostic(context.Background(), protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(rootA, "main.go")},
	})
	if err == nil {
		t.Error("expected an error when the routed adapter does not support pull")
	}
}
