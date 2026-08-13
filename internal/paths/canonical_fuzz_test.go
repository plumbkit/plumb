package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonical_fuzz_test.go — path canonicalisation, fuzzed against the KERNEL.
//
// TODO_IMPROVE_3 §9 names path canonicalisation as a parser taking
// attacker-influenced input with no fuzz coverage. Canonical answers "are these
// two paths the same place", and every boundary decision above it inherits that
// answer, so a disagreement with the filesystem is a disagreement the policy
// layer cannot see.
//
// The oracle is the filesystem, not a second string implementation. For a path
// that exists, Canonical(p) must name THE SAME FILE as p — asserted with
// os.SameFile, which compares inode identity rather than text. That is the
// property #264 broke: filepath.Abs Cleans `sub/..` lexically before any symlink
// resolves, while the kernel resolves `sub` first, so the canonical form named a
// different file from the one a syscall on p would reach.
//
// Two further properties, both cheap and both real:
//
//   - IDEMPOTENCE. Canonical(Canonical(p)) == Canonical(p). A canonicaliser that
//     moves on the second application has no fixed point, so callers that
//     canonicalise at different depths (the pool key, the boundary check, a
//     stats row) can disagree about one path.
//   - ABSOLUTENESS IS PRESERVED. An absolute input must stay absolute. A relative
//     result would silently start resolving against the process working
//     directory at the point of use, which for a daemon serving several
//     workspaces is not a place any caller means.
//
// Canonical is documented as an identity function, NOT an authorisation check —
// it deliberately does not refuse anything. So this target asserts identity
// properties only; the refusal behaviour belongs to the boundary policy and is
// covered by internal/tools' own kernel-differential target.

func FuzzCanonicalAgreesWithKernel(f *testing.F) {
	f.Add("a")
	f.Add("a/b")
	f.Add("link")
	f.Add("link/b")
	f.Add("link/..")
	f.Add("a/../b")
	f.Add("a/./b")
	f.Add("./a")
	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("a//b")
	f.Add("missing/deeper/still")
	f.Add("link/../a")
	f.Add("loop")
	f.Add("a/b/../../a/b")
	f.Add(strings.Repeat("a/", 40) + "b")

	f.Fuzz(func(t *testing.T, rel string) {
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Skip()
		}
		// A small real filesystem: a directory, a file, a symlink to each, and a
		// dangling link. The interesting inputs are the ones that traverse them.
		mustMk(t, filepath.Join(base, "a"))
		mustMk(t, filepath.Join(base, "a", "b"))
		if err := os.WriteFile(filepath.Join(base, "a", "f"), []byte("x"), 0o600); err != nil {
			t.Skip()
		}
		if err := os.Symlink(filepath.Join(base, "a"), filepath.Join(base, "link")); err != nil {
			t.Skip() // no symlink support on this platform
		}
		_ = os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(base, "dangling"))
		loop := filepath.Join(base, "loop")
		_ = os.Symlink(loop, loop)

		p := filepath.Join(base, rel)
		got := Canonical(p)

		// Property 1 — the kernel differential. Only meaningful when the path
		// exists: for a path that does not, there is no file to compare against and
		// Canonical's contract is only that it resolves what it can.
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			cfi, cerr := os.Stat(got)
			ofi, oerr := os.Stat(p)
			if cerr == nil && oerr == nil && !os.SameFile(cfi, ofi) {
				t.Errorf("Canonical named a DIFFERENT FILE than the path it was given:\n"+
					"  in:  %q\n  out: %q\n"+
					"every boundary decision above this inherits the wrong answer", p, got)
			}
		}

		// Property 2 — idempotence.
		if twice := Canonical(got); twice != got {
			t.Errorf("Canonical has no fixed point:\n  once:  %q\n  twice: %q\n"+
				"callers that canonicalise at different depths will disagree about one path", got, twice)
		}

		// Property 3 — an absolute input stays absolute.
		if filepath.IsAbs(p) && got != "" && !filepath.IsAbs(got) {
			t.Errorf("Canonical(%q) = %q, which is relative — it would resolve against "+
				"the process working directory at the point of use", p, got)
		}
	})
}

func mustMk(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestCanonical_SameFileAsKernelForSymlinkedParent pins the #264 shape as an
// ordinary case: a path reached THROUGH a symlinked parent must canonicalise to
// the file the kernel reaches, not to the lexical reading.
func TestCanonical_SameFileAsKernelForSymlinkedParent(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustMk(t, filepath.Join(base, "real"))
	if err := os.WriteFile(filepath.Join(base, "real", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "via")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	through := filepath.Join(base, "via", "f")
	direct := filepath.Join(base, "real", "f")
	if got, want := Canonical(through), Canonical(direct); got != want {
		t.Errorf("two spellings of one file canonicalise apart:\n  %q\n  %q", got, want)
	}
	a, err := os.Stat(Canonical(through))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(direct)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Error("the canonical form of the symlinked spelling is not the same file")
	}
}
