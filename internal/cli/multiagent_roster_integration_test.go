//go:build integration

package cli

// multiagent_roster_integration_test.go — issue #472: the roster half of the
// two-pin class.
//
// A connection registers exactly one session.Info, whose Folder is the
// CONNECTION's pin. workspace_sessions builds its roster by matching
// Folder == workspace (internal/tools/workspace_sessions.go), so a logical agent
// that pinned its own shard elsewhere is invisible in the workspace it is
// actually working in, and present in one it never touched. Observed on disk as
// a session file reading folder=…/yayl with external_id=…pauta.
//
// The topology here is the one the incident actually had, not an arbitrary pair
// of directories: a worktree INSIDE the parent checkout. #470 refuses a shard
// re-pin to an unrelated workspace and exempts it only when the roots contain
// one another, so contained roots are the only shape in which an agent legally
// holds a root different from its connection's — which is exactly why the bug
// was silent (every wrong path still resolved to a real file).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

func TestAgentIsVisibleInTheRosterOfItsOwnWorkspace(t *testing.T) {
	m := newMultiAgentConn(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	worktree := filepath.Join(parent, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	mustGitDir(t, worktree)

	// The coordinator takes the connection's pin, as the parent conversation does.
	if err := m.sessionStart(t, map[string]any{"session_id": "coord", "workspace": parent}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	// The subagent pins its OWN shard to the worktree. Contained in the parent,
	// so #470's containment exemption admits it; if this ever starts failing the
	// premise of the whole test has moved and the guard must be re-read before
	// the assertion below is touched.
	if err := m.sessionStart(t, map[string]any{"session_id": "sub", "workspace": worktree}); err != nil {
		t.Fatalf("subagent session_start into a contained worktree was refused: %v", err)
	}

	// Precondition: the agent really does hold the worktree as its own root.
	// Without this the assertion below could pass or fail for reasons that have
	// nothing to do with the roster.
	if got := m.s.workspaceFor(agentCtx("sub")); filepath.Clean(got) != filepath.Clean(worktree) {
		t.Fatalf("precondition: subagent shard root = %q, want %q", got, worktree)
	}

	rows, err := session.List()
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	// The roster question, asked exactly as workspace_sessions asks it.
	var inWorktree, inParent []string
	for _, r := range rows {
		switch filepath.Clean(r.Folder) {
		case filepath.Clean(worktree):
			inWorktree = append(inWorktree, r.ID)
		case filepath.Clean(parent):
			inParent = append(inParent, r.ID)
		}
	}
	if len(inWorktree) == 0 {
		t.Errorf("no session row has Folder=%s, so the subagent is invisible to the roster of the workspace it works in (issue #472); rows in the parent: %v", worktree, inParent)
	}
	// The connection itself must stay where the coordinator put it: making the
	// agent visible must not move the connection's own row, or this trades one
	// wrong-folder bug for another.
	if len(inParent) == 0 {
		t.Errorf("the connection's own row left Folder=%s; the coordinator must remain visible in its own workspace", parent)
	}
}

// A row that outlives the thing it describes is worse than the invisibility
// this feature fixes: a peer reads the roster, addresses the name, and nobody
// is listening. These two pin the lifecycle rather than the happy path.

func TestAgentRosterRowIsRetiredWhenTheAgentReturnsToTheConnectionRoot(t *testing.T) {
	m := newMultiAgentConn(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	worktree := filepath.Join(parent, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	mustGitDir(t, worktree)

	if err := m.sessionStart(t, map[string]any{"session_id": "coord", "workspace": parent}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"session_id": "sub", "workspace": worktree}); err != nil {
		t.Fatalf("subagent session_start: %v", err)
	}
	if rowsIn(t, worktree) == 0 {
		t.Fatal("precondition: the agent should hold a row in the worktree")
	}

	// Back to the connection's own root, where the connection's row already
	// lists it. force, because the agent's pin is its own and sticky by then.
	if err := m.sessionStart(t, map[string]any{"session_id": "sub", "workspace": parent, "force": true}); err != nil {
		t.Fatalf("subagent returning to the parent root: %v", err)
	}
	if n := rowsIn(t, worktree); n != 0 {
		t.Errorf("agent returned to the connection root but %d row(s) still claim the worktree", n)
	}
}

func TestAgentRosterRowsAreRetiredOnConnectionTeardown(t *testing.T) {
	m := newMultiAgentConn(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	worktree := filepath.Join(parent, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	mustGitDir(t, worktree)

	if err := m.sessionStart(t, map[string]any{"session_id": "coord", "workspace": parent}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"session_id": "sub", "workspace": worktree}); err != nil {
		t.Fatalf("subagent session_start: %v", err)
	}
	if rowsIn(t, worktree) == 0 {
		t.Fatal("precondition: the agent should hold a row in the worktree")
	}

	m.s.unregisterAgentRosters()
	if n := rowsIn(t, worktree); n != 0 {
		t.Errorf("connection torn down but %d agent row(s) still claim the worktree", n)
	}
}

// rowsIn counts live session rows whose Folder is dir, the way
// workspace_sessions counts them.
func rowsIn(t *testing.T, dir string) int {
	t.Helper()
	rows, err := session.List()
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	n := 0
	for _, r := range rows {
		if filepath.Clean(r.Folder) == filepath.Clean(dir) {
			n++
		}
	}
	return n
}

// A shard can hold a root different from its connection's WITHOUT a live re-pin
// having happened in this process: after a daemon restart the agent's pin is
// restored, and its next session_start names the root it already holds. That
// takes repinAgent's confirm branch, not its changed branch — so a roster row
// registered only on a move leaves the restored agent exactly as invisible as
// issue #472 describes, by a path no live-move test exercises.
//
// The restored state is modelled directly: a shard on the worktree with no row,
// which is what shardFor produces after a restart.
func TestRestoredAgentConfirmingItsRootIsStillListed(t *testing.T) {
	m := newMultiAgentConn(t)
	parent := freshTempDir(t)
	mustGitDir(t, parent)
	worktree := filepath.Join(parent, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	mustGitDir(t, worktree)

	if err := m.sessionStart(t, map[string]any{"session_id": "coord", "workspace": parent}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"session_id": "sub", "workspace": worktree}); err != nil {
		t.Fatalf("subagent session_start: %v", err)
	}

	// Model the restart: the shard keeps its root, the row is gone.
	m.s.shardsMu.Lock()
	sh := m.s.shards["sub"]
	m.s.shardsMu.Unlock()
	if sh == nil {
		t.Fatal("precondition: the subagent should hold a shard")
	}
	sh.mu.Lock()
	m.s.retireAgentRoster(sh)
	sh.mu.Unlock()
	if rowsIn(t, worktree) != 0 {
		t.Fatal("precondition: the modelled restart should leave no row")
	}

	// The reconnecting agent re-orients, naming the root it already holds.
	if err := m.sessionStart(t, map[string]any{"session_id": "sub", "workspace": worktree}); err != nil {
		t.Fatalf("restored subagent re-confirming its root: %v", err)
	}
	if n := rowsIn(t, worktree); n == 0 {
		t.Error("a restored agent that confirmed its own root has no roster row, so it is invisible in the workspace it works in (issue #472)")
	}
}
