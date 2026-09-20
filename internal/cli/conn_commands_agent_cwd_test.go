package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// run_command and run_task have no workspace argument: their working directory
// comes from the resolver. Both resolvers asked s.workspace() — the CONNECTION's
// pin — so an agent holding its own shard ran its build, tests and scripts in
// whichever project the connection was pinned to.
//
// That is the dangerous sibling of the wrong-root READ in issue #472's
// observation 5, and silent for the same reason: a worktree sits inside its
// parent checkout, so every path still resolves and the commands still succeed
// — against the wrong tree. PLAN-440 item 4.
func TestRunCommandResolvesTheCallingAgentsRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	parent := t.TempDir()
	writeExecProject(t, parent, "[[command]]\nname = \"build\"\nexec = [\"go\", \"build\"]\n")
	grantExecTrust(t, parent)

	worktree := filepath.Join(parent, "worktree")
	mustGitDir(t, worktree)

	s := execTrustSession(t, parent)

	// Two identities make the connection shared, which is what gives the agent
	// a shard of its own to pin.
	s.recordLogicalAgentCall("coordinator")
	s.recordLogicalAgentCall("subagent")
	ctx := mcp.WithLogicalAgent(context.Background(), "subagent")
	if _, err := s.repinAgent(ctx, worktree, "", sessionstate.PinSourceSessionStart, false); err != nil {
		t.Fatalf("pinning the subagent to its worktree: %v", err)
	}
	if got := s.workspaceFor(ctx); filepath.Clean(got) != filepath.Clean(worktree) {
		t.Fatalf("precondition: agent root = %q, want %q", got, worktree)
	}

	got, err := s.commandResolver(ctx, "build", "")
	if err != nil {
		t.Fatalf("resolving build for the subagent: %v", err)
	}
	if filepath.Clean(got.WorkingDir) != filepath.Clean(worktree) {
		t.Errorf("run_command would execute in %q, but the calling agent is pinned to %q — "+
			"the command runs against the wrong tree", got.WorkingDir, worktree)
	}
}

// The connection's own caller is unaffected: an unidentified call, and a
// single-agent connection, still resolve against the connection's pin.
func TestRunCommandStillResolvesTheConnectionRootForItsOwnCaller(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	parent := t.TempDir()
	writeExecProject(t, parent, "[[command]]\nname = \"build\"\nexec = [\"go\", \"build\"]\n")
	grantExecTrust(t, parent)

	s := execTrustSession(t, parent)

	got, err := s.commandResolver(context.Background(), "build", "")
	if err != nil {
		t.Fatalf("resolving build on a single-agent connection: %v", err)
	}
	if filepath.Clean(got.WorkingDir) != filepath.Clean(parent) {
		t.Errorf("single-agent connection resolved %q, want the connection pin %q", got.WorkingDir, parent)
	}
}
