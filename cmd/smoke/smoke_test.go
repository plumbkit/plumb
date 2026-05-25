//go:build integration

package smoke_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestSmoke_EndToEnd is the wire-level happy path: a real `plumb serve`
// subprocess driven over JSON-RPC 2.0 through the core read/edit/write loop
// against a Go fixture, asserting post-write diagnostics arrive from a real
// gopls. It is the integration complement to the live `selftest` MCP prompt
// (which drives the full tool surface against a sandbox) — see
// internal/mcp/selftest_prompt.go.
func TestSmoke_EndToEnd(t *testing.T) {
	requireGopls(t)

	plumbBin := buildPlumb(t)
	fixture := makeFixture(t)
	tmpHome := mkTmpHome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)

	// ── MCP handshake ────────────────────────────────────────────────────────
	client.initialize(t, fixture)

	// ── session_start attaches the workspace and returns orientation ─────────
	// The explicit workspace arg drives OnBeforeTool → attachWorkspace → gopls
	// start. This is the slow step; we allow a generous timeout.
	t.Log("session_start (may wait for gopls to start…)")
	sessionOut := client.call(t, "session_start",
		map[string]any{"workspace": fixture}, sessionStartTimeout)
	assertContains(t, "session_start", sessionOut, "Language: Go")
	assertContains(t, "session_start", sessionOut, fixture)

	mainGo := filepath.Join(fixture, "main.go")

	// ── read_file returns the mtime header ───────────────────────────────────
	t.Log("read_file")
	readOut := client.call(t, "read_file", map[string]any{"file_path": mainGo}, toolTimeout)
	assertContains(t, "read_file", readOut, "# plumb-read mtime=")
	assertContains(t, "read_file", readOut, "func main()")
	mtime := extractMtime(t, readOut)

	// ── edit_file applies a valid change ─────────────────────────────────────
	t.Log("edit_file (valid change)")
	editOut := client.call(t, "edit_file", map[string]any{
		"file_path": mainGo,
		"edits": []map[string]any{
			{"old_string": `g.Greet("world")`, "new_string": `g.Greet("smoke test")`},
		},
		"expected_mtime": mtime,
		"dirty_ok":       true,
	}, toolTimeout)
	assertContains(t, "edit_file", editOut, "applied 1 edit")

	// ── write_file a new broken file; gopls must report diagnostics ──────────
	// A brand-new file (FileCreated notification) makes gopls load it fresh —
	// the same pattern as the gopls adapter integration test. Editing an
	// existing file on a cold workspace can outrun the post-write diagnostics
	// window because gopls may not have the file in its in-memory view yet.
	t.Log("write_file (new file with syntax error — expect diagnostics)")
	brokenGo := filepath.Join(fixture, "broken.go")
	syntaxOut := client.call(t, "write_file", map[string]any{
		"file_path": brokenGo,
		"content":   "package main\n\nfunc broken( { } // missing closing paren\n",
	}, toolTimeout)
	assertContains(t, "write_file(syntax error)", syntaxOut, "diagnostics after write")

	// Remove broken.go so gopls is clean for any further steps.
	t.Log("removing broken.go")
	client.call(t, "delete_file", map[string]any{"file_path": brokenGo}, toolTimeout)

	// ── list_memories returns without error ──────────────────────────────────
	t.Log("list_memories")
	_ = client.call(t, "list_memories", map[string]any{}, toolTimeout)
}
