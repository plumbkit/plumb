package fswatch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// fakeSkip is a worktreeSkip over /ws whose verified worktrees, recorded
// bookkeeping and directories (place, vanish) the test sets, counting the
// verifications it makes.
var listed int // fakeSkip's bookkeeping reads, across the test binary

// fakeDir is a directory in a fakeSkip's tree; its id is its identity.
type fakeDir struct{ id int }

func (fakeDir) Name() string       { return "d" }
func (fakeDir) Size() int64        { return 0 }
func (fakeDir) Mode() os.FileMode  { return os.ModeDir | 0o755 }
func (fakeDir) ModTime() time.Time { return time.Time{} }
func (fakeDir) IsDir() bool        { return true }
func (fakeDir) Sys() any           { return nil }

var (
	fakeTrees  = map[*worktreeSkip]map[string]int{}
	fakeNextID int
)

// place puts a NEW directory at path — a fresh identity, as a mkdir or a tree
// moved in from elsewhere gives it.
func place(s *worktreeSkip, paths ...string) {
	for _, p := range paths {
		fakeNextID++
		fakeTrees[s][p] = fakeNextID
	}
}

// vanish removes path and everything beneath it from the fake tree.
func vanish(s *worktreeSkip, path string) {
	for p := range fakeTrees[s] {
		if p == path || strings.HasPrefix(p, path+string(filepath.Separator)) {
			delete(fakeTrees[s], p)
		}
	}
}

func fakeSkip(t *testing.T) (s *worktreeSkip, verified, recorded map[string]bool, asked map[string]int) {
	t.Helper()
	root := filepath.Join(string(filepath.Separator), "ws")
	verified, recorded, asked = map[string]bool{}, map[string]bool{}, map[string]int{}
	s = &worktreeSkip{root: root}
	s.resetLocked()
	fakeTrees[s] = map[string]int{}
	t.Cleanup(func() { delete(fakeTrees, s) })
	s.stat = func(p string) (os.FileInfo, error) {
		if id, ok := fakeTrees[s][p]; ok {
			return fakeDir{id}, nil
		}
		return nil, os.ErrNotExist
	}
	s.sameFile = func(a, b os.FileInfo) bool { return a.(fakeDir).id == b.(fakeDir).id }
	s.isWorktree = func(r, dir string) bool {
		if r != root {
			t.Fatalf("judged against %q, want the root", r)
		}
		asked[dir]++
		return verified[dir]
	}
	s.listWorktrees = func(string) []string {
		listed++
		var out []string
		for d, ok := range recorded {
			if ok {
				out = append(out, d)
			}
		}
		return out
	}
	s.refreshLocked()
	return s, verified, recorded, asked
}

func TestWorktreeSkip_CachesAndForgets(t *testing.T) {
	s, verified, _, asked := fakeSkip(t)
	root := s.root
	at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
	in := func(rel string, op Op) bool { return s.inside(at(rel), op) }
	verified[at("wt")] = true

	if !in("wt/a/b.go", Write) || !in("wt/c.go", Write) {
		t.Fatal("paths inside the worktree were not skipped")
	}
	if asked[at("wt")] != 1 {
		t.Errorf("the worktree was judged %d times, want once (cached)", asked[at("wt")])
	}
	if in("src/x.go", Write) || in("top.go", Write) {
		t.Fatal("paths outside any worktree were skipped")
	}
	if _, judged := asked[at("top.go")]; judged {
		t.Error("an event's own path was judged: it may be a file")
	}
	if _, judged := asked[root]; judged {
		t.Error("the root was judged")
	}

	// A directory that becomes a worktree: its .git event re-decides it.
	verified[at("src")] = true
	if in("src/y.go", Write) {
		t.Fatal("control: the cached verdict stands until something says otherwise")
	}
	in("src/.git", Create)
	if !in("src/y.go", Write) {
		t.Error("a .git event did not re-decide its directory")
	}
	// Its .git going away (a removal starting) does not downgrade it.
	verified[at("src")] = false
	in("src/.git", Remove)
	if !in("src/z.go", Remove) {
		t.Error("a worktree's .git removal downgraded it mid-removal")
	}
	// A metadata event for the directory itself re-decides nothing; its own
	// removal does.
	in("src", Chmod)
	if !in("src/z.go", Remove) {
		t.Error("a metadata event for the worktree re-decided it")
	}
	// Nor does its own Remove (it can arrive before its children's); a
	// directory arriving at the path does.
	in("src", Remove)
	if !in("src/z.go", Remove) {
		t.Error("the worktree's own Remove re-decided it mid-removal")
	}
	place(s, at("src"))
	in("src", Create)
	if in("src/z.go", Create) {
		t.Error("a directory arriving at the path did not re-decide it")
	}

	var off *worktreeSkip
	if off.inside(at("wt/a.go"), Write) || off.arrived(at("wt")) {
		t.Error("a watcher without the option skipped something")
	}
	off.lost()
	if s.arrived(root) || s.inside(filepath.Join(string(filepath.Separator), "elsewhere", "wt", "a.go"), Write) {
		t.Error("the root itself, or a path outside it, was judged")
	}
}

