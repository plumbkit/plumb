package fswatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// worktrees_test.go — Options.SkipLinkedWorktrees. The layouts are built by
// hand, as git writes them (paths.LinkedWorktreeOf verifies the commondir and
// the back-link, so the fixture must carry both); paths' own tests cover the
// classification, these cover the watcher.

// gitRepoAt makes dir a main checkout: a .git directory with HEAD and objects.
func gitRepoAt(t *testing.T, dir string) {
	t.Helper()
	populate(t, filepath.Join(dir, ".git"), "HEAD")
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// worktreeAdmin writes the admin side of a linked worktree of the repository at
// repo whose checkout will live at wt: <repo>/.git/worktrees/<id> with its
// commondir and its back-link to wt/.git.
func worktreeAdmin(t *testing.T, repo, id, wt string) string {
	t.Helper()
	admin := filepath.Join(repo, ".git", "worktrees", id)
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"commondir": "../..\n", "gitdir": filepath.Join(wt, ".git") + "\n"} {
		if err := os.WriteFile(filepath.Join(admin, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return admin
}

// worktreeLink writes wt's .git file, pointing at its admin directory.
func worktreeLink(t *testing.T, wt, admin string) {
	t.Helper()
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// eventsUnder is every reported path inside dir.
func eventsUnder(c *collector, dir string) []string {
	var out []string
	for _, ev := range c.snapshot() {
		if strings.HasPrefix(ev.Path, dir+string(filepath.Separator)) {
			out = append(out, ev.Path)
		}
	}
	return out
}

// rootStillReports writes a file at the root until the watcher reports it — the
// positive control that the watcher was live while the worktree was quiet.
func rootStillReports(t *testing.T, c *collector, root string) {
	t.Helper()
	expectEventually(t, c, filepath.Join(root, "ping.go"), "the root itself")
}

func TestWatcher_SkipsAnExistingLinkedWorktree(t *testing.T) {
	for _, skip := range []bool{true, false} {
		t.Run(map[bool]string{true: "skipped", false: "control: watched"}[skip], func(t *testing.T) {
			root := t.TempDir()
			gitRepoAt(t, root)
			wt := filepath.Join(root, "plumb-wt-a")
			worktreeLink(t, wt, worktreeAdmin(t, root, "plumb-wt-a", wt))
			c := startWatcher(t, root, Options{SkipLinkedWorktrees: skip})

			inside := filepath.Join(wt, "pkg", "a.go")
			populate(t, wt, "pkg/a.go")
			if !skip {
				expectEventually(t, c, inside, "the worktree, with the skip off")
				return
			}
			rootStillReports(t, c, root)
			if err := os.WriteFile(inside, []byte("y"), 0o644); err != nil {
				t.Fatal(err)
			}
			rootStillReports(t, c, root)
			if got := eventsUnder(c, wt); len(got) != 0 {
				t.Fatalf("events inside a linked worktree were reported: %v", got)
			}
		})
	}
}

// TestWatcher_SkipsAWorktreeCreatedInPlace: `git worktree add` makes the
// directory, then writes its .git, then checks files out. The first verdict for
// the directory can be "not a worktree"; the .git write must correct it.
func TestWatcher_SkipsAWorktreeCreatedInPlace(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, root)
	c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})

	wt := filepath.Join(root, "plumb-wt-new")
	early := filepath.Join(wt, "early.go")
	populate(t, wt, "early.go") // before the .git exists: an ordinary directory
	expectEventually(t, c, early, "a plain directory, before it is a worktree")

	worktreeLink(t, wt, worktreeAdmin(t, root, "plumb-wt-new", wt))
	rootStillReports(t, c, root)
	before := len(eventsUnder(c, wt))
	populate(t, wt, "late/a.go", "late/b.go")
	if err := os.WriteFile(early, []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootStillReports(t, c, root)
	time.Sleep(200 * time.Millisecond)
	if got := eventsUnder(c, wt); len(got) != before {
		t.Fatalf("after its .git was written, the worktree still reported %v", got[before:])
	}
}

// TestWatcher_DoesNotExpandAWorktreeMovedInWhole: a populated worktree renamed
// into the tree arrives as one directory; it must not be expanded.
func TestWatcher_DoesNotExpandAWorktreeMovedInWhole(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, root)
	staging := filepath.Join(t.TempDir(), "wt")
	wt := filepath.Join(root, "plumb-wt-moved")
	populate(t, staging, "a.go", "deep/b.go")
	worktreeLink(t, staging, worktreeAdmin(t, root, "plumb-wt-moved", wt)) // the back-link names its final place
	c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})

	if err := os.Rename(staging, wt); err != nil {
		t.Fatal(err)
	}
	rootStillReports(t, c, root)
	time.Sleep(200 * time.Millisecond)
	if got := eventsUnder(c, wt); len(got) != 0 {
		t.Fatalf("a worktree moved in whole was expanded: %v", got)
	}
}

