package cli

// routing_inv_agent_isolation_test.go — issue #499, the cross-agent half of the
// diagnostics routing INV proxy.
//
// A pull record is written into the cache of the workspace owning the URI, and
// the only thing that may decide which workspaces a record can enter is the
// CALLING agent's boundary policy. The URIs on this path are not all the
// caller's: relatedDocuments keys and workspace-report items come from the
// LANGUAGE SERVER, which is exactly the gap the tool's ctx-aware entry guard
// cannot cover. Checking them against every root pinned on the connection (the
// pre-fix union) admitted a URI under a PEER agent's shard root — that agent's
// own policy is what admits it — and recorded the report into the PEER's cache
// while the caller was shown nothing.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// pullOpener is a pull-mode diagnostics opener: every URI resolves to pull mode
// and is answered with whatever report() returns for it.
type pullOpener struct {
	report func(uri string) *protocol.DocumentDiagnosticReport
}

func (o *pullOpener) DidOpen(context.Context, protocol.DidOpenTextDocumentParams) error { return nil }

func (o *pullOpener) DidClose(context.Context, protocol.DidCloseTextDocumentParams) error {
	return nil
}
func (o *pullOpener) DiagnosticsMode(string) string { return diagModePull }
func (o *pullOpener) Diagnostic(_ context.Context, p protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	return o.report(p.TextDocument.URI), nil
}

func executeDiagnostics(t *testing.T, tool *tools.Diagnostics, ctx context.Context, uri string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"uri": uri})
	if err != nil {
		t.Fatalf("marshal diagnostics args: %v", err)
	}
	out, err := tool.Execute(ctx, raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	return out
}

// twoProjectRoutingInvProxy mounts the two-project routing fixture: projA is the
// connection's primary, projB a second acquired workspace, each with its own
// invalidator.
func twoProjectRoutingInvProxy(t *testing.T) (*routingInvProxy, *workspacePool, string, string) {
	t.Helper()
	rootA, rootB := setupTwoProjects(t)
	pool := newTestPool()
	installEntry(pool, rootA, &stubClient{id: "A"})
	installEntry(pool, rootB, &stubClient{id: "B"})
	pool.entries[poolKey{rootA, "go"}].inv = newInv(t)
	pool.entries[poolKey{rootB, "go"}].inv = newInv(t)

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(rootA, "go", pool.entries[poolKey{rootA, "go"}].inv)
	return ri, pool, rootA, rootB
}

func invFor(t *testing.T, pool *workspacePool, root string) *cache.Invalidator {
	t.Helper()
	e := pool.entries[poolKey{root, "go"}]
	if e == nil || e.inv == nil {
		t.Fatalf("no invalidator installed for %s", root)
	}
	return e.inv
}

// TestRoutingInvProxy_AttributedPullRecordUsesTheCallingAgentsPolicy is the
// cross-agent cache write at the layer that performs it. The connection is
// pinned to rootA and agent-A's shard to rootB; the report the server returned
// for agent-A's own file names a related document under rootA. Recorded through
// the attributed surface, agent-A's policy refuses that related document, so
// neither it nor its result ID crosses into rootA's cache.
func TestRoutingInvProxy_AttributedPullRecordUsesTheCallingAgentsPolicy(t *testing.T) {
	ri, pool, rootA, rootB := twoProjectRoutingInvProxy(t)
	invA, invB := invFor(t, pool, rootA), invFor(t, pool, rootB)

	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "inv-cross-agent")
	t.Cleanup(s.close)
	ri.setBoundaryGuard(s.invProxyBoundaryGuard)
	agentCtx := pinSharedConnection(t, s, rootA, rootB)

	uriA := "file://" + filepath.Join(rootA, "peer.go")
	uriB := "file://" + filepath.Join(rootB, "mine.go")
	applied, unresolved := ri.RecordPullResultFor(agentCtx, uriB, protocol.DocumentDiagnosticReport{
		Kind:     protocol.DiagnosticReportFull,
		ResultID: "rb",
		Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "mine boom"}},
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			uriA: {
				Kind:     protocol.DiagnosticReportFull,
				ResultID: "ra",
				Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "peer boom"}},
			},
		},
	})
	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %#v, want none: a refused related document is dropped, not surfaced as unverified", unresolved)
	}
	if !reflect.DeepEqual(applied, []string{uriB}) {
		t.Fatalf("applied = %#v, want only the calling agent's own file", applied)
	}
	if invA.Tracked(uriA) {
		t.Error("a server-supplied related document under a PEER's root was recorded into that peer's cache")
	}
	if id, ok := invB.PullResultID(uriB); !ok || id != "rb" {
		t.Errorf("the agent's own report must land in its own cache: PullResultID = (%q, %v)", id, ok)
	}
}