// TestWorktreeSkip_BookkeepingCoversARemoval: a worktree whose .git is already
// gone (removed first, or before this watcher ever saw it) is still a worktree
// while git's bookkeeping records it.
func TestWorktreeSkip_BookkeepingCoversARemoval(t *testing.T) {
	s, _, recorded, _ := fakeSkip(t)
	wt := filepath.Join(s.root, "plumb-wt-gone")
	recorded[wt] = true
	s.lost() // re-read the bookkeeping
	if !s.inside(filepath.Join(wt, "a.go"), Remove) || !s.arrived(wt) {
		t.Fatal("a recorded worktree with no .git was not skipped")
	}
	// Once the bookkeeping no longer records it and events were lost, the
	// directory is re-decided: it may have been replaced by an ordinary one.
	recorded[wt] = false
	s.lost()
	if s.inside(filepath.Join(wt, "a.go"), Create) {
		t.Error("after a loss, a directory git no longer records stayed skipped")
	}
}

// TestWorktreeSkip_TheCapKeepsWorktrees: a full cache drops "not a worktree"
// verdicts, never a "worktree" one.
func TestWorktreeSkip_TheCapKeepsWorktrees(t *testing.T) {
	s, verified, _, asked := fakeSkip(t)
	wt := filepath.Join(s.root, "wt")
	verified[wt] = true
	s.inside(filepath.Join(wt, "a.go"), Write)
	for i := range worktreeCacheMax + 10 {
		s.inside(filepath.Join(s.root, "d"+strconv.Itoa(i), "x.go"), Write)
	}
	verified[wt] = false // a re-decision now would say no
	if !s.inside(filepath.Join(wt, "b.go"), Write) || asked[wt] != 1 {
		t.Errorf("the cap evicted a worktree verdict (judged %d times)", asked[wt])
	}
	s.mu.Lock()
	n := len(s.ordinary) + len(s.worktree)
	s.mu.Unlock()
	if n > worktreeCacheMax+1 {
		t.Errorf("the cache holds %d verdicts, past its cap", n)
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

// TestWatcher_ARemovalIsQuiet: deleting a worktree — warm (seen before), cold
// (never seen), or with its .git deleted first — reports nothing inside it
// while git's admin directory still records it, which is the order both
// `git worktree remove` and `rm -rf` leave things in.
func TestWatcher_ARemovalIsQuiet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		warm      bool
		gitsFirst bool
	}{{"warm", true, false}, {"cold", false, false}, {".git first", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			gitRepoAt(t, root)
			wt := filepath.Join(root, "plumb-wt-rm")
			populate(t, wt, "a.go", "pkg/b.go", "pkg/deep/c.go")
			worktreeLink(t, wt, worktreeAdmin(t, root, "plumb-wt-rm", wt))
			c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})
			if tc.warm {
				if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("w"), 0o644); err != nil {
					t.Fatal(err)
				}
				rootStillReports(t, c, root)
			}
			if tc.gitsFirst {
				if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(wt); err != nil {
				t.Fatal(err)
			}
			rootStillReports(t, c, root)
			time.Sleep(200 * time.Millisecond)
			if got := eventsUnder(c, wt); len(got) != 0 {
				t.Fatalf("a worktree's removal was reported: %v", got)
			}
		})
	}
}

