package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// checkout_test.go builds git layouts BY HAND rather than by running git: the
// shapes under test are the link files git writes (a `.git` FILE's `gitdir:` line,
// a worktree git directory's `commondir` and `gitdir` back-link, a submodule git
// directory's `core.worktree`), and a fixture that spells them out is the only way
// to test a FORGED one — which is the half that matters. A real-git test would
// prove the parser reads what git writes and nothing about what a hostile clone
// can write.

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mainCheckout builds a repository at root with .git as a directory.
func mainCheckout(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// linkedWorktree builds the checkout at wt, linked to the repository whose .git
// directory is common. git writes the worktree's .git file pointing INTO
// <common>/worktrees/<id>, and that directory's `gitdir` back-link pointing at the
// worktree's own .git file.
func linkedWorktree(t *testing.T, common, id, wt string, relativeBackLink bool) {
	t.Helper()
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(common, "worktrees", id)
	write(t, filepath.Join(wt, ".git"), "gitdir: "+gitDir+"\n")
	write(t, filepath.Join(gitDir, "commondir"), "../..\n")
	back := filepath.Join(wt, ".git")
	if relativeBackLink {
		rel, err := filepath.Rel(gitDir, back)
		if err != nil {
			t.Fatal(err)
		}
		back = rel
	}
	write(t, filepath.Join(gitDir, "gitdir"), back+"\n")
}

// submodule builds the checkout at sub, whose git directory lives inside the
// superproject's .git/modules and whose config names the checkout it serves.
func submodule(t *testing.T, super, name, sub string, worktreeValue string) string {
	t.Helper()
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(super, ".git", "modules", name)
	write(t, filepath.Join(sub, ".git"), "gitdir: "+gitDir+"\n")
	write(t, filepath.Join(gitDir, "config"), "[core]\n\tbare = false\n\tworktree = "+worktreeValue+"\n")
	return gitDir
}

func TestSameProject_MainCheckoutAndItsLinkedWorktree(t *testing.T) {
	repo := mainCheckout(t, t.TempDir())
	wt := filepath.Join(t.TempDir(), "wt")
	linkedWorktree(t, filepath.Join(repo, ".git"), "wt", wt, false)

	if !SameProject(repo, wt) || !SameProject(wt, repo) {
		t.Error("a main checkout and its linked worktree are one project, either way round")
	}
	if !SameProject(repo, repo) {
		t.Error("a checkout is its own project")
	}
	// SameCommonDir is the read-time half: the worktree may be gone, its stamped
	// common directory may not.
	if c, ok := CheckoutAt(wt); !ok || !SameCommonDir(c.CommonDir, repo) || !SameCommonDir(c.CommonDir, wt) {
		t.Error("a stamped common directory must resolve against either checkout of the same repository")
	}
}

func TestSameProject_SiblingWorktrees(t *testing.T) {
	repo := mainCheckout(t, t.TempDir())
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	common := filepath.Join(repo, ".git")
	linkedWorktree(t, common, "a", a, false)
	linkedWorktree(t, common, "b", b, true) // relative back-link, as git also writes it

	if !SameProject(a, b) || !SameProject(b, a) {
		t.Error("two worktrees of one repository are one project: their common directories are equal")
	}
}

func TestSameProject_SubmoduleAndSuperproject(t *testing.T) {
	super := mainCheckout(t, t.TempDir())
	sub := filepath.Join(super, "sub")
	submodule(t, super, "sub", sub, "../../../sub")

	// The boundary the acceptance decision documents: the submodule checkout and
	// its superproject are one project, in both directions.
	if !SameProject(super, sub) || !SameProject(sub, super) {
		t.Error("a submodule and its superproject are one project, either way round")
	}
	// A worktree OF the submodule is in the same family too, transitively.
	wt := filepath.Join(t.TempDir(), "sub-wt")
	linkedWorktree(t, filepath.Join(super, ".git", "modules", "sub"), "sw", wt, false)
	if !SameProject(wt, super) || !SameProject(super, wt) {
		t.Error("a worktree of a submodule belongs to the superproject's family")
	}
}

func TestSameProject_RefusesAPlainNestedDirectory(t *testing.T) {
	repo := mainCheckout(t, t.TempDir())
	nested := filepath.Join(repo, "vendor", "plain")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if SameProject(repo, nested) || SameProject(nested, repo) {
		t.Error("a directory holding no checkout of its own is not part of the project")
	}
	if _, ok := CheckoutAt(nested); ok {
		t.Error("a directory with no .git must not resolve as a checkout")
	}
}

func TestSameProject_RefusesAnUnrelatedNestedClone(t *testing.T) {
	repo := mainCheckout(t, t.TempDir())
	clone := mainCheckout(t, filepath.Join(repo, "vendor", "clone"))

	if SameProject(repo, clone) || SameProject(clone, repo) {
		t.Error("a repository cloned INSIDE another is a different project: its common directory is its own .git")
	}
}

func TestSameProject_RefusesALinkNamingAnotherCheckoutsGitDir(t *testing.T) {
	super := mainCheckout(t, t.TempDir())
	realWT := filepath.Join(t.TempDir(), "real")
	realGitDir := filepath.Join(super, ".git", "worktrees", "real")
	linkedWorktree(t, filepath.Join(super, ".git"), "real", realWT, false)

	// The forgery: a clone inside the superproject writes its own .git file naming
	// the REAL worktree's git directory, hoping to inherit its commondir. Its name
	// is free; writing the back-link is not, and that back-link names `realWT`.
	evil := filepath.Join(super, "vendor", "evil")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(evil, ".git"), "gitdir: "+realGitDir+"\n")

	if _, ok := CheckoutAt(evil); ok {
		t.Error("a .git file whose gitdir's back-link names a DIFFERENT checkout must not verify")
	}
	if SameProject(super, evil) || SameProject(evil, super) {
		t.Error("the forged link must not make the clone part of the project")
	}
}

