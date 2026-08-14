package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
)

// pool_synthesise_root_fuzz_test.go — workspace-root synthesis, fuzzed against
// the kernel. The last of TODO_IMPROVE_3 §9's six parsers to get a target,
// deferred until #288 stopped changing SynthesiseRoot.
//
// SynthesiseRoot decides, for a markerless directory, how far up the tree a
// session's read-write boundary extends. A wrong answer is not an error the
// caller sees — it is a WIDER ROOT: the dotfiles-at-$HOME shape closed by #288
// made every credential under the home directory part of one workspace. So
// every property below is about width, and the oracle for every one of them is
// the filesystem, not a second string implementation:
//
//   - REFUSAL. A seed that IS a home directory — by identity, reached through
//     a symlinked spelling too — must be refused ("") unless the caller
//     declared it explicit; an explicit declaration must be honoured.
//   - NEVER HOME. A synthesised root must never BE the home directory by
//     filesystem identity, whatever spelling the input reached it through.
//   - CONTAINMENT. The root must be the seed or an ancestor of it, compared by
//     inode identity up the seed's own chain. A walk that jumps sideways or
//     above the seed names a directory nothing on the input's chain pins.
//   - CANONICAL FIXED POINT. EvalSymlinks must be a no-op on the output and
//     Clean must not move it — a root two callers spell differently is the
//     #263 defect again.
//   - NEAREST-REPO DIFFERENTIAL. An independent kernel walk — stat .git at
//     each level of the seed's chain, stopping at the first home identity —
//     must agree with the level production stopped at. Stopping WIDER than the
//     nearest repo is the confinement failure; the mirror direction catches a
//     walk that stops too early and pins the seed where a repo boundary
//     exists.
//
// The last property mirrors the documented contract, so it cannot catch a
// defect the contract itself carries; the four above it are independent
// oracles and exist for what the mirror cannot see. One limit is shared by all
// five and disclosed rather than hidden: the home guard compares by IDENTITY
// (equality), not containment. Issue #306 records why the containment form was
// built, defeated across seven rounds of adversarial review on macOS
// path-aliasing shapes, and removed — so a seed ABOVE the home directory that
// finds its own .git still synthesises to that ancestor. Current behaviour,
// not asserted here, tracked there.
func FuzzSynthesiseRootAgainstKernel(f *testing.F) {
	// Layout bits: every dangerous shape is independently reachable, and the
	// all-zero layout exercises the no-.git-anywhere fallback.
	const (
		gitAtBase = 1 << iota // .git at the fixture base: a repo ABOVE the home dir
		gitAtHome             // dotfiles repo checked out AT $HOME (#288's shape)
		homeAlias             // second, symlinked spelling of $HOME
		gitAtProj             // .git on a project BELOW $HOME — must still win
		gitAtRepo             // .git on a project outside $HOME — the ordinary case
		asideLink             // symlink reaching DEEP across the tree, so ".." after it diverges lexically
	)

	f.Add("home/scratch/markerless", byte(gitAtHome), false)
	f.Add("home-alias/scratch/markerless", byte(gitAtHome|homeAlias), false)
	f.Add("home-alias/scratch/..", byte(gitAtHome|homeAlias), false)
	f.Add("home/scratch/markerless", byte(gitAtBase), false)
	f.Add("home/proj/internal/deep", byte(gitAtHome|gitAtProj), false)
	f.Add("repo/a/b", byte(gitAtRepo), false)
	f.Add("aside/deep/..", byte(gitAtProj|asideLink), false)
	f.Add("home", byte(gitAtHome), false)
	f.Add("home-alias", byte(gitAtHome|homeAlias), false)
	f.Add("home-alias", byte(gitAtHome|homeAlias|gitAtBase), false)
	f.Add("home", byte(gitAtHome), true)
	f.Add("home/", byte(gitAtHome), false)
	f.Add("other/deep/none", byte(0), false)
	f.Add("../../elsewhere", byte(0), false)
	f.Add(".", byte(0), false)

	f.Fuzz(func(t *testing.T, rel string, layout byte, explicit bool) {
		base := freshTempDir(t) // NOT t.TempDir: see the GOTMPDIR note there
		home := filepath.Join(base, "home")
		mustMkdir(t, home)
		mustMkdir(t, filepath.Join(home, "scratch"))
		mustMkdir(t, filepath.Join(home, "proj"))
		mustMkdir(t, filepath.Join(base, "repo"))
		mustMkdir(t, filepath.Join(base, "other"))
		if layout&gitAtBase != 0 {
			mustMkdir(t, filepath.Join(base, ".git"))
		}
		if layout&gitAtHome != 0 {
			mustMkdir(t, filepath.Join(home, ".git"))
		}
		if layout&gitAtProj != 0 {
			mustMkdir(t, filepath.Join(home, "proj", ".git"))
		}
		if layout&gitAtRepo != 0 {
			mustMkdir(t, filepath.Join(base, "repo", ".git"))
		}
		if layout&homeAlias != 0 {
			if err := os.Symlink(home, filepath.Join(base, "home-alias")); err != nil {
				t.Skipf("symlinks unsupported on this filesystem: %v", err)
			}
		}
		if layout&asideLink != 0 {
			if err := os.Symlink(filepath.Join(home, "proj"), filepath.Join(base, "aside")); err != nil {
				t.Skipf("symlinks unsupported on this filesystem: %v", err)
			}
		}
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)

		// String concatenation, NOT filepath.Join: Join cleans, and cleaning
		// rewrites exactly the raw traversal shapes under test.
		seed := base + string(filepath.Separator) + rel
		got := (&workspacePool{}).SynthesiseRoot(seed, explicit)

		homeInfos := homeDirInfos()
		cseed := filepath.Clean(seed)
		fallback := paths.Canonical(cseed)

		// Property 1 — refusal, and its converse.
		if sameDirAs(cseed, homeInfos) {
			if !explicit && got != "" {
				t.Errorf("SynthesiseRoot(%q, non-explicit) = %q, want refusal — an incidental seed "+
					"(a tool path, a client root, a replayed non-explicit pin) must not name the home "+
					"directory as the workspace", seed, got)
			}
			if explicit && !sameDirAs(got, homeInfos) {
				t.Errorf("SynthesiseRoot(%q, explicit) = %q, want the declared home root — an explicit "+
					"pin must always be honoured (issue #182)", seed, got)
			}
			return
		}

		// Property 2 — never home, by identity rather than spelling.
		if got != "" && sameDirAs(got, homeInfos) {
			t.Errorf("SynthesiseRoot(%q) = %q, which IS the home directory by filesystem identity — "+
				"a dotfiles repo at $HOME must not capture everything beneath it", seed, got)
		}

		// Property 3 — containment: the root is the seed or an ancestor of it.
		// Both sides are compared as CANONICAL spellings — got comes out of
		// paths.Canonical and the chain walked here starts at Canonical(seed) —
		// because the seed may not exist (a synthesised root often names a
		// not-yet-created directory), and for paths that do not exist there is
		// no inode to compare. On resolved spellings lexical ancestry IS real
		// ancestry; on the raw seed's spelling it is not (the alias shape),
		// which is why the chain starts from the canonicalised seed.
		if got != "" {
			d := fallback
			contained := false
			for {
				if got == d {
					contained = true
					break
				}
				parent := filepath.Dir(d)
				if parent == d {
					break
				}
				d = parent
			}
			if !contained {
				t.Errorf("SynthesiseRoot(%q) = %q, which is neither the canonical seed nor an "+
					"ancestor of it — the walk named a directory nothing on the seed's chain pins", seed, got)
			}
		}

		// Property 4 — the output is canonical: Clean does not move it, and
		// EvalSymlinks is a no-op on it when it exists. A root that still
		// carries an unresolved symlink is one two callers will spell
		// differently, and boundary checks are string comparisons.
		if got != "" {
			if !filepath.IsAbs(got) || filepath.Clean(got) != got {
				t.Errorf("SynthesiseRoot(%q) = %q, which is not an absolute cleaned path", seed, got)
			}
			if _, err := os.Stat(got); err == nil {
				if resolved, err := filepath.EvalSymlinks(got); err == nil && resolved != got {
					t.Errorf("SynthesiseRoot(%q) = %q still carries an unresolved symlink (resolves to "+
						"%q) — two spellings of one root then compare unequal", seed, got, resolved)
				}
			}
		}

		// Property 5 — the nearest-repo differential: an independent kernel
		// walk over the same contract must stop at the same level.
		if want := nearestGitOrFallback(cseed, homeInfos, fallback); !sameFileOrString(got, want) {
			t.Errorf("SynthesiseRoot(%q) = %q, but the kernel walk finds %q — the walk stopped at a "+
				"different level than the nearest .git below the first home directory", seed, got, want)
		}
	})
}

// nearestGitOrFallback is the property-5 oracle: walk the seed's chain with
// the kernel (os.Stat resolves each level's .git through any symlink), stop at
// the first directory that IS a home by identity, and fall back to the seed at
// both the home stop and the filesystem root — the documented contract,
// spelled independently of the production loop.
func nearestGitOrFallback(seed string, homeInfos []os.FileInfo, fallback string) string {
	d := filepath.Clean(seed)
	for {
		if sameDirAs(d, homeInfos) {
			return fallback
		}
		parent := filepath.Dir(d)
		if parent == d {
			return fallback
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		d = parent
	}
}

// sameFileOrString compares two paths by inode identity when both exist, and
// falls back to string equality when either does not (a synthesised root may
// legitimately name a not-yet-created seed).
func sameFileOrString(a, b string) bool {
	af, aerr := os.Stat(a)
	bf, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(af, bf)
	}
	return a == b
}