// TestWatcher_GitWorktreeAddAndRemoveAreQuiet runs real git.
func TestWatcher_GitWorktreeAddAndRemoveAreQuiet(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	populate(t, root, "pkg/a.go", "pkg/deep/b.go", "c.go")
	git("add", ".")
	git("commit", "-q", "-m", "init")
	c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})

	wt := filepath.Join(root, "plumb-wt-git")
	git("worktree", "add", "-q", wt)
	rootStillReports(t, c, root)
	git("worktree", "remove", "--force", wt)
	rootStillReports(t, c, root)
	time.Sleep(200 * time.Millisecond)
	if got := eventsUnder(c, wt); len(got) > 0 {
		t.Fatalf("git worktree add/remove reported %d events inside the worktree: %v", len(got), got)
	}
}

// TestWorktreeSkip_AGitEventRereadsTheBookkeeping: a worktree git records
// before its checkout verifies (commondir not yet written) is found when its
// .git appears.
func TestWorktreeSkip_AGitEventRereadsTheBookkeeping(t *testing.T) {
	s, _, recorded, _ := fakeSkip(t)
	wt := filepath.Join(s.root, "plumb-wt-new")
	if s.inside(filepath.Join(wt, "a.go"), Create) {
		t.Fatal("control: an unrecorded, unverified directory was skipped")
	}
	recorded[wt] = true
	s.inside(filepath.Join(wt, ".git"), Create)
	if !s.inside(filepath.Join(wt, "b.go"), Create) {
		t.Error("the .git event did not re-read git's bookkeeping")
	}
}

// TestWatcher_LostEventsRedecideWorktrees: the watcher's loss signal reaches
// the worktree verdicts.
func TestWatcher_LostEventsRedecideWorktrees(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, Options{SkipLinkedWorktrees: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	w.worktrees.inside(filepath.Join(root, "d", "x.go"), Write)
	w.signalLost()
	if !w.worktrees.dirty.Load() {
		t.Fatal("lost events did not mark the worktree verdicts for re-deciding")
	}
}

// TestWorktreeSkip_ARecreatedPathOutlivesItsStaleRecord: an ordinary
// directory created where a removed worktree was is watched, though git's
// bookkeeping still records the worktree (an `rm -rf` leaves the admin
// directory until `git worktree prune`) — and stays watched across a later
// re-read of that bookkeeping.
func TestWorktreeSkip_ARecreatedPathOutlivesItsStaleRecord(t *testing.T) {
	s, verified, recorded, _ := fakeSkip(t)
	wt := filepath.Join(s.root, "plumb-wt-old")
	recorded[wt] = true
	s.lost()
	if !s.inside(filepath.Join(wt, "a.go"), Remove) {
		t.Fatal("control: the recorded worktree's removal was not skipped")
	}
	s.inside(wt, Remove)
	place(s, wt)
	s.inside(wt, Create) // an ordinary directory at the same path
	if s.inside(filepath.Join(wt, "b.go"), Create) {
		t.Fatal("an ordinary directory at a removed worktree's path was skipped on a stale record")
	}
	s.inside(filepath.Join(s.root, "other", ".git"), Create) // re-reads the bookkeeping
	s.lost()
	if s.inside(filepath.Join(wt, "c.go"), Write) {
		t.Error("a re-read of the bookkeeping brought the stale record back")
	}
	// A worktree created there again is one.
	verified[wt] = true
	s.inside(filepath.Join(wt, ".git"), Create)
	if !s.inside(filepath.Join(wt, "d.go"), Create) {
		t.Error("a new worktree at the path was not skipped")
	}
	// Its removal, after a loss (a cold verdict), is covered by its record
	// again: verifying it as a worktree lifted the supersede.
	verified[wt] = false
	s.lost()
	if !s.inside(filepath.Join(wt, "e.go"), Remove) {
		t.Error("the new worktree's removal fell back on a superseded record")
	}
}

// TestWorktreeSkip_LostIsLazy: a burst of loss signals costs one re-read, at
// the next lookup; the root's own .git changing costs none.
func TestWorktreeSkip_LostIsLazy(t *testing.T) {
	s, _, _, _ := fakeSkip(t)
	before := listed
	for range 100 {
		s.lost()
	}
	if listed != before {
		t.Fatalf("lost re-read the bookkeeping %d times by itself", listed-before)
	}
	s.inside(filepath.Join(s.root, "a", "b.go"), Write)
	s.inside(filepath.Join(s.root, "a", "c.go"), Write)
	if listed-before != 1 {
		t.Fatalf("a burst of losses re-read the bookkeeping %d times, want 1", listed-before)
	}
	s.inside(filepath.Join(s.root, ".git"), Write)
	if listed-before != 1 {
		t.Error("the root's own .git changing re-read the bookkeeping")
	}
}

// TestWatcher_AnOrdinaryDirectoryWhereAWorktreeWasIsWatched: the real-backend
// shape of the stale-record case: `rm -rf` of a worktree (its admin directory
// stays), then an ordinary directory at the same path.
func TestWatcher_AnOrdinaryDirectoryWhereAWorktreeWasIsWatched(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, root)
	wt := filepath.Join(root, "plumb-wt-reused")
	populate(t, wt, "a.go")
	worktreeLink(t, wt, worktreeAdmin(t, root, "plumb-wt-reused", wt))
	c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	rootStillReports(t, c, root)
	populate(t, wt, "plain.go")
	expectEventually(t, c, filepath.Join(wt, "plain.go"), "an ordinary directory where a worktree was")
}

