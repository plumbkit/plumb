package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// canonicalTempDir returns a temp directory already resolved, so a test can
// assert on exact strings. On macOS t.TempDir() sits under /var, itself a
// symlink to /private/var — the very aliasing these tests are about.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(TempDir): %v", err)
	}
	return dir
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink(%s -> %s): %v", link, target, err)
	}
}

// TestCanonical_TwoSpellingsOfOneDirectoryAgree is issue #263 in miniature: the
// whole point of the function is that a path reached through a symlink and the
// same path reached directly produce ONE string.
func TestCanonical_TwoSpellingsOfOneDirectoryAgree(t *testing.T) {
	base := canonicalTempDir(t)
	realDir := filepath.Join(base, "real", "proj")
	mustMkdirAll(t, realDir)
	mustSymlink(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))

	viaAlias := Canonical(filepath.Join(base, "alias", "proj"))
	viaReal := Canonical(realDir)
	if viaAlias != viaReal {
		t.Fatalf("spellings disagree: alias=%q resolved=%q", viaAlias, viaReal)
	}
	if viaAlias != realDir {
		t.Errorf("Canonical = %q, want %q", viaAlias, realDir)
	}
}

// TestCanonical_MissingPathResolvesAgainstExistingAncestor covers a workspace
// root that does not exist yet (an explicit session_start naming a directory
// about to be created) reached through a symlinked parent. Resolving only
// existing paths would leave this case un-canonicalised — and it is exactly the
// case where two agents would then disagree about where they are.
func TestCanonical_MissingPathResolvesAgainstExistingAncestor(t *testing.T) {
	base := canonicalTempDir(t)
	mustMkdirAll(t, filepath.Join(base, "real"))
	mustSymlink(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))

	got := Canonical(filepath.Join(base, "alias", "not", "created", "yet"))
	want := filepath.Join(base, "real", "not", "created", "yet")
	if got != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}
}

// TestCanonical_DotDotResolvesInKernelOrderNotLexically guards the divergence
// found in PR #264: filepath.Clean collapses ".." lexically, but the kernel
// resolves a symlink FIRST and then applies "..". When the symlink's target
// lives under a different parent, the two answers name different directories —
// so canonicalising with Clean first would report two distinct places as the
// same. Layout: a/link -> b/target, so a/link/../x must resolve to b/x, never
// to a/x.
func TestCanonical_DotDotResolvesInKernelOrderNotLexically(t *testing.T) {
	base := canonicalTempDir(t)
	mustMkdirAll(t, filepath.Join(base, "a"))
	mustMkdirAll(t, filepath.Join(base, "b", "target"))
	mustMkdirAll(t, filepath.Join(base, "b", "x"))
	mustMkdirAll(t, filepath.Join(base, "a", "x"))
	mustSymlink(t, filepath.Join(base, "b", "target"), filepath.Join(base, "a", "link"))

	// Built by concatenation, not filepath.Join: Join Cleans, which would
	// collapse the ".." before Canonical ever saw it — the exact mistake this
	// test exists to catch in the production path.
	sep := string(filepath.Separator)
	in := base + sep + "a" + sep + "link" + sep + ".." + sep + "x"
	got := Canonical(in)
	want := filepath.Join(base, "b", "x")
	lexical := filepath.Join(base, "a", "x")
	if got == lexical {
		t.Fatalf("Canonical collapsed %q lexically to %q; the kernel reaches %q", in, lexical, want)
	}
	if got != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}
}

// TestCanonical_RelativePathIsCleanedNotAnchored is the issue #181 guard: a
// relative path names no location, and anchoring it to the daemon's working
// directory is how plumb once wrote into an unrelated repository. Canonical
// must clean it and hand it straight back.
func TestCanonical_RelativePathIsCleanedNotAnchored(t *testing.T) {
	for _, in := range []string{"proj", "./proj", "a/../proj", "a/b/../../proj"} {
		got := Canonical(in)
		if filepath.IsAbs(got) {
			t.Errorf("Canonical(%q) = %q — a relative path must not become absolute", in, got)
		}
		if got != "proj" {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, "proj")
		}
	}
}

// TestCanonical_MissingPathWithDotDotStillResolvesItsParent pins the decision
// made when the ".." handling was measured rather than assumed. An earlier draft
// refused the ancestor walk whenever the input contained "..", on the theory
// that a lexically-collapsed ".." made the answer untrustworthy. Measurement
// showed the refusal recovers nothing in the case it was aimed at (the lexical
// collapse has already happened, so guarded and unguarded produce the SAME
// string) while losing canonicalisation here, where the parent is an ordinary
// alias. So the walk always runs.
func TestCanonical_MissingPathWithDotDotStillResolvesItsParent(t *testing.T) {
	base := canonicalTempDir(t)
	mustMkdirAll(t, filepath.Join(base, "real", "sub"))
	mustSymlink(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))

	sep := string(filepath.Separator)
	in := base + sep + "alias" + sep + "sub" + sep + ".." + sep + "missing"
	got := Canonical(in)
	if want := filepath.Join(base, "real", "missing"); got != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}
}

func TestCanonical_EmptyAndIdempotent(t *testing.T) {
	if got := Canonical(""); got != "" {
		t.Errorf("Canonical(\"\") = %q, want empty", got)
	}
	base := canonicalTempDir(t)
	mustMkdirAll(t, filepath.Join(base, "real"))
	mustSymlink(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))
	for _, in := range []string{
		filepath.Join(base, "alias"),
		filepath.Join(base, "alias", "missing", "leaf"),
		"relative/path",
	} {
		once := Canonical(in)
		if twice := Canonical(once); twice != once {
			t.Errorf("Canonical not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}
