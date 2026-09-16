package cli

// routing_agent_boundary_test.go — the routing proxies must honour the CALLING
// logical agent's workspace, not the connection's default pin.
//
// The defect: on a shared connection the LSP routing proxy was guarded by the
// connection-level policy. Whenever the connection's pin named one project and a
// declared agent another, every LSP-backed tool refused the agent's own
// workspace with "this connection is pinned to <the other project>". Several of
// those tools (get_definition, find_references, call_hierarchy, type_hierarchy,
// explain_symbol) carry no boundary guard of their own, so the proxy guard is
// their only guard — it has to be the agent's, not the connection's.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// TestRoutingProxy_BoundaryGuardResolvesTheCallingAgent pins the plumbing: the
// guard installed on the proxy must be handed the per-call ctx, so the
// ctx-aware guard can resolve the calling agent. A ctx-less guard here is what
// made every LSP tool use the connection pin.
func TestRoutingProxy_BoundaryGuardResolvesTheCallingAgent(t *testing.T) {
	rp := newRoutingProxy(nil)
	sentinel := errors.New("guard sentinel")
	var seen string
	rp.setBoundaryGuard(func(ctx context.Context, _ string) error {
		seen = mcp.LogicalAgentFromCtx(ctx)
		return sentinel
	})

	_, err := rp.route(mcp.WithLogicalAgent(context.Background(), "agent-7"), "file:///w/x.go", false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("route error = %v, want the guard's sentinel (the guard was not consulted)", err)
	}
	if seen != "agent-7" {
		t.Fatalf("guard saw logical agent %q, want agent-7 — the proxy dropped the identity and fell back to the connection pin", seen)
	}
}

// TestBoundaryGuards_AgentScopedGuardUsesTheAgentsPinNotTheConnections is the
// defect end-to-end at the guard layer: the connection is pinned to one root
// while a declared agent's shard is pinned to another. The per-agent guard must
// admit the agent's own file; the connection-level guard must still refuse it —
// proving the two really do diverge, so the fix is not vacuous.
func TestBoundaryGuards_AgentScopedGuardUsesTheAgentsPinNotTheConnections(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-boundary")
	t.Cleanup(s.close)

	connRoot, agentRoot := t.TempDir(), t.TempDir()
	mustGitDir(t, connRoot)
	mustGitDir(t, agentRoot)

	ctx := context.Background()
	if _, err := s.repinWorkspace(ctx, connRoot, "", false); err != nil {
		t.Fatalf("pin the connection to %s: %v", connRoot, err)
	}
	// Commit one identity so the connection is shared and the agent gets a shard.
	s.recordLogicalAgentAttach("coordinator")
	agentCtx := mcp.WithLogicalAgent(ctx, "agent-A")
	if _, refused := s.repinAgent(agentCtx, agentRoot, "", sessionstate.PinSourceSessionStart, true); refused != nil {
		t.Fatalf("per-agent re-pin refused: %v", refused)
	}

	file := filepath.Join(agentRoot, "x.go")
	if err := s.readBoundaryGuardFor(agentCtx, file); err != nil {
		t.Fatalf("the per-agent guard refused the agent's own workspace: %v", err)
	}
	if err := s.readBoundaryGuard(file); err == nil {
		t.Fatal("the connection-level guard allowed the other root; the divergence this fix addresses is not being exercised")
	}
}

// TestPinnedRootsGuard_AllowsEveryPinnedRoot covers the diagnostics inv-proxy's
// guard: it admits a path under the connection's root or any logical agent's
// shard root, and still refuses a path under none.
func TestPinnedRootsGuard_AllowsEveryPinnedRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-inv")
	t.Cleanup(s.close)

	rootA, rootB, unrelated := t.TempDir(), t.TempDir(), t.TempDir()
	s.shardsMu.Lock()
	s.shards = map[string]*agentShard{
		"agent-A": {id: "agent-A", root: rootA},
		"agent-B": {id: "agent-B", root: rootB},
	}
	s.shardsMu.Unlock()

	for _, root := range []string{rootA, rootB} {
		if err := s.pinnedRootsGuard(filepath.Join(root, "x.go")); err != nil {
			t.Errorf("a root pinned by a logical agent was refused: %v", err)
		}
	}
	if err := s.pinnedRootsGuard(filepath.Join(unrelated, "x.go")); err == nil {
		t.Error("a path under no pinned root was allowed; the guard is not defence in depth")
	}
}