// TestWorktreeSkip_ADirectorysOwnRemoveFirstKeepsItsRecord: sgtdi can deliver
// a removed worktree's own Remove before its children's; those stay skipped.
func TestWorktreeSkip_ADirectorysOwnRemoveFirstKeepsItsRecord(t *testing.T) {
	s, _, recorded, _ := fakeSkip(t)
	wt := filepath.Join(s.root, "plumb-wt-gone")
	recorded[wt] = true
	s.lost()
	s.inside(wt, Remove)
	for _, rel := range []string{"a.go", "pkg/b.go"} {
		if !s.inside(filepath.Join(wt, filepath.FromSlash(rel)), Remove) {
			t.Errorf("%s's removal, after the directory's own, was reported", rel)
		}
	}
}

// TestWorktreeSkip_AnArrivedWorktreeIsRemembered: a worktree seen only by its
// own arrival (never expanded, nothing inside it looked up) stays skipped
// through a removal processed after git has forgotten it.
func TestWorktreeSkip_AnArrivedWorktreeIsRemembered(t *testing.T) {
	s, verified, _, _ := fakeSkip(t)
	wt := filepath.Join(s.root, "plumb-wt-brief")
	verified[wt] = true
	s.inside(wt, Create)
	if !s.arrived(wt) {
		t.Fatal("control: the arrived worktree was not recognised")
	}
	verified[wt] = false // its .git and git's record are gone by now
	s.inside(wt, Remove)
	if !s.inside(filepath.Join(wt, "pkg"), Remove) {
		t.Error("a worktree seen only arriving leaked its removal")
	}
}

