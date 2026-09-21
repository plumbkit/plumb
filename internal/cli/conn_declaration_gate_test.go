package cli

// conn_declaration_gate_test.go — S4: a REFUSED workspace declaration must not
// leave the agent silently usable in the root it was seeded with.
//
// The sequence is the one the 2026-09-16 daemon log records for the DSH
// connection: a shard is seeded from the connection's pin (another
// conversation's workspace), the agent's own session_start is refused by the
// per-agent sticky guard, and every later relative-path call resolves inside the
// seeded root. Before this gate the only trace was one daemon-log warning.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/toolerror"
)

// shardRoot reads the root a logical agent's shard currently holds. A test that
// asserts "the shard did not move" must ask the shard, because workspaceFor
// deliberately refuses to report a root while the agent's declaration is
// pending — that refusal is the S4 behaviour, not a moved shard.
func shardRoot(t *testing.T, s *connSession, ctx context.Context) string {
	t.Helper()
	sh := s.shardFor(ctx)
	if sh == nil {
		t.Fatal("no shard for this agent — the connection is not shared")
	}
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.root
}

func TestRefusedDeclarationGatesWorkspaceDependentCalls(t *testing.T) {
	store, ss := newOriginStore(t)
	seeded := freshTempDir(t)
	mustGitDir(t, seeded)
	mustWrite(t, filepath.Join(seeded, "notes.md"), "another conversation's project\n")
	other := freshTempDir(t)
	mustGitDir(t, other)
	mustWrite(t, filepath.Join(other, "notes.md"), "the agent's own project\n")

	s := newPersistSession(t, store, ss, "proxy-gate")
	s.attachWorkspace(context.Background(), "file://"+seeded)
	// A peer naming the seeded root explicitly promotes the connection pin's
	// origin to session_start and turns the connection shared — the reported
	// shape, and what makes the next agent's shard sticky-seeded.
	ctxPeer := mcp.WithLogicalAgent(context.Background(), "peer")
	if _, err := s.repinWorkspace(ctxPeer, seeded, "", false, false); err != nil {
		t.Fatalf("the peer's same-root session_start: %v", err)
	}
	s.recordLogicalAgentAttach("peer")

	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-gated")
	// The agent's FIRST declaration, naming an unrelated project: refused,
	// because its shard is seeded from the connection root and never chose one.
	pinErr, err := s.repinWorkspace(ctxAgent, other, "", false, false)
	if err == nil {
		t.Fatal("a cross-workspace first declaration must be refused (fail-closed, PLAN-395)")
	}
	// The refusal must reach the WIRE with its classification intact: a client
	// recovers from what the envelope says, never from parsing the sentence.
	env, _, present := callFailingTool(t, err)
	if !present || env == nil {
		t.Fatal("the pin refusal carried no _meta envelope on the wire")
	}
	if env.Kind != string(toolerror.KindPinRefused) {
		t.Errorf("wire kind = %q, want %q", env.Kind, toolerror.KindPinRefused)
	}
	if env.Details["scope"] != "agent" {
		t.Errorf("wire details.scope = %q, want \"agent\" — the scope is what makes an automatic forced retry safe", env.Details["scope"])
	}
	if env.Remediation.Class != string(toolerror.ClassPassForce) || !env.Retryable {
		t.Errorf("wire remediation = %q retryable=%v, want %q retryable=true", env.Remediation.Class, env.Retryable, toolerror.ClassPassForce)
	}
	_ = pinErr
	if got := shardRoot(t, s, ctxAgent); got != seeded {
		t.Fatalf("the refused re-pin moved the shard to %q; it must stay at the seeded %q", got, seeded)
	}

	// The gate: a path-bearing call is refused with the remedy rather than
	// resolved inside the seeded root.
	berr := s.readBoundaryGuardFor(ctxAgent, filepath.Join(seeded, "notes.md"))
	if berr == nil {
		t.Fatal("a refused declaration left the agent usable in the seeded root")
	}
	if !strings.Contains(berr.Error(), "force: true") {
		t.Errorf("the gate must name the remedy that clears it: %v", berr)
	}
	te, ok := toolerror.Classify(berr)
	if !ok || te.Details["reason"] != "declaration_refused" || te.Details["scope"] != "agent" {
		t.Errorf("the gate must be machine-readable: %+v (classified=%v)", te, ok)
	}
	// ...and on the wire, where a client actually reads it.
	genv, _, gpresent := callFailingTool(t, berr)
	if !gpresent || genv == nil {
		t.Fatal("the gate refusal carried no _meta envelope on the wire")
	}
	if genv.Details["reason"] != "declaration_refused" || genv.Details["scope"] != "agent" {
		t.Errorf("wire gate envelope = %+v, want reason=declaration_refused scope=agent", genv.Details)
	}
	if genv.Remediation.Class != string(toolerror.ClassPassForce) || !genv.Retryable {
		t.Errorf("wire gate remediation = %q retryable=%v, want %q retryable=true", genv.Remediation.Class, genv.Retryable, toolerror.ClassPassForce)
	}
	if got := s.workspaceFor(ctxAgent); got != "" {
		t.Errorf("workspaceFor = %q, want \"\" — implicit resolution must not anchor to a root the agent never chose", got)
	}

	// The remedy is reachable, and it clears the gate.
	if _, err := s.repinWorkspace(ctxAgent, other, "", true, false); err != nil {
		t.Fatalf("the forced declaration the refusal names must succeed: %v", err)
	}
	if err := s.readBoundaryGuardFor(ctxAgent, filepath.Join(other, "notes.md")); err != nil {
		t.Errorf("after the declaration lands, the agent's own root must be readable: %v", err)
	}
	if got := s.workspaceFor(ctxAgent); got != other {
		t.Errorf("workspaceFor = %q, want %q", got, other)
	}
}

// TestChosenShardIsNeverGated is the dual, and it is why the marker is set only
// for a shard that never chose a root: an agent refused a move AWAY from its own
// workspace must keep working in that workspace.
func TestChosenShardIsNeverGated(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	other := freshTempDir(t)
	mustGitDir(t, other)

	s := newPersistSession(t, store, ss, "proxy-gate-chosen")
	s.attachWorkspace(context.Background(), "file://"+parent)
	s.recordLogicalAgentAttach("peer")

	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-chosen")
	// Same-tree correction: this one LANDS, so the shard has chosen a root.
	if _, err := s.repinWorkspace(ctxAgent, worktree, "", false, false); err != nil {
		t.Fatalf("the agent's own same-tree pin: %v", err)
	}
	// A move away from a root it chose is still refused (issue #182)...
	if _, err := s.repinWorkspace(ctxAgent, other, "", false, false); err == nil {
		t.Fatal("a move away from the agent's own workspace must be refused without force")
	}
	// ...but the agent keeps its own workspace: no gate, and the boundary still
	// admits its files.
	if got := s.workspaceFor(ctxAgent); got != worktree {
		t.Errorf("workspaceFor = %q, want %q", got, worktree)
	}
	if err := s.readBoundaryGuardFor(ctxAgent, filepath.Join(worktree, "notes.md")); err != nil {
		t.Errorf("an agent refused a move away was gated out of its OWN workspace: %v", err)
	}
}
