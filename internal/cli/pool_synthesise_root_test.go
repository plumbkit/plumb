package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
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
			got := pool.SynthesiseRoot(tc.seed, false)
			if got != filepath.Clean(got) {
				t.Errorf("SynthesiseRoot(%q) = %q, which is not canonical (Clean = %q)",
					tc.seed, got, filepath.Clean(got))
			}
		})
	}
}

// TestSynthesiseRoot_DoesNotEscapeToHome is the $HOME guard Detect has and this
// function did not.
//
// Detect skips its .git check at $HOME by filesystem identity, so a dotfiles
// repo checked out AT the home directory cannot capture every path beneath it.
// SynthesiseRoot walked up to the nearest .git unconditionally — so with
// [workspace] auto_attach = true, a tool call seeded anywhere under a $HOME
// dotfiles repo synthesised the workspace to $HOME itself. That makes the entire
// home directory one read-write root for the session: every SSH key, browser
// profile and credential file under it lands inside the boundary, and the
// project the user was actually working in stops being the unit of isolation.
//
// The CLI side dodged this by refusing to call SynthesiseRoot at all
// (workspaceRootForCLI, pool_detect.go). The daemon still calls it from four
// places — conn_attach, conn_persist, conn_repin, conn_roots — so the guard has
// to live in the function.
func TestSynthesiseRoot_DoesNotEscapeToHome(t *testing.T) {
	// freshTempDir, not t.TempDir: `make test` sets GOTMPDIR to .testcache INSIDE
	// the repo, so t.TempDir() sits under plumb's own .git and the walk finds that
	// instead of falling back to the seed. Bare `go test` does not set it, which
	// is why this passed locally and failed on every CI job.
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	mustMkdir(t, filepath.Join(home, ".git"))
	seed := filepath.Join(home, "scratch", "markerless")
	mustMkdir(t, seed)

	// Precondition: the fixture really is the dangerous shape — a .git at $HOME
	// with no marker anywhere between it and the seed. Without this the test
	// could pass because the walk never had anything to find.
	if _, err := os.Stat(filepath.Join(home, ".git")); err != nil {
		t.Fatalf("fixture: no .git at the fake $HOME: %v", err)
	}

	got := (&workspacePool{}).SynthesiseRoot(seed, false)
	if sameDirAs(got, homeDirInfos()) {
		t.Errorf("SynthesiseRoot(%q) escaped to $HOME (%q); a dotfiles repo at the home "+
			"directory must not become the workspace for everything beneath it", seed, got)
	}
	if want := paths.Canonical(seed); got != want {
		t.Errorf("SynthesiseRoot(%q) = %q, want the seed %q", seed, got, want)
	}
}

// TestSynthesiseRoot_HomeAsTheSeedNeedsAnExplicitDeclaration is the two sides
// of the $HOME-seed rule.
//
// EXPLICIT honours it: an explicit session_start pin must always succeed
// (issue #182's contract), and a user who points plumb at their home directory
// has declared that intent.
//
// Non-explicit refuses it: the first cut of this guard exempted any seed whose
// STRING was $HOME, but the seed reaching SynthesiseRoot is fed by
// seedPathFromArgs — uri / file_path / path — so a single read_file of
// ~/.zshrc seeded $HOME and pinned the entire home directory. Reading a
// dotfile is not a declaration of intent; only the caller's stated origin is.
func TestSynthesiseRoot_HomeAsTheSeedNeedsAnExplicitDeclaration(t *testing.T) {
	home := freshTempDir(t) // see the GOTMPDIR note above
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(home, ".git"))

	if got, want := (&workspacePool{}).SynthesiseRoot(home, true), paths.Canonical(home); got != want {
		t.Errorf("SynthesiseRoot(%q, explicit) = %q, want %q — an explicitly named root must be honoured", home, got, want)
	}
	if got := (&workspacePool{}).SynthesiseRoot(home, false); got != "" {
		t.Errorf("SynthesiseRoot(%q, non-explicit) = %q, want refusal — an incidental seed (a tool path, a client root, a replayed non-explicit pin) must not name the home directory as the workspace", home, got)
	}
	// The refusal is about $HOME's identity, not its spelling.
	if got := (&workspacePool{}).SynthesiseRoot(home+string(filepath.Separator), false); got != "" {
		t.Errorf("SynthesiseRoot(%q, non-explicit) = %q, want refusal — a trailing-slash spelling must not slip past", home+string(filepath.Separator), got)
	}
}

