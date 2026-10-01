package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMain moves the data directory to a temporary one for the whole test
// binary. Tests in this package reach the session registry, directly or through
// the code under test; without this, a test that does not set its own
// XDG_DATA_HOME would take the user's real .sessions.lock and read or write the
// real session files, contending with the live daemon (#551). session.Dir ends a
// test binary that still resolves to the live registry, so a package that
// forgets this fails loudly rather than stalling.
//
// It sets XDG_DATA_HOME rather than a dedicated registry override because a test
// that sets its own XDG_DATA_HOME must keep the private registry that gives it:
// the registry is where fixed session names, and the random draws that avoid
// them, are made unique, so one directory shared by every test lets a session
// another test left live hold a name this one asks for.
func TestMain(m *testing.M) {
	os.Exit(runWithIsolatedData(m))
}

func runWithIsolatedData(m *testing.M) int {
	dir, err := os.MkdirTemp("", "plumb-data-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: creating the data dir:", err)
		return 1
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a temp dir after the run
	if err := os.Setenv("XDG_DATA_HOME", dir); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: setting XDG_DATA_HOME:", err)
		return 1
	}
	if err := disableGitAutoMaintenance(); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: disabling git auto-maintenance:", err)
		return 1
	}
	return m.Run()
}

// disableGitAutoMaintenance stops every git child of this test binary from
// forking a detached background `git maintenance run --auto`.
//
// A fixture's `git commit` otherwise returns while that child is still alive: it
// takes and drops .git/objects/maintenance.lock after the commit has returned, so
// a test that ends soon afterwards has t.TempDir's RemoveAll race the child
// writing into the repository's .git, and fails with "directory not empty"
// (#574; the lock was still there after the commit returned in 133 to 224 of 300
// runs, under load). A short test is where the window bites, because a long one
// gives the child time to finish. Disabling the maintenance, not retrying the
// clean-up, removes the writer instead of racing it.
//
// GIT_CONFIG_COUNT is command-scope configuration, so it outranks a fixture's
// own .git/config and a developer's global one, and every fixture in the package
// inherits it however it builds its repository. It appends to a count the
// environment already holds rather than replacing it.
func disableGitAutoMaintenance() error {
	n := 0
	if v := os.Getenv("GIT_CONFIG_COUNT"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 0 {
			return fmt.Errorf("GIT_CONFIG_COUNT=%q is not a count", v)
		}
	}
	// gc.auto=0 covers a git that predates `git maintenance` (before 2.29) and
	// runs `gc --auto` straight from the commit.
	for i, kv := range [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}} {
		idx := strconv.Itoa(n + i)
		if err := os.Setenv("GIT_CONFIG_KEY_"+idx, kv[0]); err != nil {
			return err
		}
		if err := os.Setenv("GIT_CONFIG_VALUE_"+idx, kv[1]); err != nil {
			return err
		}
	}
	return os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(n+2))
}

// TestGitFixtureCommitForksNoMaintenance pins what disableGitAutoMaintenance is
// for. git traces every child it starts, so the trace of one fixture `git commit`
// shows, synchronously and with no timing involved, whether the commit forked the
// detached `git maintenance run --auto` that kept writing into .git after the
// commit returned (#574). Asserting on the trace rather than polling the
// directory is what keeps this from being flaky itself.
func TestGitFixtureCommitForksNoMaintenance(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	trace := filepath.Join(t.TempDir(), "trace2.txt")
	gitTraced := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TRACE2="+trace)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gwWrite(t, dir, "again.txt", "again\n")
	gitTraced("add", "again.txt")
	gitTraced("commit", "-q", "-m", "again")

	got, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	// The control: a trace that never saw the commit would pass the check below
	// for the wrong reason.
	if !strings.Contains(string(got), "start git commit") {
		t.Fatalf("the trace did not record the commit, so it cannot show what the commit forked:\n%s", got)
	}
	if strings.Contains(string(got), "maintenance") {
		t.Errorf("a fixture commit must not fork git maintenance, which outlives the commit and writes into .git:\n%s", got)
	}
}
