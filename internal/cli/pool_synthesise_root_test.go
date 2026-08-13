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
			got := pool.SynthesiseRoot(tc.seed)
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
	home := t.TempDir()
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

	got := (&workspacePool{}).SynthesiseRoot(seed)
	if sameDirAs(got, homeFileInfo()) {
		t.Errorf("SynthesiseRoot(%q) escaped to $HOME (%q); a dotfiles repo at the home "+
			"directory must not become the workspace for everything beneath it", seed, got)
	}
	if want := paths.Canonical(seed); got != want {
		t.Errorf("SynthesiseRoot(%q) = %q, want the seed %q", seed, got, want)
	}
}

// TestSynthesiseRoot_HomeAsTheSeedIsStillHonoured keeps the guard from
// overreaching. Refusing to ASCEND into $HOME is not the same as refusing $HOME
// when the caller named it: an explicit session_start pin must always succeed
// (issue #182's contract), and a user who points plumb at their home directory
// has declared that intent. Only the silent upward escape is blocked.
func TestSynthesiseRoot_HomeAsTheSeedIsStillHonoured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(home, ".git"))

	if got, want := (&workspacePool{}).SynthesiseRoot(home), paths.Canonical(home); got != want {
		t.Errorf("SynthesiseRoot(%q) = %q, want %q — an explicitly named root must be honoured", home, got, want)
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
	base := t.TempDir()
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

	got := (&workspacePool{}).SynthesiseRoot(seed)
	if sameDirAs(got, homeFileInfo()) {
		t.Errorf("SynthesiseRoot(%q) escaped to $HOME (%q) — $HOME was named through a "+
			"symlink, so a string compare missed it; the guard must compare by "+
			"filesystem identity", seed, got)
	}
}

// TestSynthesiseRoot_GitBelowHomeStillWins is the control: the guard is about
// $HOME specifically, not about .git under it. An ordinary project checked out
// inside the home directory — which is where most projects live — must still
// resolve to the project, or the guard would have broken the common case.
func TestSynthesiseRoot_GitBelowHomeStillWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(home, ".git")) // dotfiles repo at $HOME, as above
	proj := filepath.Join(home, "Projects", "realproj")
	mustMkdir(t, filepath.Join(proj, ".git"))
	seed := filepath.Join(proj, "internal", "deep")
	mustMkdir(t, seed)

	if got, want := (&workspacePool{}).SynthesiseRoot(seed), paths.Canonical(proj); got != want {
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
	ws := (&workspacePool{}).SynthesiseRoot(filepath.Join(projRoot, "sub") + "/..")

	pol := tools.NewPathPolicy(ws, []tools.AllowedRoot{
		{Path: ws, Access: tools.AccessReadWrite, Label: "workspace"},
	})
	if _, err := pol.Check(ws, tools.AccessRead); err != nil {
		t.Errorf("a policy refused its own workspace root %q: %v", ws, err)
	}
}
