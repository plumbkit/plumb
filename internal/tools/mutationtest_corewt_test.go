package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mutationtest_corewt_test.go: gitProbes.mainKey trusts a repository's
// core.worktree only for a checkout of that same repository, and cannot loop.

// loggingRepo makes an upstream repository whose scripts log where they ran.
func loggingRepo(t *testing.T, dir, log string) {
	t.Helper()
	script := "echo \"$(/bin/pwd)\" >> " + shellQuote(log) + "\nexit 0\n"
	gwWrite(t, dir, "target.txt", worktreeTargetOriginal)
	gwWrite(t, dir, "compile.sh", script)
	gwWrite(t, dir, "test.sh", script)
	gitInit(t, dir)
}

// TestMutationTest_AStaleCoreWorktreeNamesNoCheckoutOfThisRepository: submodule s1
// was checked out at p and got a worktree gs1; then p was replaced by a different
// submodule, s2. s1's git directory keeps core.worktree=…/p, which now names s2's
// checkout. Taking that checkout's key made gs1 look like a copy of s2, and moved
// the commands between two unrelated repositories. gs1 is a linked worktree of a
// repository the commands do not run in: refused, as before the key existed.
func TestMutationTest_AStaleCoreWorktreeNamesNoCheckoutOfThisRepository(t *testing.T) {
	requireGit(t)
	unsetEnvForTest(t, "GOWORK")
	root := evalTempDir(t)
	log := filepath.Join(t.TempDir(), "ran.log")
	s1, s2 := filepath.Join(root, "s1-up"), filepath.Join(root, "s2-up")
	loggingRepo(t, s1, log)
	loggingRepo(t, s2, log)
	g := filepath.Join(root, "G")
	gwWrite(t, g, "README", "g\n")
	gitInit(t, g)
	gwGit(t, g, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--name", "s1", s1, "p")
	gwGit(t, g, "commit", "-q", "-m", "s1 at p")
	gs1 := filepath.Join(root, "ext", "gs1")
	gwGit(t, filepath.Join(g, "p"), "worktree", "add", "-q", "-b", "gs1", gs1)
	gwGit(t, g, "rm", "-q", "-f", "p") // s1's git directory, and its core.worktree, stay behind
	gwGit(t, g, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "--name", "s2", s2, "p")
	gwGit(t, g, "commit", "-q", "-m", "s2 at p")
	if got := gwGit(t, g, "--git-dir", filepath.Join(g, ".git", "modules", "s1"), "config", "--get", "core.worktree"); got == "" {
		t.Skip("this git removed s1's core.worktree; the stale case does not arise")
	}

	e := &submoduleEnv{super: g, log: log}
	_, err := executeMutants(t, e.tool(filepath.Join(g, "p"), "", ""), filepath.Join(gs1, "target.txt"))
	if err == nil || !strings.Contains(err.Error(), "linked git worktree") {
		t.Fatalf("a worktree of s1 is not a copy of s2; the run must be refused, not moved; got %v", err)
	}
	if b, rerr := os.ReadFile(log); rerr == nil && len(b) > 0 {
		t.Errorf("a refusal runs nothing; ran:\n%s", b)
	}
}

// TestMutationTest_ACoreWorktreeCycleEnds: core.worktree naming one of the
// repository's own linked worktrees (easy to do by accident: `git config
// core.worktree X` inside a linked worktree writes the shared config) sent
// mainKey into itself without end. The run must finish, in place.
func TestMutationTest_ACoreWorktreeCycleEnds(t *testing.T) {
	requireGit(t)
	unsetEnvForTest(t, "GOWORK")
	root := evalTempDir(t)
	log := filepath.Join(t.TempDir(), "ran.log")
	h := filepath.Join(root, "H")
	loggingRepo(t, h, log)
	hw := filepath.Join(root, "ext", "hw")
	gwGit(t, h, "worktree", "add", "-q", "-b", "hw", hw)
	gwGit(t, h, "config", "core.worktree", hw)

	e := &submoduleEnv{super: h, log: log}
	raw, err := json.Marshal(map[string]any{"mutants": []mutantJSON{{"file_path": filepath.Join(hw, "target.txt"), "old_string": "42", "new_string": "43"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := e.tool(hw, "", "").Execute(ctx, raw)
	if ctx.Err() != nil {
		t.Fatal("the run did not finish: a core.worktree cycle must end, not recurse")
	}
	if err != nil {
		t.Fatalf("commands and file in the same worktree run in place: %v", err)
	}
	if strings.Contains(out, "re-rooted") {
		t.Errorf("nothing may move; got:\n%s", out)
	}
	e.requireAllRanIn(t, hw)
}

// TestMutationTest_ACoreWorktreeIntoAnotherRepositoryNeverMovesThere: J's
// core.worktree names kw, a linked worktree of an UNRELATED repository K, so git
// prints kw as J's work-tree root. From J's own worktree jw, a file in J matched
// at J's level and the destination's path re-check agreed, and the commands were
// moved into K. The re-check now also compares repository identity.
func TestMutationTest_ACoreWorktreeIntoAnotherRepositoryNeverMovesThere(t *testing.T) {
	requireGit(t)
	unsetEnvForTest(t, "GOWORK")
	root := evalTempDir(t)
	log := filepath.Join(t.TempDir(), "ran.log")
	k, j := filepath.Join(root, "K"), filepath.Join(root, "J")
	loggingRepo(t, k, log)
	loggingRepo(t, j, log)
	kw, jw := filepath.Join(root, "ext", "kw"), filepath.Join(root, "ext", "jw")
	gwGit(t, k, "worktree", "add", "-q", "-b", "kw", kw)
	gwGit(t, j, "worktree", "add", "-q", "-b", "jw", jw)
	gwGit(t, j, "config", "core.worktree", kw)

	e := &submoduleEnv{super: j, log: log}
	out, err := executeMutants(t, e.tool(jw, "", ""), filepath.Join(j, "target.txt"))
	if err == nil && strings.Contains(out, "re-rooted") {
		t.Fatalf("the commands must never be moved into another repository's worktree; got:\n%s", out)
	}
	if b, rerr := os.ReadFile(log); rerr == nil {
		for _, d := range strings.Fields(string(b)) {
			if d == kw {
				t.Errorf("a command ran in %s, K's worktree", kw)
			}
		}
	}
}
