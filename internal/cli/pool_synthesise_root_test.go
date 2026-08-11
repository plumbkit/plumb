package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

// TestSynthesiseRoot_ReturnsACanonicalPath pins that a synthesised workspace
// root is in canonical form.
//
// It returned the seed VERBATIM on the filesystem-root fallback, so an explicit
// markerless pin — `session_start({workspace: "<path>/sub/.."})`, which is
// supported and always succeeds — stored a root containing "..". Several tools
// boundary-check that raw workspace string rather than a path derived from it
// (list_memories, read_memory, write_memory, delete_memory, search_memories,
// relevant_memories, topology_status, and git's default repo), so once
// PathPolicy.Check began refusing an unresolved "..", those tools refused for
// the whole session — with a message telling the caller to pass a different
// path, which it cannot do, because it never named one.
func TestSynthesiseRoot_ReturnsACanonicalPath(t *testing.T) {
	base := t.TempDir()
	projRoot := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(projRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	pool := &workspacePool{}

	for _, tc := range []struct{ name, seed string }{
		{"traversal in the seed", filepath.Join(projRoot, "sub") + "/.."},
		{"trailing dot segment", projRoot + "/."},
		{"already canonical", projRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pool.SynthesiseRoot(tc.seed)
			if got != filepath.Clean(got) {
				t.Errorf("SynthesiseRoot(%q) = %q, which is not canonical (Clean = %q)",
					tc.seed, got, filepath.Clean(got))
			}
		})
	}
}

// TestPathPolicy_AdmitsItsOwnRoot is the invariant the regression violated: a
// session must never be refused access to the workspace it is pinned to. Tools
// that take no path argument check the workspace string itself, so a root the
// policy rejects makes them permanently unusable.
func TestPathPolicy_AdmitsItsOwnRoot(t *testing.T) {
	base := t.TempDir()
	projRoot := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(projRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := (&workspacePool{}).SynthesiseRoot(filepath.Join(projRoot, "sub") + "/..")

	pol := tools.NewPathPolicy(ws, []tools.AllowedRoot{
		{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"},
	})
	if _, err := pol.Check(ws, tools.AccessRead); err != nil {
		t.Errorf("a policy refused its own workspace root %q: %v", ws, err)
	}
}
