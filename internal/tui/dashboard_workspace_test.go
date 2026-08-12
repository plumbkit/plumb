package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectWorkspaceFolder_ResolvesAnAliasedCwd is the guard a third review
// found missing: the canonicalisation in detectWorkspaceFolder was the one
// production line on this branch with no test, and reverting it left the whole
// internal/tui package green.
//
// The value is compared against session.Info.Folder, which the daemon's pool
// resolved. Launched from a cwd that reaches the project through a symlink — the
// everyday macOS /tmp case, or a symlinked checkout — an un-canonicalised answer
// does not match, and the memory workspace picker lists the one project twice.
func TestDetectWorkspaceFolder_ResolvesAnAliasedCwd(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	realRoot := filepath.Join(base, "real", "proj")
	if err := os.MkdirAll(filepath.Join(realRoot, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias", "proj")
	if filepath.Clean(alias) == filepath.Clean(realRoot) {
		t.Fatalf("fixture is not aliased: both spellings clean to %q", realRoot)
	}

	// Enter the project by its aliased spelling, as a shell would.
	t.Chdir(alias)

	got := detectWorkspaceFolder()
	if got != realRoot {
		t.Errorf("detectWorkspaceFolder = %q, want the resolved root %q — an aliased "+
			"answer does not match session.Folder, so the project is listed twice", got, realRoot)
	}
}