// TestDiagnosticsPull_ServerRelatedURICannotLandInAPeersCache drives the tool
// end to end against the real routing proxy: agent-A pulls its own file, the
// server's report names a related document in the peer's project, and neither
// the peer's cache nor the caller's report may show it.
func TestDiagnosticsPull_ServerRelatedURICannotLandInAPeersCache(t *testing.T) {
	ri, pool, rootA, rootB := twoProjectRoutingInvProxy(t)
	invA := invFor(t, pool, rootA)

	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "inv-cross-agent-e2e")
	t.Cleanup(s.close)
	ri.setBoundaryGuard(s.invProxyBoundaryGuard)
	ri.setWorkspaceFn(s.workspaceFor)
	agentCtx := pinSharedConnection(t, s, rootA, rootB)

	uriA := "file://" + filepath.Join(rootA, "peer.go")
	uriB := "file://" + filepath.Join(rootB, "mine.go")
	opener := &pullOpener{report: func(uri string) *protocol.DocumentDiagnosticReport {
		if uri != uriB {
			return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}
		}
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "rb",
			Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "mine boom"}},
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				uriA: {
					Kind:     protocol.DiagnosticReportFull,
					ResultID: "ra",
					Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "peer boom"}},
				},
			},
		}
	}}
	tool := tools.NewDiagnosticsWithOpener(ri, opener).WithBoundary(s.readBoundaryGuardFor)

	out := executeDiagnostics(t, tool, agentCtx, uriB)
	if !strings.Contains(out, "mine boom") {
		t.Fatalf("the calling agent's own diagnostic is missing from the report:\n%s", out)
	}
	if strings.Contains(out, "peer boom") {
		t.Errorf("a server-supplied related document under a PEER's root was rendered to the caller:\n%s", out)
	}
	if invA.Tracked(uriA) {
		t.Error("a server-supplied related document was recorded into a peer's cache")
	}
}

// TestDiagnosticsPull_GrantedAllowDirIsNotAFalseClean is defect 2 of issue #499
// end to end. A client-granted allow-dir is inside the connection's policy but
// is NOT a pinned root, so the inv proxy's old containment guard dropped the
// pull record; with the cache left empty the tool answered "No issues found —
// pulled from the language server, file is clean" for a file whose error the
// server had just reported.
func TestDiagnosticsPull_GrantedAllowDirIsNotAFalseClean(t *testing.T) {
	ri, pool, rootA, granted := twoProjectRoutingInvProxy(t)
	invB := invFor(t, pool, granted)

	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "inv-allowdir-e2e")
	t.Cleanup(s.close)
	ri.setBoundaryGuard(s.invProxyBoundaryGuard)
	ri.setWorkspaceFn(s.workspaceFor)
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false, false); err != nil {
		t.Fatalf("pin the connection to %s: %v", rootA, err)
	}
	s.onAllowDirs([]string{granted})

	uri := "file://" + filepath.Join(granted, "main.go")
	opener := &pullOpener{report: func(string) *protocol.DocumentDiagnosticReport {
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "rg",
			Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "granted boom"}},
		}
	}}
	tool := tools.NewDiagnosticsWithOpener(ri, opener).WithBoundary(s.readBoundaryGuardFor)

	out := executeDiagnostics(t, tool, context.Background(), uri)
	if !invB.Tracked(uri) {
		t.Error("the pull record for a client-granted allow-dir was dropped instead of landing in the owning cache")
	}
	if strings.Contains(out, "No issues found") {
		t.Fatalf("a granted allow-dir was reported clean although the server reported an error:\n%s", out)
	}
	if !strings.Contains(out, "granted boom") {
		t.Fatalf("the server's diagnostic for a granted allow-dir is missing from the report:\n%s", out)
	}
}
