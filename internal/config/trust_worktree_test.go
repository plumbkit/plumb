package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// trust_worktree_test.go covers #530 item 2: a linked git worktree of a trusted
// checkout is a new path, so it had no trust record of its own and silently fell
// back to the global capability values. A worktree now shares its repository's
// grant — but only for content identical to what was approved, and only when git
// itself vouches that the worktree belongs to that repository.

const worktreeTrustConfig = "[git]\nallow_push = true\n\n[tasks.go]\nbuild = \"go build ./...\"\n"

func requireGitBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// realTempDir is t.TempDir with symlinks resolved (macOS /var → /private/var),
// because git records resolved paths in a worktree's links.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// initConfigRepo makes a committed repository at dir carrying body as its
// project config.
func initConfigRepo(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, dir, body)
	gitCmd(t, dir, "init", "-q", "-b", "main")
	gitCmd(t, dir, "config", "user.email", "t@example.com")
	gitCmd(t, dir, "config", "user.name", "t")
	gitCmd(t, dir, "config", "commit.gpgsign", "false")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "-m", "init")
}

// worktreeFixture returns a trusted main checkout and a linked worktree of it at
// the path agents use (<main>/.claude/worktrees/<name>).
func worktreeFixture(t *testing.T) (main, wt string) {
	t.Helper()
	requireGitBinary(t)
	main = filepath.Join(realTempDir(t), "main")
	initConfigRepo(t, main, worktreeTrustConfig)
	wt = filepath.Join(main, ".claude", "worktrees", "wt")
	gitCmd(t, main, "worktree", "add", "-q", "-b", "feature", wt, "main")
	return main, wt
}

