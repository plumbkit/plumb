package topology

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestStore_ToRelativeAcceptsAnAliasedPath guards the quietest failure in the
// issue #263 family, and the one a review caught after the rest of the fix was
// already written.
//
// Store.workspace is the canonical root the pool resolved. The paths reaching
// toRelative come from the AGENT — file_outline, workspace_symbols,
// topology_affected and the peer-activity annotation all pass a caller-supplied
// path straight through. When those two spellings disagree, toRelative hands the
// absolute path to `WHERE f.path = ?`, which matches nothing: every topology
// query for the project returns empty, with no error and nothing in the log.
func TestStore_ToRelativeAcceptsAnAliasedPath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	realRoot := filepath.Join(base, "real", "proj")
	if err := os.MkdirAll(filepath.Join(realRoot, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias", "proj")

	s, err := Open(realRoot, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The same file, named by each spelling, must produce the one relative path
	// the index actually stores.
	const want = "internal/a.go"
	if got := s.toRelative(filepath.Join(realRoot, "internal", "a.go")); got != want {
		t.Errorf("toRelative(resolved) = %q, want %q", got, want)
	}
	got := s.toRelative(filepath.Join(alias, "internal", "a.go"))
	if filepath.IsAbs(got) {
		t.Fatalf("toRelative(aliased) = %q — an absolute path here matches no indexed "+
			"row, so every topology query for this project silently returns empty", got)
	}
	if got != want {
		t.Errorf("toRelative(aliased) = %q, want %q", got, want)
	}

	// A path genuinely outside the workspace must still come back untouched.
	outside := filepath.Join(base, "elsewhere", "a.go")
	if got := s.toRelative(outside); got != outside {
		t.Errorf("toRelative(outside) = %q, want it returned unchanged (%q)", got, outside)
	}

	// An already-relative path — what the fswatcher hands Enqueue for every
	// filesystem event — comes straight back.
	if got := s.toRelative("internal/a.go"); got != "internal/a.go" {
		t.Errorf("toRelative(relative) = %q, want it returned unchanged", got)
	}
}

// BenchmarkToRelative_RelativeInput exists because the guard it measures cannot
// be unit-tested: with or without `if !filepath.IsAbs(path)`, a relative path
// comes back unchanged, so only the syscall count differs. Without the guard
// WorkspaceRel resolves the workspace root with EvalSymlinks on every call before
// failing both passes — an lstat chain per filesystem event, on the single
// watcher-consumer goroutine. Measured at ~1.8 ns guarded against ~5400 ns
// unguarded on a real workspace root; if this ever reads in microseconds again,
// the guard has been removed.
func BenchmarkToRelative_RelativeInput(b *testing.B) {
	dir, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	s, err := Open(dir, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, nil)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	for b.Loop() {
		_ = s.toRelative("internal/auth/token.go")
	}
}