func TestWorktreeSkip_CachesAndForgets(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws")
	s := newWorktreeSkip(root)
	asked := map[string]int{}
	worktrees := map[string]bool{filepath.Join(root, "wt"): true}
	s.isWorktree = func(r, dir string) bool {
		if r != root {
			t.Fatalf("judged against %q, want the root", r)
		}
		asked[dir]++
		return worktrees[dir]
	}
	in := func(rel string) bool { return s.inside(filepath.Join(root, filepath.FromSlash(rel))) }

	if !in("wt/a/b.go") || !in("wt/c.go") {
		t.Fatal("paths inside the worktree were not skipped")
	}
	if asked[filepath.Join(root, "wt")] != 1 {
		t.Errorf("the worktree was judged %d times, want once (cached)", asked[filepath.Join(root, "wt")])
	}
	if in("src/x.go") || in("top.go") {
		t.Fatal("paths outside any worktree were skipped")
	}
	if _, judged := asked[filepath.Join(root, "top.go")]; judged {
		t.Error("an event's own path was judged: it may be a file")
	}
	if _, judged := asked[root]; judged {
		t.Error("the root was judged")
	}

	// A directory that becomes a worktree: its .git event re-decides it.
	plain := filepath.Join(root, "src")
	worktrees[plain] = true
	if in("src/y.go") {
		t.Fatal("control: the cached verdict stands until something says otherwise")
	}
	in("src/.git")
	if !in("src/y.go") {
		t.Error("a .git event did not re-decide its directory")
	}
	// The directory's own event (removed, replaced) forgets it too.
	worktrees[plain] = false
	in("src")
	if in("src/z.go") {
		t.Error("an event for the directory itself did not re-decide it")
	}

	var off *worktreeSkip
	if off.inside(filepath.Join(root, "wt", "a.go")) || off.arrived(filepath.Join(root, "wt")) {
		t.Error("a watcher without the option skipped something")
	}
	if s.arrived(root) || s.inside(filepath.Join(string(filepath.Separator), "elsewhere", "wt", "a.go")) {
		t.Error("the root itself, or a path outside it, was judged")
	}
}

// TestWatcher_ExpansionPrunesANestedWorktree: a directory moved in whole that
// holds a worktree is expanded, but not into the worktree.
func TestWatcher_ExpansionPrunesANestedWorktree(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, root)
	staging := filepath.Join(t.TempDir(), "agents")
	moved := filepath.Join(root, "agents")
	wt := filepath.Join(moved, "wt")
	populate(t, staging, "notes.go", "wt/a.go", "wt/deep/b.go")
	worktreeLink(t, filepath.Join(staging, "wt"), worktreeAdmin(t, root, "wt", wt))
	c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})

	if err := os.Rename(staging, moved); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(moved, "notes.go")
	if !waitFor(func() bool { return c.count(notes, 0) > 0 }, 15*time.Second) {
		t.Fatalf("control: the moved directory's own file was not reported; got %v", c.snapshot())
	}
	rootStillReports(t, c, root)
	if got := eventsUnder(c, wt); len(got) != 0 {
		t.Fatalf("the expansion walked into a nested worktree: %v", got)
	}
}
