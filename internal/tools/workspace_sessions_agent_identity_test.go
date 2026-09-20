package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// workspace_sessions answers "who else is here, and which one am I?" — and on a
// shared connection it answered both for the CONNECTION. The workspace it
// listed was the connection's pin, not the calling agent's, and the "(you)"
// marker keyed on the connection's session ID. Once an agent holds a row of its
// own (issue #472) that second half stops being merely incomplete and becomes
// wrong: the agent reads the roster of its own workspace, finds its own row,
// and cannot tell it from a peer — so it treats its own writes as a peer's and
// re-reads files nobody else touched.

// registerRow is a helper for building the on-disk roster these tests read.
func registerRow(t *testing.T, folder, parentID, externalID string) session.Info {
	t.Helper()
	info, err := session.Register(session.Info{Folder: folder, ParentID: parentID, ExternalID: externalID})
	if err != nil {
		t.Fatalf("register row in %s: %v", folder, err)
	}
	return info
}

func TestRosterAnswersForTheCallingAgentNotTheConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const parent = "/ws/parent"
	const worktree = "/ws/parent/worktree"

	conn := registerRow(t, parent, "", "")
	agent := registerRow(t, worktree, conn.ID, "sub")
	// A genuine peer in the agent's workspace, so "(you)" has something to be
	// distinguished FROM — a test where the only row is the caller's would pass
	// on a self-marker that marked every row.
	peer := registerRow(t, worktree, "", "other")

	ws := NewWorkspaceSessions(
		func() string { return parent },  // the CONNECTION's pin
		func() string { return conn.ID }, // the CONNECTION's id
	).WithAgentIdentity(func(context.Context) (string, string) {
		return worktree, agent.ID
	})

	out, err := ws.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The roster is the AGENT's workspace, proved by who is absent: the
	// connection's own row sits in the parent and must not be listed here.
	if strings.Contains(out, conn.Name) {
		t.Errorf("roster listed the connection's row (%s), so it is still the connection's workspace, got:\n%s", conn.Name, out)
	}
	if !strings.Contains(out, peer.Name) {
		t.Errorf("roster should list the peer (%s) sharing the agent's workspace, got:\n%s", peer.Name, out)
	}
	if !strings.Contains(out, agent.Name+" (you)") {
		t.Errorf("the calling agent's own row (%s) must be marked (you), got:\n%s", agent.Name, out)
	}
	if strings.Contains(out, peer.Name+" (you)") {
		t.Errorf("a peer (%s) must not be marked (you), got:\n%s", peer.Name, out)
	}
}

// An unwired accessor must leave the connection-scoped behaviour exactly as it
// was: every existing caller, and every single-agent connection, passes nil.
func TestRosterWithoutAgentIdentityStaysConnectionScoped(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const parent = "/ws/parent"

	conn := registerRow(t, parent, "", "")
	// A second row, so the listing takes the peer-list branch rather than the
	// "you are the only active session" one, which renders no (you) marker at
	// all and would make the assertion below vacuous.
	registerRow(t, parent, "", "other")

	ws := NewWorkspaceSessions(func() string { return parent }, func() string { return conn.ID })
	out, err := ws.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, conn.Name+" (you)") {
		t.Errorf("unwired, the connection's own row must still be marked (you), got:\n%s", out)
	}
}
