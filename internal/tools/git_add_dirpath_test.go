package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitAddDirFixture builds a repo with a committed multi-file subtree at
// <root>/pkg (plus a nested level), so a directory pathspec has real tracked
// content beneath it. Returns the repo root.
func gitAddDirFixture(t *testing.T) string {
	t.Helper()
	dir := initTestRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"pkg/a.txt", "pkg/b.txt", "pkg/deep/c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "pkg")
	run("commit", "-m", "add pkg tree")
	return dir
}

// stagedDeletions returns the paths git reports as staged deletions.
func stagedDeletions(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "diff", "--cached", "--name-only", "--diff-filter=D")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	var got []string
	for l := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		if l != "" {
			got = append(got, l)
		}
	}
	return got
}

// TestGit_AddStagesDeletionsUnderDeletedDirectory is the regression test for
// the reported friction: after a bulk `rm -rf <dir>`, staging that DIRECTORY
// path reported "nothing staged" with an unmatched-path warning and left every
// deletion unstaged.
//
// partitionAddPaths matched a path two ways and both were file-shaped:
// `git ls-files` never prints a directory itself, and os.Stat fails once the
// directory is gone. The directory was therefore filed as an unmatched typo and
// resolveAddArgv short-circuited without invoking git at all — even though
// `git add -A -- <dir>`, which the tool builds, stages exactly these deletions.
func TestGit_AddStagesDeletionsUnderDeletedDirectory(t *testing.T) {
	requireGit(t)
	dir := gitAddDirFixture(t)

	// The bulk deletion the agent performed outside plumb.
	if err := os.RemoveAll(filepath.Join(dir, "pkg")); err != nil {
		t.Fatal(err)
	}

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "add",
		"files":      []string{filepath.Join(dir, "pkg")},
		"repo":       dir,
	})
	if err != nil {
		t.Fatalf("staging a deleted directory should succeed, got: %v", err)
	}
	if strings.Contains(out, "nothing staged") {
		t.Errorf("deleted directory was short-circuited as unmatched, got %q", out)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("a directory holding tracked content must not be reported unmatched, got %q", out)
	}

	got := stagedDeletions(t, dir)
	want := []string{"pkg/a.txt", "pkg/b.txt", "pkg/deep/c.txt"}
	if len(got) != len(want) {
		t.Fatalf("expected %d staged deletions %v, got %d: %v", len(want), want, len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("staged deletion %d: want %q, got %q", i, w, got[i])
		}
	}
}

// TestGit_AddStagesDeletionsUnderDeletedSubdirectory covers the nested case: a
// directory one level down, so the ancestor walk must record more than the
// immediate parent of each tracked file.
func TestGit_AddStagesDeletionsUnderDeletedSubdirectory(t *testing.T) {
	requireGit(t)
	dir := gitAddDirFixture(t)

	if err := os.RemoveAll(filepath.Join(dir, "pkg", "deep")); err != nil {
		t.Fatal(err)
	}

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "add",
		"files":      []string{filepath.Join(dir, "pkg", "deep")},
		"repo":       dir,
	})
	if err != nil {
		t.Fatalf("staging a deleted nested directory should succeed, got: %v", err)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("nested tracked directory must not be reported unmatched, got %q", out)
	}
	got := stagedDeletions(t, dir)
	if len(got) != 1 || got[0] != "pkg/deep/c.txt" {
		t.Errorf("expected only pkg/deep/c.txt staged as deleted, got %v", got)
	}
}

// TestGit_AddRelativeDeletedDirectoryPathspec covers the same fix reached
// through a repo-relative pathspec rather than an absolute one, since
// canonicalAddPaths resolves the two through different branches.
func TestGit_AddRelativeDeletedDirectoryPathspec(t *testing.T) {
	requireGit(t)
	dir := gitAddDirFixture(t)

	if err := os.RemoveAll(filepath.Join(dir, "pkg")); err != nil {
		t.Fatal(err)
	}

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "add",
		"files":      []string{"pkg"},
		"repo":       dir,
	})
	if err != nil {
		t.Fatalf("staging a deleted directory by relative path should succeed, got: %v", err)
	}
	if strings.Contains(out, "warning") {
		t.Errorf("relative deleted directory must not be reported unmatched, got %q", out)
	}
	if n := len(stagedDeletions(t, dir)); n != 3 {
		t.Errorf("expected 3 staged deletions, got %d", n)
	}
}

