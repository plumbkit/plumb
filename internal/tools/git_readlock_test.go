package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// gitRepoWithStatDirtyFile builds a repo with one committed file whose stat
// info no longer matches the index, which is the condition that makes `git
// status` want to refresh (and therefore lock) the index.
func gitRepoWithStatDirtyFile(t *testing.T) (repo, file string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo = t.TempDir()
	file = filepath.Join(repo, "f.txt")

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(file, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-qm", "init")

	// Bump mtime past the index's recorded stat so a refresh has work to do.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	return repo, file
}

// TestDirtyCheck_DoesNotTakeIndexLock is the regression for a stranded
// .git/index.lock blocking every later commit in the repo.
//
// `git status` refreshes the index as a side effect, and that refresh is a
// write: it takes .git/index.lock and rewrites .git/index. plumb's dirty guard
// runs it on every destructive write, under a context whose cancellation
// SIGKILLs the child — which git cannot trap, so it never removes the lock.
//
// Asserting on .git/index's mtime is the direct proof: if the file is
// unchanged, no refresh happened, so no lock was ever taken.
func TestDirtyCheck_DoesNotTakeIndexLock(t *testing.T) {
	repo, file := gitRepoWithStatDirtyFile(t)
	indexPath := filepath.Join(repo, ".git", "index")

	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	// The production dirty-guard path.
	_ = pathIsDirty(context.Background(), file)

	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf(".git/index was rewritten by the dirty check (mtime %v -> %v): "+
			"the read took index.lock, so a cancelled check can strand it",
			before.ModTime(), after.ModTime())
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); err == nil {
		t.Error("dirty check left an index.lock behind")
	}
}

// TestDirtyCheck_WorksWithStaleIndexLockPresent covers the other half: once a
// lock HAS been stranded (by an older plumb, or by any interrupted git), the
// dirty guard must keep answering rather than silently failing open and letting
// a destructive write through unchecked.
func TestDirtyCheck_WorksWithStaleIndexLockPresent(t *testing.T) {
	repo, file := gitRepoWithStatDirtyFile(t)

	if err := os.WriteFile(filepath.Join(repo, ".git", "index.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the file genuinely dirty so there is a real answer to get wrong.
	if err := os.WriteFile(file, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !pathIsDirty(context.Background(), file) {
		t.Error("dirty check reported clean while a stale index.lock was present — " +
			"a destructive write would be allowed through the guard")
	}
}

// TestGitReadArgv_PrefixesFlag pins the helper itself, so a new read-only call
// site that forgets it is a visible difference rather than a silent one.
func TestGitReadArgv_PrefixesFlag(t *testing.T) {
	got := gitReadArgv([]string{"status", "--porcelain"})
	want := []string{"--no-optional-locks", "status", "--porcelain"}
	if len(got) != len(want) {
		t.Fatalf("gitReadArgv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gitReadArgv = %v, want %v", got, want)
		}
	}
}