// TestWorktreeSkip_AParentsArrivalRedecidesWorktreesBeneathIt (PLAN-558): a
// worktree verdict cached below a directory that is removed and then arrives
// whole again — an ordinary tree moved in where a worktree's parent was — does
// not silence the ordinary directory now at that path. A real worktree that
// arrives inside it is still skipped.
func TestWorktreeSkip_AParentsArrivalRedecidesWorktreesBeneathIt(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached verdict", true: "stale record too"}[stale], func(t *testing.T) {
			s, verified, recorded, _ := fakeSkip(t)
			x := filepath.Join(s.root, "agents")
			wt := filepath.Join(x, "wt")
			place(s, x, wt)
			verified[wt] = true
			if stale {
				recorded[wt] = true
				s.lost()
			}
			if !s.inside(filepath.Join(wt, "a.go"), Write) {
				t.Fatal("control: the worktree was not skipped")
			}
			vanish(s, x)
			s.inside(x, Remove)
			verified[wt] = false // an ordinary tree arrives at x, with an ordinary wt
			place(s, x, wt)
			s.inside(x, Create)
			if s.arrived(wt) {
				t.Error("the expansion of the arrived parent treats its ordinary wt as a worktree")
			}
			if s.inside(filepath.Join(wt, "edit.go"), Write) {
				t.Error("an edit inside the ordinary wt was silenced by the old verdict")
			}
		})
	}
	// Control: a real worktree inside an arriving parent is still a worktree.
	s, verified, _, _ := fakeSkip(t)
	x := filepath.Join(s.root, "agents")
	wt := filepath.Join(x, "wt")
	verified[wt] = true
	place(s, x, wt)
	s.inside(x, Create)
	if !s.arrived(wt) || !s.inside(filepath.Join(wt, "b.go"), Write) {
		t.Error("a worktree inside an arriving directory was not skipped")
	}
}

// TestWatcher_AnOrdinaryTreeMovedOverAWorktreesParentIsWatched (PLAN-558):
// the real-backend shape — a worktree's parent removed, an ordinary tree with
// the same layout moved into its place.
func TestWatcher_AnOrdinaryTreeMovedOverAWorktreesParentIsWatched(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, root)
	x := filepath.Join(root, "agents")
	wt := filepath.Join(x, "wt")
	populate(t, wt, "a.go")
	worktreeLink(t, wt, worktreeAdmin(t, root, "wt", wt))
	c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})
	if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootStillReports(t, c, root)
	if err := os.RemoveAll(x); err != nil {
		t.Fatal(err)
	}
	rootStillReports(t, c, root)
	staging := filepath.Join(t.TempDir(), "agents")
	populate(t, staging, "wt/plain.go")
	if err := os.Rename(staging, x); err != nil {
		t.Fatal(err)
	}
	expectEventually(t, c, filepath.Join(wt, "plain.go"), "an ordinary tree moved over a worktree's parent")
}

// TestWorktreeSkip_AFullWorktreeSetDropsOnlyVanishedDirectories: at the cap,
// worktree verdicts for directories that are gone are dropped; one that still
// exists is kept.
func TestWorktreeSkip_AFullWorktreeSetDropsOnlyVanishedDirectories(t *testing.T) {
	s, _, _, _ := fakeSkip(t)
	live := filepath.Join(s.root, "live")
	place(s, live)
	s.mu.Lock()
	s.rememberLocked(live, true)
	for i := range worktreeCacheMax {
		s.rememberLocked(filepath.Join(s.root, "gone-"+strconv.Itoa(i)), true)
	}
	_, kept := s.worktree[live]
	n := len(s.worktree)
	s.mu.Unlock()
	if !kept {
		t.Error("an existing worktree's verdict was dropped at the cap")
	}
	if n > 2 {
		t.Errorf("the worktree set holds %d verdicts after the cap, want the live one and the newest", n)
	}
}

// TestWorktreeSkip_AnArrivalTouchesOnlyItsOwnSubtree: a directory arriving at
// agents leaves a worktree under its sibling agents-old alone, and a worktree
// under it that was once cached as ordinary is re-decided, not answered by
// that stale verdict.
func TestWorktreeSkip_AnArrivalTouchesOnlyItsOwnSubtree(t *testing.T) {
	s, verified, _, asked := fakeSkip(t)
	sibling := filepath.Join(s.root, "agents-old", "wt")
	verified[sibling] = true
	s.inside(filepath.Join(sibling, "a.go"), Write)
	place(s, sibling)
	inner := filepath.Join(s.root, "agents", "wt")
	s.inside(filepath.Join(inner, "a.go"), Write) // cached ordinary
	verified[inner] = true
	if !s.arrived(inner) {
		t.Fatal("control: the directory that became a worktree was not recognised")
	}
	place(s, filepath.Join(s.root, "agents"), inner) // a tree with a worktree in it moved in
	s.inside(filepath.Join(s.root, "agents"), Create)
	before := asked[sibling]
	if !s.inside(filepath.Join(sibling, "b.go"), Write) || asked[sibling] != before {
		t.Error("an arrival at agents re-decided a worktree under agents-old")
	}
	if !s.inside(filepath.Join(inner, "b.go"), Write) {
		t.Error("a worktree beneath the arrival was answered by its stale ordinary verdict")
	}
}

