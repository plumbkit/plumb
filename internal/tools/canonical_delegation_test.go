package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
)

// canonical_delegation_test.go — issue #273. The boundary seam and workspace-root
// identity must answer "are these the same place?" with ONE function.
//
// Before the fold there were two, and they disagreed on the two cases that
// caused the bugs each was written for: canonicalRoot anchored a relative path
// to the daemon's working directory (issue #181's silent cross-repository
// write), and it Cleaned ".." lexically before resolving symlinks (issue #264's
// check-and-syscall divergence). paths.Canonical refuses both.
//
// These assert canonicalRoot's OBSERVABLE agreement with paths.Canonical rather
// than that it literally calls it, so the property survives a reimplementation.

// TestCanonicalRoot_DoesNotAnchorRelativePaths is the #181 guard at the boundary
// seam. A relative path names no location; anchoring it to the daemon's working
// directory — a singleton process whose cwd belongs to whichever client happened
// to spawn it — is how plumb once wrote into an unrelated repository.
func TestCanonicalRoot_DoesNotAnchorRelativePaths(t *testing.T) {
	for _, in := range []string{"proj", "./proj", "a/../proj"} {
		got := canonicalRoot(in)
		if filepath.IsAbs(got) {
			t.Errorf("canonicalRoot(%q) = %q — a relative path must not be given a location", in, got)
		}
		if want := paths.Canonical(in); got != want {
			t.Errorf("canonicalRoot(%q) = %q, paths.Canonical = %q — the two canonicalisers disagree", in, got, want)
		}
	}
}

// TestCanonicalRoot_ResolvesDotDotInKernelOrder is the #264 divergence one layer
// down. filepath.Abs Cleans, collapsing "link/.." as a pair; the kernel follows
// `link` first and applies ".." to wherever that landed. When the link's target
// lives under a different parent the two name DIFFERENT directories, so a
// canonicaliser that Cleans first reports two distinct places as the same.
//
// This is reachable at the seam even with #264's refusal in place: the refusal
// guards Check and PathWithinWorkspace, but NewPathPolicy canonicalises its
// roots with no refusal above it.
func TestCanonicalRoot_ResolvesDotDotInKernelOrder(t *testing.T) {
	base := evalTempDir(t)
	mustMkdirAllT(t, filepath.Join(base, "a", "x"))
	mustMkdirAllT(t, filepath.Join(base, "b", "target"))
	mustMkdirAllT(t, filepath.Join(base, "b", "x"))
	mustSymlinkT(t, filepath.Join(base, "b", "target"), filepath.Join(base, "a", "link"))

	// Concatenated, not filepath.Join: Join Cleans, which would collapse the ".."
	// before canonicalRoot ever saw it — the exact mistake under test.
	sep := string(filepath.Separator)
	in := base + sep + "a" + sep + "link" + sep + ".." + sep + "x"

	got := canonicalRoot(in)
	want := filepath.Join(base, "b", "x")
	lexical := filepath.Join(base, "a", "x")
	if got == lexical {
		t.Fatalf("canonicalRoot collapsed %q lexically to %q; the kernel reaches %q", in, lexical, want)
	}
	if got != want {
		t.Errorf("canonicalRoot = %q, want %q", got, want)
	}
	if canon := paths.Canonical(in); got != canon {
		t.Errorf("canonicalRoot = %q, paths.Canonical = %q — the two canonicalisers disagree", got, canon)
	}
}

// TestCanonicalRoot_AgreesWithPathsCanonical sweeps the ordinary shapes as well,
// so the fold cannot be satisfied by special-casing the two cases above.
func TestCanonicalRoot_AgreesWithPathsCanonical(t *testing.T) {
	base := evalTempDir(t)
	mustMkdirAllT(t, filepath.Join(base, "real", "sub"))
	mustSymlinkT(t, filepath.Join(base, "real"), filepath.Join(base, "alias"))

	for _, in := range []string{
		base,
		filepath.Join(base, "real", "sub"),
		filepath.Join(base, "alias", "sub"),
		filepath.Join(base, "alias", "not", "created", "yet"),
		filepath.Join(base, "real") + string(filepath.Separator),
		"",
	} {
		if got, want := canonicalRoot(in), paths.Canonical(in); got != want {
			t.Errorf("canonicalRoot(%q) = %q, paths.Canonical = %q", in, got, want)
		}
	}
}

// TestCanonicalRoot_DecodesFileURIs pins the one thing the seam adds on top of
// paths.Canonical: tool arguments arrive as either a path or a file:// URI, and
// the URI form must canonicalise to the same place. paths.Canonical deliberately
// does not know about URIs — it is called with paths from inside the daemon too.
func TestCanonicalRoot_DecodesFileURIs(t *testing.T) {
	base := evalTempDir(t)
	mustMkdirAllT(t, filepath.Join(base, "proj"))
	dir := filepath.Join(base, "proj")

	if got, want := canonicalRoot("file://"+dir), canonicalRoot(dir); got != want {
		t.Errorf("canonicalRoot(file://%s) = %q, want %q", dir, got, want)
	}
}

// TestPathPolicy_RelativePathGrantsNothing pins the consequence of not anchoring,
// which is the whole reason the fold is safe: a relative path that reaches the
// policy matches no root and is refused, rather than being resolved against the
// daemon's cwd and silently admitted because that cwd happened to sit inside an
// allowed root. Fail closed — a refused call is recoverable, a misplaced write is
// not.
func TestPathPolicy_RelativePathGrantsNothing(t *testing.T) {
	ws := evalTempDir(t)
	pol := NewPathPolicy(ws, []AllowedRoot{{Path: ws, Access: AccessReadWrite, Label: "workspace"}})

	if _, err := pol.Check("relative.txt", AccessReadWrite); err == nil {
		t.Error("the policy admitted a relative path, which names no location")
	}
	// Control: the absolute form of the same file is admitted, so the refusal
	// above is about the spelling and not a policy that denies everything.
	if _, err := pol.Check(filepath.Join(ws, "relative.txt"), AccessReadWrite); err != nil {
		t.Errorf("control failed — an ordinary in-workspace path was refused: %v", err)
	}
}

// TestNewPathPolicy_DropsRelativeRoots: a root that names no location cannot be
// honoured, and anchoring it to the daemon cwd is the #181 hazard. It is dropped
// at construction, where the decision is visible, rather than silently matching
// nothing later.
func TestNewPathPolicy_DropsRelativeRoots(t *testing.T) {
	ws := evalTempDir(t)
	pol := NewPathPolicy(ws, []AllowedRoot{
		{Path: ws, Access: AccessReadWrite, Label: "workspace"},
		{Path: "relative/root", Access: AccessReadWrite, Label: "configured"},
	})

	if got := len(pol.roots); got != 1 {
		t.Errorf("relative root was kept: %d roots, want 1 (%+v)", got, pol.roots)
	}
	// It must not grant access under either spelling.
	if _, err := pol.Check("relative/root/file.txt", AccessReadWrite); err == nil {
		t.Error("a relative root granted access to a relative path")
	}
	abs, err := filepath.Abs("relative/root/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pol.Check(abs, AccessReadWrite); err == nil {
		t.Errorf("a relative root granted access to %s, resolved against the daemon's cwd", abs)
	}
}

func mustMkdirAllT(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
}

func mustSymlinkT(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
}