// TestGit_AddTypoedDirectoryStillWarns guards the fix's blast radius: widening
// the match to directories holding tracked content must NOT make a genuinely
// bogus path pass the precheck, which is what stops `git add` hard-failing the
// whole call.
func TestGit_AddTypoedDirectoryStillWarns(t *testing.T) {
	requireGit(t)
	dir := gitAddDirFixture(t)

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	bogus := filepath.Join(dir, "pkgg") // never existed, holds nothing tracked
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "add",
		"files":      []string{bogus},
		"repo":       dir,
	})
	if err != nil {
		t.Fatalf("a bogus path should be warned about, not hard-fail: %v", err)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("expected a warning for a path holding no tracked content, got %q", out)
	}
	if !strings.Contains(out, bogus) {
		t.Errorf("expected the warning to name %q, got %q", bogus, out)
	}
}

// TestRecordTrackedDirs_StopsAtRoot pins the ancestor walk directly: it must
// record every level from the file's parent up to and including root, and must
// never ascend past root.
func TestRecordTrackedDirs_StopsAtRoot(t *testing.T) {
	root := filepath.Join("/tmp", "repo")
	dirs := map[string]bool{}
	recordTrackedDirs(dirs, filepath.Join(root, "a", "b", "c.txt"), root)

	for _, want := range []string{root, filepath.Join(root, "a"), filepath.Join(root, "a", "b")} {
		if !dirs[want] {
			t.Errorf("expected %q to be recorded, got %v", want, dirs)
		}
	}
	if dirs["/tmp"] || dirs["/"] {
		t.Errorf("walk ascended above root: %v", dirs)
	}
	if len(dirs) != 3 {
		t.Errorf("expected exactly 3 recorded dirs, got %d: %v", len(dirs), dirs)
	}
}

// TestRecordTrackedDirs_BoundaryCases covers the pitfall class the raw
// strings.HasPrefix version had. The walk-stops-at-root test above passes under
// EITHER implementation — its fixture never has a sibling sharing a textual
// prefix, nor a trailing separator on the root — so it could not catch a
// regression to the very bug dirWithinRoot was written to fix.
func TestRecordTrackedDirs_BoundaryCases(t *testing.T) {
	t.Run("sibling sharing a textual prefix is not admitted", func(t *testing.T) {
		dirs := map[string]bool{}
		// "/tmp/repository" merely starts with "/tmp/repo"; nothing under it is
		// inside the repo, so nothing may be recorded.
		recordTrackedDirs(dirs, filepath.Join("/tmp", "repository", "x.txt"), filepath.Join("/tmp", "repo"))
		if len(dirs) != 0 {
			t.Errorf("a sibling sharing a prefix must record nothing, got %v", dirs)
		}
	})

	t.Run("root with a trailing separator still records the root", func(t *testing.T) {
		root := filepath.Join("/tmp", "repo")
		dirs := map[string]bool{}
		recordTrackedDirs(dirs, filepath.Join(root, "a", "c.txt"), root+string(filepath.Separator))
		if !dirs[root] {
			t.Errorf("the root itself must be recorded even when given with a trailing separator, got %v", dirs)
		}
		if !dirs[filepath.Join(root, "a")] {
			t.Errorf("the intermediate directory must be recorded, got %v", dirs)
		}
	})

	t.Run("file directly in the root records only the root", func(t *testing.T) {
		root := filepath.Join("/tmp", "repo")
		dirs := map[string]bool{}
		recordTrackedDirs(dirs, filepath.Join(root, "c.txt"), root)
		if len(dirs) != 1 || !dirs[root] {
			t.Errorf("expected only the root recorded, got %v", dirs)
		}
	})
}

func TestDirWithinRoot(t *testing.T) {
	root := filepath.Join("/tmp", "repo")
	cases := []struct {
		dir  string
		want bool
	}{
		{root, true},
		{filepath.Join(root, "a"), true},
		{filepath.Join(root, "a", "b"), true},
		{filepath.Join("/tmp", "repository"), false},         // sibling sharing a prefix
		{filepath.Join("/tmp", "repository", "deep"), false}, // and below it
		{"/tmp", false},
		{"/", false},
		{filepath.Join("/other", "repo"), false},
	}
	for _, tc := range cases {
		if got := dirWithinRoot(tc.dir, root); got != tc.want {
			t.Errorf("dirWithinRoot(%q, %q) = %v, want %v", tc.dir, root, got, tc.want)
		}
	}
}
