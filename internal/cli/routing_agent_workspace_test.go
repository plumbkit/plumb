package cli

// routing_agent_workspace_test.go — URI-less queries must resolve the CALLING
// agent's workspace, not the connection's attach-time primary.
//
// The ctx-aware boundary guard (routing_agent_boundary_test.go) fixed which
// roots an LSP query may TOUCH. This pins the other half: which root a query
// with no URI argument SEARCHES. workspace_symbols and the whole-workspace
// diagnostics aggregate both used the connection's primary, so an agent pinned
// elsewhere was told its own symbols did not exist.

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/mcp"
)

const workspaceTestAgent = "agent-A"

// agentScopedWorkspace answers with the agent's root for the declared agent and
// "" otherwise, the same shape connSession.workspaceFor has on a shared
// connection.
func agentScopedWorkspace(agentRoot string) func(context.Context) string {
	return func(ctx context.Context) string {
		if mcp.LogicalAgentFromCtx(ctx) == workspaceTestAgent {
			return agentRoot
		}
		return ""
	}
}

// TestRoutingProxy_WorkspaceSymbolsUsesTheAgentsRoot: a URI-less symbol query
// from a declared agent searches THAT agent's project. Before the fix it used
// the connection's primary, so a symbol the URI-scoped query finds was reported
// missing.
func TestRoutingProxy_WorkspaceSymbolsUsesTheAgentsRoot(t *testing.T) {
	connRoot, agentRoot := t.TempDir(), t.TempDir()
	pool := newTestPool()
	installEntry(pool, connRoot, &stubClient{symbols: []protocol.SymbolInformation{
		stubSym("ConnFn", "file://"+filepath.Join(connRoot, "c.go")),
	}})
	installEntry(pool, agentRoot, &stubClient{symbols: []protocol.SymbolInformation{
		stubSym("AgentFn", "file://"+filepath.Join(agentRoot, "a.go")),
	}})

	rp := newRoutingProxy(pool)
	rp.setPrimary(connRoot, "go", pool.entries[poolKey{connRoot, "go"}].proxy)
	rp.setDiscovered(connRoot, nil)
	rp.setWorkspaceFn(agentScopedWorkspace(agentRoot))

	agentCtx := mcp.WithLogicalAgent(context.Background(), workspaceTestAgent)
	got, err := rp.WorkspaceSymbols(agentCtx, protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols as the agent: %v", err)
	}
	if names := stubSymNames(got); !slices.Equal(names, []string{"AgentFn"}) {
		t.Errorf("URI-less symbols = %v, want [AgentFn] — the query searched the connection root, not the agent's", names)
	}

	// An unattributed call keeps the connection's behaviour.
	base, err := rp.WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: "Fn"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols unattributed: %v", err)
	}
	if names := stubSymNames(base); !slices.Equal(names, []string{"ConnFn"}) {
		t.Errorf("unattributed URI-less symbols = %v, want [ConnFn]", names)
	}
}

// TestRoutingInvProxy_AllDiagnosticsFor_UsesTheAgentsRoot: the whole-workspace
// diagnostics aggregate is scoped the same way, and an unattributed call still
// answers from the connection.
func TestRoutingInvProxy_AllDiagnosticsFor_UsesTheAgentsRoot(t *testing.T) {
	connRoot, agentRoot := t.TempDir(), t.TempDir()
	pool := newTestPool()
	installEntry(pool, connRoot, &stubClient{id: "conn"})
	installEntry(pool, agentRoot, &stubClient{id: "agent"})
	invConn, invAgent := newInv(t), newInv(t)
	pool.entries[poolKey{connRoot, "go"}].inv = invConn
	pool.entries[poolKey{agentRoot, "go"}].inv = invAgent

	connURI := "file://" + filepath.Join(connRoot, "c.go")
	agentURI := "file://" + filepath.Join(agentRoot, "a.go")
	pushDiag(t, invConn, connURI, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "conn err"}})
	pushDiag(t, invAgent, agentURI, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "agent err"}})

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(connRoot, "go", invConn)
	ri.setWorkspaceFn(agentScopedWorkspace(agentRoot))

	agentCtx := mcp.WithLogicalAgent(context.Background(), workspaceTestAgent)
	got := ri.AllDiagnosticsFor(agentCtx)
	if _, ok := got[agentURI]; !ok {
		t.Errorf("AllDiagnosticsFor missing the agent's own diagnostics: %v", got)
	}
	if _, ok := got[connURI]; ok {
		t.Errorf("AllDiagnosticsFor leaked the connection project's diagnostics: %v", got)
	}

	base := ri.AllDiagnosticsFor(context.Background())
	if _, ok := base[connURI]; !ok {
		t.Errorf("unattributed AllDiagnosticsFor lost the connection's diagnostics: %v", base)
	}
	if _, ok := base[agentURI]; ok {
		t.Errorf("unattributed AllDiagnosticsFor leaked the agent's diagnostics: %v", base)
	}
}
