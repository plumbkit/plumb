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

// makeMarkerFixture creates a workspace with only a .plumb/ marker and a plain
// text file: no language root, so it attaches as LanguageNone and the scenario
// needs no language server (fast, runs anywhere).
func makeMarkerFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".plumb"), 0o755); err != nil {
		t.Fatalf("creating .plumb: %v", err)
	}
	shared := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(shared, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving fixture path: %v", err)
	}
	return resolved
}

// TestSmoke_TwoSessions_StaleEditRejected is the daemon tier of the
// multi-agent coexistence tests (review-plan B4 W2): two real `plumb serve`
// proxies share one daemon (same isolated HOME/XDG tree) and one workspace.
// Session A reads a file and captures its version header; session B edits the
// same file; A's edit guarded by the now-stale expected_mtime must be rejected
// with the staleness message, B's content must survive intact, and each
// session must see the other in workspace_sessions.
func TestSmoke_TwoSessions_StaleEditRejected(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeMarkerFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Two proxies, one daemon: both clients share tmpHome, so the second
	// `plumb serve` dials the daemon the first one spawned.
	clientA := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	clientA.initialize(t, fixture)
	clientB := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	clientB.initialize(t, fixture)

	t.Log("session_start A and B (marker-only workspace, no LSP)")
	outA := clientA.call(t, "session_start", map[string]any{"workspace": fixture}, sessionStartTimeout)
	assertContains(t, "session_start A", outA, fixture)
	outB := clientB.call(t, "session_start", map[string]any{"workspace": fixture}, sessionStartTimeout)
	assertContains(t, "session_start B", outB, fixture)

	shared := filepath.Join(fixture, "shared.txt")

	// A reads the file and captures the optimistic-concurrency header.
	t.Log("read_file (A captures mtime)")
	readOut := clientA.call(t, "read_file", map[string]any{"file_path": shared}, toolTimeout)
	staleMtime := extractMtime(t, readOut)

	// B edits the same file; A's captured mtime is now stale.
	t.Log("edit_file (B edits the shared file)")
	editOut := clientB.call(t, "edit_file", map[string]any{
		"file_path": shared,
		"edits":     []map[string]any{{"old_string": "alpha", "new_string": "alpha (B)"}},
	}, toolTimeout)
	assertContains(t, "edit_file B", editOut, "applied 1 edit")

	// A's guarded edit must be rejected with the staleness message.
	t.Log("edit_file (A's stale guarded edit must be rejected)")
	text, isErr := callResult(t, clientA, "edit_file", map[string]any{
		"file_path":      shared,
		"edits":          []map[string]any{{"old_string": "beta", "new_string": "beta (A)"}},
		"expected_mtime": staleMtime,
	}, toolTimeout)
	if !isErr {
		t.Fatalf("edit with a stale expected_mtime must be rejected, got success:\n%s", text)
	}
	assertContains(t, "stale-edit rejection", text, "was modified since you read it")

	// B's edit survived and A's rejected edit left no trace.
	verify := clientA.call(t, "read_file", map[string]any{"file_path": shared}, toolTimeout)
	assertContains(t, "final content keeps B's edit", verify, "alpha (B)")
	if strings.Contains(verify, "beta (A)") {
		t.Fatalf("rejected edit must not modify the file:\n%s", verify)
	}

	// Each session sees the other.
	t.Log("workspace_sessions (A must see both sessions)")
	sessions := clientA.call(t, "workspace_sessions", map[string]any{}, toolTimeout)
	assertContains(t, "workspace_sessions", sessions, "active sessions: 2")
}