// TestSynthesiseRoot_ReachingHomeStopsTheWalk: the guard must TERMINATE the
// ascent at $HOME, not merely skip its .git check and keep climbing. With a
// .git ABOVE the home directory the skip-and-continue shape returned $HOME's
// PARENT — a root strictly WIDER than the $HOME escape the guard was built to
// block. Reaching $HOME falls back to the seed, whatever sits above.
func TestSynthesiseRoot_ReachingHomeStopsTheWalk(t *testing.T) {
	base := freshTempDir(t)                   // see the GOTMPDIR note above
	mustMkdir(t, filepath.Join(base, ".git")) // a repo ABOVE the home directory
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	seed := filepath.Join(home, "scratch", "markerless")
	mustMkdir(t, seed)

	got := (&workspacePool{}).SynthesiseRoot(seed, false)
	if got == paths.Canonical(base) || sameDirAs(got, homeDirInfos()) {
		t.Fatalf("SynthesiseRoot(%q) = %q — the walk climbed to or past $HOME; reaching the home directory must stop it", seed, got)
	}
	if want := paths.Canonical(seed); got != want {
		t.Errorf("SynthesiseRoot(%q) = %q, want the seed %q", seed, got, want)
	}
}

// TestSynthesiseRoot_HomeGuardIsByIdentityNotByString pins WHY the guard uses
// os.SameFile rather than comparing strings against $HOME.
//
// Found by mutation: swapping sameDirAs for `d != os.UserHomeDir()` left every
// other test in this file green, so nothing was holding the identity comparison
// in place. The walk cleans its path but never resolves symlinks, so the moment
// $HOME is reached under a second spelling — a symlinked path, or the macOS
// /var -> /private/var firmlink that every t.TempDir() sits behind — a string
// compare does not recognise it and the escape reopens.
//
// Layout: HOME is the real directory; the seed is named through a symlink to it,
// so the two spellings differ but denote the same inode.
func TestSynthesiseRoot_HomeGuardIsByIdentityNotByString(t *testing.T) {
	base := freshTempDir(t) // see the GOTMPDIR note above
	home := filepath.Join(base, "home")
	mustMkdir(t, filepath.Join(home, ".git"))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	alias := filepath.Join(base, "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	seed := filepath.Join(alias, "scratch", "markerless")
	mustMkdir(t, seed)

	// Precondition: the two spellings really do differ as strings, or a string
	// compare would pass here and the test would prove nothing.
	if alias == home {
		t.Fatal("fixture: alias and home are the same string")
	}

	got := (&workspacePool{}).SynthesiseRoot(seed, false)
	if sameDirAs(got, homeDirInfos()) {
		t.Errorf("SynthesiseRoot(%q) escaped to $HOME (%q) — $HOME was named through a "+
			"symlink, so a string compare missed it; the guard must compare by "+
			"filesystem identity", seed, got)
	}
}

// TestSynthesiseRoot_WalkStopsAtDirContainingHome holds the IN-LOOP half of the
// containment guard, which round 6 found surviving deletion untested. The
// top-of-function refusal only judges the SEED; the walk needs its own stop,
// because a markerless SIBLING of the home directory passes the seed check and
// then ascends — and with a .git above the home directory's parent, the walk
// would hand back a root that contains the home directory even though the seed
// never did. Reaching a dir that contains a home directory falls back to the
// seed, exactly as reaching $HOME itself does.
func TestSynthesiseRoot_WalkStopsAtDirContainingHome(t *testing.T) {
	base := freshTempDir(t)                   // see the GOTMPDIR note above
	mustMkdir(t, filepath.Join(base, ".git")) // a repo above the home's parent
	users := filepath.Join(base, "users")     // stands in for /Users
	home := filepath.Join(users, "alice")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	seed := filepath.Join(users, "work") // markerless SIBLING of the home directory
	mustMkdir(t, seed)

	got := (&workspacePool{}).SynthesiseRoot(seed, false)
	if got == paths.Canonical(base) {
		t.Fatalf("SynthesiseRoot(%q) = %q — the walk ascended through %q, which contains the home directory, and widened the root to it", seed, got, users)
	}
	if want := paths.Canonical(seed); got != want {
		t.Errorf("SynthesiseRoot(%q) = %q, want the seed %q", seed, got, want)
	}
}

// TestSynthesiseRoot_GitBelowHomeStillWins is the control: the guard is about
// $HOME specifically, not about .git under it. An ordinary project checked out
// inside the home directory — which is where most projects live — must still
// resolve to the project, or the guard would have broken the common case.
func TestSynthesiseRoot_GitBelowHomeStillWins(t *testing.T) {
	home := freshTempDir(t) // see the GOTMPDIR note above
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(home, ".git")) // dotfiles repo at $HOME, as above
	proj := filepath.Join(home, "Projects", "realproj")
	mustMkdir(t, filepath.Join(proj, ".git"))
	seed := filepath.Join(proj, "internal", "deep")
	mustMkdir(t, seed)

	if got, want := (&workspacePool{}).SynthesiseRoot(seed, false), paths.Canonical(proj); got != want {
		t.Errorf("SynthesiseRoot(%q) = %q, want the project root %q", seed, got, want)
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
	ws := (&workspacePool{}).SynthesiseRoot(filepath.Join(projRoot, "sub")+"/..", false)

	pol := tools.NewPathPolicy(ws, []tools.AllowedRoot{
		{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"},
	})
	if _, err := pol.Check(ws, tools.AccessRead); err != nil {
		t.Errorf("a policy refused its own workspace root %q: %v", ws, err)
	}
}
