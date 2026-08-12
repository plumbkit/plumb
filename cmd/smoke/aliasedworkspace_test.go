//go:build integration

package smoke_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeAliasedFixture builds ONE marker-only workspace reachable by two path
// spellings and returns them: the resolved path, and an equivalent path that
// traverses a symlink. The alias is deliberately constructed rather than relying
// on the platform's own (macOS /tmp → /private/tmp), so the scenario means the
// same thing on Linux.
func makeAliasedFixture(t *testing.T) (realRoot, alias string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving fixture base: %v", err)
	}
	realRoot = filepath.Join(base, "real", "proj")
	if err := os.MkdirAll(filepath.Join(realRoot, ".plumb"), 0o755); err != nil {
		t.Fatalf("creating .plumb: %v", err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatalf("creating the alias symlink: %v", err)
	}
	alias = filepath.Join(base, "alias", "proj")
	if filepath.Clean(alias) == filepath.Clean(realRoot) {
		t.Fatalf("fixture is not aliased: both spellings clean to %q", realRoot)
	}
	return realRoot, alias
}

// TestSmoke_TwoSessions_AliasedWorkspaceDeliversSameProject is issue #263 end to
// end, against real `plumb serve` processes and a real daemon.
//
// Two sessions pin ONE project by two different spellings — the everyday macOS
// /tmp → /private/tmp firmlink, a checkout under a symlinked parent. Before the
// root was canonicalised they disagreed about where they were, and the damage
// was silent: sameWorkspace compared the two roots textually, leave_note took
// the cross-project branch for a peer sitting in the same folder, and delivery
// dropped the message unread because [collab] cross_project is off by default —
// while telling the sender the recipient was "pinned to" another project.
//
// The unit tests cover the resolution itself. This exists because the failure
// was a whole-pipeline one: pin → session registry → peer lookup → routing
// decision → store selection → delivery, five layers that each looked correct
// on its own. Run against main it reproduces every symptom below.
func TestSmoke_TwoSessions_AliasedWorkspaceDeliversSameProject(t *testing.T) {
	plumbBin := buildPlumb(t)
	realRoot, alias := makeAliasedFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Two proxies, one daemon (shared tmpHome), one project — reached two ways.
	clientA := newMCPClient(t, ctx, plumbBin, tmpHome, realRoot)
	clientA.initialize(t, realRoot)
	clientB := newMCPClient(t, ctx, plumbBin, tmpHome, alias)
	clientB.initialize(t, alias)

	t.Log("session_start: A via the resolved path, B via the alias")
	outA := clientA.call(t, "session_start", map[string]any{"workspace": realRoot}, sessionStartTimeout)
	outB := clientB.call(t, "session_start", map[string]any{"workspace": alias}, sessionStartTimeout)

	// Both must report the SAME workspace, and it must be the resolved one.
	assertContains(t, "session_start A", outA, realRoot)
	assertContains(t, "session_start B (aliased pin must resolve)", outB, realRoot)
	if strings.Contains(outB, alias) {
		t.Errorf("B reports the aliased spelling %q; the pin must be resolved:\n%s", alias, outB)
	}

	// The session registry is what the mailbox consults to find a peer.
	clientB.call(t, "rename_session", map[string]any{"name": "bee"}, toolTimeout)
	t.Log("workspace_sessions: A must see B despite the differing spellings")
	sessions := clientA.call(t, "workspace_sessions", map[string]any{}, toolTimeout)
	assertContains(t, "workspace_sessions", sessions, "active sessions: 2")
	assertContains(t, "peer visible to A", sessions, "bee")

	// The heart of it: same project, so the note must NOT take the cross-project
	// branch — that branch is where the default config silently drops it.
	t.Log("leave_note: A → B must route same-project")
	const body = "issue 263: same project, two spellings"
	sent := clientA.call(t, "leave_note", map[string]any{"to": "bee", "body": body}, toolTimeout)
	if strings.Contains(strings.ToLower(sent), "cross-project") {
		t.Fatalf("a same-project message was routed cross-project, where the default "+
			"config drops it unread:\n%s", sent)
	}

	// Routing correctly is not delivery. Assert B actually receives it.
	t.Log("check_messages: B must actually receive it")
	got := clientB.call(t, "check_messages", map[string]any{"wait_seconds": 5}, toolTimeout)
	assertContains(t, "B's inbox", got, body)

	// The other half of the fix: a path the agent names by the workspace's other
	// spelling must still be recognised as inside it. When this regressed, the
	// tool contradicted the boundary guard that had just admitted the same path.
	t.Log("relevant_memories: an aliased path must resolve inside the workspace")
	rel := clientB.call(t, "relevant_memories",
		map[string]any{"path": filepath.Join(alias, "internal", "auth", "token.go")}, toolTimeout)
	if strings.Contains(rel, "is not inside workspace") {
		t.Fatalf("an in-project path named by the workspace's other spelling was "+
			"reported outside it:\n%s", rel)
	}
}