// TestWorktreeSkip_AMoveIsNotAnArrival (#703's review): inotify reports a
// directory moving AWAY as Remove|Rename, and one moved away and back is the
// same directory. Neither drops the worktree beliefs beneath it, so the rest
// of a removal stays skipped.
func TestWorktreeSkip_AMoveIsNotAnArrival(t *testing.T) {
	for _, back := range []bool{false, true} {
		t.Run(map[bool]string{false: "moved away", true: "moved away and back"}[back], func(t *testing.T) {
			s, verified, recorded, _ := fakeSkip(t)
			x := filepath.Join(s.root, "agents")
			wt := filepath.Join(x, "wt")
			place(s, x, wt)
			verified[wt], recorded[wt] = true, true
			s.lost()
			if !s.inside(filepath.Join(wt, "a.go"), Write) {
				t.Fatal("control: the worktree was not skipped")
			}
			ids := map[string]int{x: fakeTrees[s][x], wt: fakeTrees[s][wt]}
			vanish(s, x)
			s.inside(x, Remove|Rename) // IN_MOVED_FROM
			if back {
				fakeTrees[s][x], fakeTrees[s][wt] = ids[x], ids[wt] // the same directories
				s.inside(x, Create|Rename)                          // IN_MOVED_TO
			}
			verified[wt] = false // its .git is the first thing a removal takes
			if !s.inside(filepath.Join(wt, "pkg", "b.go"), Remove) {
				t.Error("after a move, a worktree's removal was reported")
			}
			s.lost() // a cold verdict leans on the record, which a move must not supersede
			if !s.inside(filepath.Join(wt, "pkg", "c.go"), Remove) {
				t.Error("after a move, the worktree's record was superseded")
			}
		})
	}
}

// TestWorktreeSkip_AnArrivingWorktreeLosesItsOrdinaryVerdict: a directory
// once judged ordinary that arrives anew as a worktree is re-decided.
func TestWorktreeSkip_AnArrivingWorktreeLosesItsOrdinaryVerdict(t *testing.T) {
	s, verified, _, _ := fakeSkip(t)
	x := filepath.Join(s.root, "x")
	s.inside(filepath.Join(x, "a.go"), Write) // cached ordinary
	verified[x] = true
	place(s, x)
	s.inside(x, Create)
	if !s.inside(filepath.Join(x, "b.go"), Write) {
		t.Error("an arriving worktree was answered by its old ordinary verdict")
	}
}

// TestWatcher_ARenameAwayAndBackKeepsARemovalQuiet: the real-backend shape of
// #703's review — on Linux, a worktree's parent moved away and back, then the
// worktree removed, reported thousands of events.
func TestWatcher_ARenameAwayAndBackKeepsARemovalQuiet(t *testing.T) {
	root := t.TempDir()
	gitRepoAt(t, root)
	x := filepath.Join(root, "agents")
	wt := filepath.Join(x, "wt")
	populate(t, wt, "a.go", "pkg/b.go", "pkg/deep/c.go")
	worktreeLink(t, wt, worktreeAdmin(t, root, "wt", wt))
	c := startWatcher(t, root, Options{SkipLinkedWorktrees: true})
	if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("w"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootStillReports(t, c, root)
	tmp := filepath.Join(root, "agents.tmp")
	if err := os.Rename(x, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, x); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	rootStillReports(t, c, root)
	time.Sleep(300 * time.Millisecond)
	if got := eventsUnder(c, wt); len(got) != 0 {
		t.Fatalf("a worktree's removal after its parent moved away and back was reported: %v", got)
	}
}
