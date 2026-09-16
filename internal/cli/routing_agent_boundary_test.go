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
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
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

// pinSharedConnection pins the connection to connRoot and a declared agent to
// agentRoot, returning the agent's ctx. Both roots become real PathPolicies —
// which is what the guards consult (issue #499).
func pinSharedConnection(t *testing.T, s *connSession, connRoot, agentRoot string) context.Context {
	t.Helper()
	for _, root := range []string{connRoot, agentRoot} {
		mustGitDir(t, root)
	}
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
	return agentCtx
}

// TestPinnedPolicyGuard_AllowsEveryPinnedRoot covers the diagnostics inv-proxy's
// ctx-less guard: it admits a path under the connection's policy or any logical
// agent's shard policy, and still refuses a path under none.
func TestPinnedPolicyGuard_AllowsEveryPinnedRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-inv")
	t.Cleanup(s.close)

	rootA, rootB, unrelated := freshTempDir(t), freshTempDir(t), freshTempDir(t)
	pinSharedConnection(t, s, rootA, rootB)

	for _, root := range []string{rootA, rootB} {
		if err := s.pinnedPolicyGuard(filepath.Join(root, "x.go")); err != nil {
			t.Errorf("a root pinned by the connection or a logical agent was refused: %v", err)
		}
	}
	if err := s.pinnedPolicyGuard(filepath.Join(unrelated, "x.go")); err == nil {
		t.Error("a path under no pinned root was allowed; the guard is not defence in depth")
	}
}

// TestPinnedPolicyGuard_AdmitsRootsNoPinNames is defect 2 of issue #499: the
// guard used to test containment against the PINNED ROOTS alone, so a path the
// connection's own policy admits but no pin names — a client-granted allow-dir,
// a configured extra/read root, a dependency root — was refused. On the pull
// path that refusal drops the record and the tool answers "No issues found —
// pulled from the language server, file is clean": a false clean for a
// client-named path that was never checked.
func TestPinnedPolicyGuard_AdmitsRootsNoPinNames(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-inv-allowdir")
	t.Cleanup(s.close)

	connRoot, granted := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, connRoot)
	if _, err := s.repinWorkspace(context.Background(), connRoot, "", false); err != nil {
		t.Fatalf("pin the connection to %s: %v", connRoot, err)
	}
	s.onAllowDirs([]string{granted})

	file := filepath.Join(granted, "main.go")
	if err := s.readBoundaryGuard(file); err != nil {
		t.Fatalf("precondition: the connection policy must admit a client-granted allow-dir: %v", err)
	}
	if err := s.pinnedPolicyGuard(file); err != nil {
		t.Errorf("the union refused a path the connection policy admits (a granted allow-dir is not a pinned root): %v", err)
	}
}

// TestPinnedPolicyGuard_FailsClosedWhenARootPolicyWasRefused is defect 1 of
// issue #499: raw containment consulted the pinned root STRING even when
// buildPathPolicy had refused to build a policy for it — the #306 refusal,
// where the pinned directory is swapped for a symlink to a home-containing one.
// The guard then admitted paths under the swapped root (the review drove
// ~/.ssh/id_ed25519 through it and the tool rendered "injected for a home
// path"), i.e. it failed OPEN exactly where the policy fails CLOSED.
//
// The swap is staged with $HOME repointed at a directory this test owns, so the
// fixture contains a real home without touching the machine's. The control
// below proves the case is not vacuous: raw containment still admits the path,
// which is what the guard used to fall back to.
func TestPinnedPolicyGuard_FailsClosedWhenARootPolicyWasRefused(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-inv-refused")
	t.Cleanup(s.close)

	home := filepath.Join(freshTempDir(t), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create the fixture home: %v", err)
	}
	t.Setenv("HOME", home)
	pinned := filepath.Join(freshTempDir(t), "pinned")
	if err := os.Symlink(home, pinned); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A second, perfectly valid shard on the same connection: the union must not
	// let the refused root back in through it either.
	agentRoot := freshTempDir(t)
	agentPolicy := s.buildAgentPolicy(agentRoot, "none")

	s.mutate(func(v *sessionView) {
		v.acquiredRoot = pinned
		v.policy = s.buildPathPolicy(v)
	})
	s.shardsMu.Lock()
	s.shards = map[string]*agentShard{"agent-A": {id: "agent-A", root: agentRoot, policy: agentPolicy}}
	s.shardsMu.Unlock()
	if s.boundaryPolicy() != nil {
		t.Fatal("precondition: the swapped root must leave the connection with no policy")
	}

	file := filepath.Join(pinned, "id_ed25519")
	if !tools.PathWithinWorkspace(pinned, file) {
		t.Fatalf("control: raw containment would NOT have admitted %s, so this test no longer covers the fail-open it exists for", file)
	}
	if err := s.pinnedPolicyGuard(file); err == nil {
		t.Error("a path under a root whose policy could not be built was admitted — the guard fails open where the policy fails closed")
	}
}

// TestInvProxyBoundaryGuard_AttributedCallsUseTheCallersPolicy pins the two
// halves of the inv proxy's guard: an attributed call is judged by THAT agent's
// policy (so a server-supplied URI under a peer's root is refused), and an
// unattributed one falls back to the pinned-policy union.
func TestInvProxyBoundaryGuard_AttributedCallsUseTheCallersPolicy(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-inv-ctx")
	t.Cleanup(s.close)

	rootA, rootB := freshTempDir(t), freshTempDir(t)
	agentCtx := pinSharedConnection(t, s, rootA, rootB)

	// The peer's/connection's file: admitted by the connection policy, refused to
	// this agent — which is exactly what stops a relatedDocuments key under rootA
	// being recorded into rootA's cache on agent-A's call.
	peerFile := filepath.Join(rootA, "main.go")
	if err := s.readBoundaryGuard(peerFile); err != nil {
		t.Fatalf("precondition: the connection policy admits %s: %v", peerFile, err)
	}
	if err := s.invProxyBoundaryGuard(context.Background(), peerFile); err != nil {
		t.Errorf("the unattributed guard must keep the union's admission for %s: %v", peerFile, err)
	}
	if err := s.invProxyBoundaryGuard(agentCtx, peerFile); err == nil {
		t.Error("an attributed call admitted a path outside the CALLING agent's policy; a server-supplied URI could be recorded into a peer's cache")
	}
	if err := s.invProxyBoundaryGuard(agentCtx, filepath.Join(rootB, "main.go")); err != nil {
		t.Errorf("the attributed guard refused the calling agent's own file: %v", err)
	}
}