func trustForProject(t *testing.T, s *TrustStore, root string) {
	t.Helper()
	spec, err := ProjectPolicySpecFor(root)
	if err != nil {
		t.Fatal(err)
	}
	cmds, err := ProjectTaskCommands(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTrustedForProject(root, cmds, spec); err != nil {
		t.Fatal(err)
	}
}

func policyTrusted(t *testing.T, s *TrustStore, root string) bool {
	t.Helper()
	spec, err := ProjectPolicySpecFor(root)
	if err != nil {
		t.Fatal(err)
	}
	return s.IsTrustedForPolicy(root, spec)
}

func tasksTrusted(t *testing.T, s *TrustStore, root string) bool {
	t.Helper()
	cmds, err := ProjectTaskCommands(root)
	if err != nil {
		t.Fatal(err)
	}
	return s.IsTrustedForTasks(root, cmds)
}

// TestTrust_LinkedWorktreeSharesAnIdenticalGrant is the reported case: the
// worktree reads the same checked-in config the user approved in the main
// checkout, so its [git] allow_push and task commands are honoured there too.
func TestTrust_LinkedWorktreeSharesAnIdenticalGrant(t *testing.T) {
	main, wt := worktreeFixture(t)
	s := tempTrustStore(t)
	trustForProject(t, s, main)

	if !policyTrusted(t, s, wt) {
		t.Error("a linked worktree with the approved config is not trusted for policy")
	}
	if !tasksTrusted(t, s, wt) {
		t.Error("a linked worktree with the approved task commands is not trusted for tasks")
	}
	merged, err := LoadProject(Defaults(), wt)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if !merged.Git.AllowPush {
		t.Error("the worktree's session gets the global allow_push, not the approved project value")
	}
}

// TestTrust_LinkedWorktreeWithChangedConfigIsNotTrusted: the worktree's branch
// can carry a different config. A grant is bound to content, so a changed
// capability request is not honoured, and the status says it needs trust.
func TestTrust_LinkedWorktreeWithChangedConfigIsNotTrusted(t *testing.T) {
	main, wt := worktreeFixture(t)
	s := tempTrustStore(t)
	trustForProject(t, s, main)

	writeProjectConfig(t, wt, "[git]\nallow_push = true\nallow_destructive = true\nprotected_branches = []\n\n"+
		"[tasks.go]\nbuild = \"make pwn\"\n")
	gitCmd(t, wt, "commit", "-q", "-am", "widen the grant on this branch")

	if policyTrusted(t, s, wt) {
		t.Error("a worktree whose branch widened [git] inherited the main checkout's grant")
	}
	if tasksTrusted(t, s, wt) {
		t.Error("a worktree whose branch rewrote a task command inherited the main checkout's grant")
	}
	st, err := ProjectPolicyStatusFor(wt)
	if err != nil {
		t.Fatal(err)
	}
	if !st.NeedsTrust() {
		t.Error("the changed worktree config is not reported as needing trust")
	}
	merged, err := LoadProject(Defaults(), wt)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.Git.AllowDestructive || merged.Git.AllowPush {
		t.Errorf("the changed worktree config was applied: %+v", merged.Git)
	}
	// The main checkout's own grant is untouched by the worktree's change.
	if !policyTrusted(t, s, main) {
		t.Error("the main checkout lost its grant")
	}
}

// TestTrust_IdenticalConfigInAnotherRepositoryIsNotTrusted: the grant is for
// this repository's content. A different repository shipping byte-identical
// config is a different surface — its task commands run its own scripts.
func TestTrust_IdenticalConfigInAnotherRepositoryIsNotTrusted(t *testing.T) {
	main, _ := worktreeFixture(t)
	s := tempTrustStore(t)
	trustForProject(t, s, main)

	other := filepath.Join(realTempDir(t), "other")
	initConfigRepo(t, other, worktreeTrustConfig)
	if policyTrusted(t, s, other) || tasksTrusted(t, s, other) {
		t.Error("an unrelated repository with identical config inherited the grant")
	}
	// Its linked worktree is a genuine worktree — of the wrong repository.
	otherWT := filepath.Join(other, ".claude", "worktrees", "wt")
	gitCmd(t, other, "worktree", "add", "-q", "-b", "feature", otherWT, "main")
	if policyTrusted(t, s, otherWT) || tasksTrusted(t, s, otherWT) {
		t.Error("a worktree of an unrelated repository with identical config inherited the grant")
	}
}

// TestTrust_ForgedWorktreeLinkIsNotTrusted: a .git FILE is just text, and any
// directory can carry one naming a trusted repository's worktree. Git's own
// back-link (<gitdir>/gitdir), which lives inside the trusted repository, has to
// name this directory before it counts as that repository's worktree.
func TestTrust_ForgedWorktreeLinkIsNotTrusted(t *testing.T) {
	main, _ := worktreeFixture(t)
	s := tempTrustStore(t)
	trustForProject(t, s, main)

	forged := filepath.Join(realTempDir(t), "forged")
	writeProjectConfig(t, forged, worktreeTrustConfig)
	link := "gitdir: " + filepath.Join(main, ".git", "worktrees", "wt") + "\n"
	if err := os.WriteFile(filepath.Join(forged, ".git"), []byte(link), 0o600); err != nil {
		t.Fatal(err)
	}
	if policyTrusted(t, s, forged) || tasksTrusted(t, s, forged) {
		t.Error("a directory with a forged .git link inherited the trusted repository's grant")
	}
}

// TestTrust_WorktreeOfATrustedSubmoduleSharesItsGrant is the shape the report
// came from: the trusted checkout is itself a submodule (its .git is a file
// naming <super>/.git/modules/<name>), and the worktree is one of the
// submodule's.
func TestTrust_WorktreeOfATrustedSubmoduleSharesItsGrant(t *testing.T) {
	requireGitBinary(t)
	tree := realTempDir(t)
	upstream := filepath.Join(tree, "upstream")
	initConfigRepo(t, upstream, worktreeTrustConfig)
	super := filepath.Join(tree, "super")
	initConfigRepo(t, super, "")
	gitCmd(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", upstream, "sub")
	sub := filepath.Join(super, "sub")
	if data, err := os.ReadFile(filepath.Join(sub, ".git")); err != nil || !strings.HasPrefix(string(data), "gitdir:") {
		t.Fatalf("the submodule's .git is not a link file (%q, %v); the fixture no longer has the reported shape", data, err)
	}
	wt := filepath.Join(sub, ".claude", "worktrees", "wt")
	gitCmd(t, sub, "worktree", "add", "-q", "-b", "feature", wt, "HEAD")

	s := tempTrustStore(t)
	trustForProject(t, s, sub)
	if !policyTrusted(t, s, wt) || !tasksTrusted(t, s, wt) {
		t.Error("a worktree of a trusted submodule checkout did not share its grant")
	}
}
