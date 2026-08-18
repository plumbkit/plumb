package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// symlinkEscapeTree builds a workspace carrying the shapes a hostile repository
// can commit — git stores symlinks natively, so a clone reproduces every one of
// them on disk:
//
//	ws/real.go        an ordinary in-workspace file
//	ws/innocent.env   → base/secret.txt        (file OUTSIDE the workspace)
//	ws/escape.dir     → base/outside           (directory OUTSIDE the workspace)
//	ws/inside.link    → ws/real.go             (legitimate in-workspace symlink)
//	ws/dangling.link  → base/gone/missing.txt  (target does not exist)
//
// It returns the workspace root.
func symlinkEscapeTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(base, "secret.txt"), "PRIVATE_KEY_MATERIAL\n")
	write(filepath.Join(outside, "leak.txt"), "DIRECTORY_ESCAPE_MARKER\n")
	write(filepath.Join(ws, "real.go"), "package ws\n\n// IN_WORKSPACE_MARKER\n")

	link := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	link(filepath.Join(base, "secret.txt"), "innocent.env")
	link(outside, "escape.dir")
	link(filepath.Join(ws, "real.go"), "inside.link")
	link(filepath.Join(base, "gone", "missing.txt"), "dangling.link")
	return ws
}

// readGuard is a real, symlink-resolving read boundary for ws, matching the
// per-connection policy the daemon installs.
func readGuard(ws string) BoundaryGuard {
	pol := NewPathPolicy(ws, []AllowedRoot{{Path: ws, Access: AccessReadWrite, Label: "workspace"}})
	return func(_ context.Context, path string) error { _, err := pol.Check(path, AccessRead); return err }
}

// resultBody strips the trailing withheld advisory, which necessarily names the
// very paths the escape assertions look for.
func resultBody(out string) string {
	body, _, _ := strings.Cut(out, "\n\nNote:")
	return body
}

func searchFor(t *testing.T, ws, pattern string) string {
	t.Helper()
	tool := NewSearchInFiles(wsFn(ws), nil, nil, 0).WithBoundary(readGuard(ws))
	args, _ := json.Marshal(map[string]any{"path": ws, "pattern": pattern})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("search_in_files %q: %v", pattern, err)
	}
	return out
}

// TestSearchInFiles_SymlinkEscapesWorkspace is the regression test for the
// read-boundary escape: search_in_files boundary-checked only its root, so an
// in-tree symlink to a file outside the workspace had its target's CONTENT
// returned to the agent.
func TestSearchInFiles_SymlinkEscapesWorkspace(t *testing.T) {
	ws := symlinkEscapeTree(t)

	// The pattern echoes back in the "No matches" line, so each assertion checks
	// the RESULT BODY (everything before the withheld advisory) for a hit.
	out := searchFor(t, ws, "PRIVATE_KEY_MATERIAL")
	if !strings.Contains(resultBody(out), "No matches") {
		t.Errorf("search_in_files returned the contents of a file outside the workspace via an in-tree symlink:\n%s", out)
	}

	// A symlink to a DIRECTORY outside the workspace must not be descended into.
	out = searchFor(t, ws, "DIRECTORY_ESCAPE_MARKER")
	if !strings.Contains(resultBody(out), "No matches") {
		t.Errorf("search_in_files descended a symlink to a directory outside the workspace:\n%s", out)
	}
}

// TestSearchInFiles_InWorkspaceSymlinkStillSearched pins the other half of the
// contract: a symlink whose target is inside the workspace is legitimate and
// must keep being searched.
func TestSearchInFiles_InWorkspaceSymlinkStillSearched(t *testing.T) {
	ws := symlinkEscapeTree(t)
	out := searchFor(t, ws, "IN_WORKSPACE_MARKER")
	for _, want := range []string{"real.go", "inside.link"} {
		if !strings.Contains(out, want) {
			t.Errorf("search_in_files did not report %s; in-workspace symlinks must keep working:\n%s", want, out)
		}
	}
}

// TestSearchInFiles_ReportsWithheldSymlinks asserts the omission is announced.
// A search that silently under-reports is its own hazard: the agent reads a
// clean "no matches" as proof of absence.
func TestSearchInFiles_ReportsWithheldSymlinks(t *testing.T) {
	ws := symlinkEscapeTree(t)
	out := searchFor(t, ws, "PRIVATE_KEY_MATERIAL")
	if !strings.Contains(out, "withheld") || !strings.Contains(out, "innocent.env") {
		t.Errorf("search_in_files did not report the withheld symlinks:\n%s", out)
	}
}

func listFiles(t *testing.T, ws string) string {
	t.Helper()
	tool := NewFindFiles(wsFn(ws)).WithBoundary(readGuard(ws))
	args, _ := json.Marshal(map[string]any{"path": ws, "type": "any"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("find_files: %v", err)
	}
	return out
}

// TestFindFiles_SymlinkEscapesWorkspace covers the existence-disclosure half of
// the same escape: find_files listed in-tree symlinks that resolve outside the
// workspace, and fed those paths to other tools.
func TestFindFiles_SymlinkEscapesWorkspace(t *testing.T) {
	ws := symlinkEscapeTree(t)
	out := listFiles(t, ws)

	for _, banned := range []string{"innocent.env", "escape.dir"} {
		if strings.Contains(resultBody(out), banned) {
			t.Errorf("find_files listed %s, a symlink resolving outside the workspace:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "inside.link") {
		t.Errorf("find_files dropped an in-workspace symlink:\n%s", out)
	}
	if !strings.Contains(out, "withheld") {
		t.Errorf("find_files did not report the withheld symlinks:\n%s", out)
	}
}

// TestFindFiles_DanglingSymlinkSurvivesWalk pins that a symlink whose target
// does not exist neither panics nor aborts the walk: it resolves nowhere, so it
// escapes nothing and is listed like any other entry.
func TestFindFiles_DanglingSymlinkSurvivesWalk(t *testing.T) {
	ws := symlinkEscapeTree(t)
	out := listFiles(t, ws)
	for _, want := range []string{"dangling.link", "real.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("find_files lost %s when the tree held a dangling symlink:\n%s", want, out)
		}
	}
}