func TestSameProject_RefusesAForgedSubmoduleLink(t *testing.T) {
	super := mainCheckout(t, t.TempDir())
	sub := filepath.Join(super, "sub")
	submodule(t, super, "sub", sub, "../../../sub")

	// The same forgery aimed at a submodule: the clone names the real submodule's
	// git directory, so core.worktree resolves to the REAL checkout, not this one.
	evil := filepath.Join(super, "vendor", "evil")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(evil, ".git"), "gitdir: "+filepath.Join(super, ".git", "modules", "sub")+"\n")

	if _, ok := CheckoutAt(evil); ok {
		t.Error("a .git file naming a submodule git directory must verify only for the checkout core.worktree names")
	}
	if SameProject(super, evil) {
		t.Error("the forged submodule link must not make the clone part of the project")
	}
}

func TestSameProject_RefusesAWorktreesLookalike(t *testing.T) {
	// The git directory must be DIRECTLY in <common>/worktrees/. Comparing with a
	// string prefix instead of a parent test would accept <common>/worktrees-extra,
	// whose commondir still resolves to <common> — so the fixture puts the git
	// directory exactly there.
	repo := mainCheckout(t, t.TempDir())
	common := filepath.Join(repo, ".git")
	evil := filepath.Join(t.TempDir(), "evil")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(common, "worktrees-extra", "evil")
	write(t, filepath.Join(evil, ".git"), "gitdir: "+gitDir+"\n")
	write(t, filepath.Join(gitDir, "commondir"), "../..\n")
	write(t, filepath.Join(gitDir, "gitdir"), filepath.Join(evil, ".git")+"\n")

	if _, ok := CheckoutAt(evil); ok {
		t.Error("a git directory that is not DIRECTLY in <common>/worktrees/ must not verify")
	}
	if SameProject(repo, evil) {
		t.Error("the lookalike directory must not make the clone part of the project")
	}
}

func TestSameProject_ResolvesASymlinkedWorkspace(t *testing.T) {
	base := t.TempDir()
	repo := mainCheckout(t, filepath.Join(base, "repo"))
	wt := filepath.Join(base, "wt")
	linkedWorktree(t, filepath.Join(repo, ".git"), "wt", wt, false)

	// Reached through a symlink, as macOS reaches /tmp via /private/tmp and as a
	// checkout under a symlinked home does. String comparison would call these two
	// different projects.
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !SameProject(link, wt) {
		t.Error("a workspace reached through a symlink is the same project as its worktree")
	}
}

func TestCheckoutAt_RelativeLinks(t *testing.T) {
	// Every link git writes may be relative, and each is relative to a DIFFERENT
	// base: the .git file's target to its checkout, commondir and core.worktree to
	// the git directory holding them. The fixtures above use relative commondir and
	// worktree values; this pins the .git file's own relative target.
	base := t.TempDir()
	repo := mainCheckout(t, filepath.Join(base, "repo"))
	sub := filepath.Join(repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/sub\n")
	write(t, filepath.Join(repo, ".git", "modules", "sub", "config"), "[core]\n\tworktree = ../../../sub\n")

	c, ok := CheckoutAt(sub)
	if !ok {
		t.Fatal("a submodule whose .git target is relative must resolve")
	}
	if !samePath(c.CommonDir, filepath.Join(repo, ".git", "modules", "sub")) {
		t.Errorf("common dir = %q, want the submodule's git directory", c.CommonDir)
	}
	if !SameProject(repo, sub) {
		t.Error("a relative .git target must still make the submodule part of the superproject")
	}
}
