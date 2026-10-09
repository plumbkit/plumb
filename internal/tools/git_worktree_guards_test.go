package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// git_worktree_guards_test.go covers the two guards review round 1 added to
// PLAN-454: the workspace boundary on the paths a worktree call creates or
// deletes (B2), and the detached HEAD that would be orphaned by a removal (B3).

// boundaryGitFixture builds a workspace holding a real git repository, and a
// deps value whose boundary is the workspace — the shape every other tool is
// tested against. The repository lives INSIDE the workspace, because the repo
// path is boundary-checked too.
func boundaryGitFixture(t *testing.T) (ws, repo string, deps WriteDeps) {
	t.Helper()
	requireGit(t)
	ws = t.TempDir()
	repo = filepath.Join(ws, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "config", "user.email", "t@example.com")
	gitRun(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f.txt")
	gitRun(t, repo, "commit", "-qm", "init")
	return ws, repo, WriteDeps{Boundary: testBoundaryGuard(ws), WorkspaceFn: func(_ context.Context) string { return ws }}
}

func TestGit_WorktreeAddOutsideTheWorkspaceIsRefused(t *testing.T) {
	ws, repo, deps := boundaryGitFixture(t)
	tool := NewGit(deps, func() GitPolicy { return GitPolicy{AllowWrites: true, AllowDestructive: true} })
	outside := filepath.Join(t.TempDir(), "far")

	_, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"add", "-b", "far", outside}, "repo": repo,
	})
	if err == nil {
		t.Fatal("creating a worktree outside the workspace must be refused")
	}
	if !IsWorkspaceBoundaryError(err) {
		t.Errorf("want a workspace-boundary refusal, got: %v", err)
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Errorf("the refused call created %s outside the workspace (%v)", outside, statErr)
	}

	// confirm is not a way to accept this on the workspace's behalf.
	_, err = callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"add", "-b", "far", outside}, "repo": repo, "confirm": true,
	})
	if err == nil || !IsWorkspaceBoundaryError(err) {
		t.Fatalf("confirm must not lift the workspace boundary, got: %v", err)
	}

	// Positive control: the same call inside the workspace works, so the guard is
	// about the boundary and not about creating worktrees at all.
	inside := filepath.Join(ws, "wt-inside")
	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"add", "-b", "inside", inside}, "repo": repo,
	}); err != nil {
		t.Fatalf("a worktree inside the workspace must be allowed: %v", err)
	}
}

func TestGit_WorktreeRemoveOutsideTheWorkspaceIsRefused(t *testing.T) {
	_, repo, deps := boundaryGitFixture(t)
	tool := NewGit(deps, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	// A worktree native git created outside the workspace: plumb must not be the
	// tool that deletes it.
	outside := filepath.Join(t.TempDir(), "native")
	gitRun(t, repo, "worktree", "add", "-b", "native", outside)

	_, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"remove", outside}, "repo": repo,
	})
	if err == nil || !IsWorkspaceBoundaryError(err) {
		t.Fatalf("removing a worktree outside the workspace = %v, want a boundary refusal", err)
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Errorf("the refused removal deleted %s (%v)", outside, statErr)
	}
}

// TestGit_WorktreeRemoveRefusesAnOrphanedDetachedHead is B3's regression: a
// detached HEAD can be the only thing naming its commits, and the reflog that
// also names them is deleted with the directory.
func TestGit_WorktreeRemoveRefusesAnOrphanedDetachedHead(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "detached")
	gitRun(t, repo, "worktree", "add", "--detach", wt, "HEAD")

	// A commit made here is named by no branch, tag or remote: only this
	// worktree's HEAD points at it.
	if err := os.WriteFile(filepath.Join(wt, "orphan.txt"), []byte("only here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "add", "orphan.txt")
	gitRun(t, wt, "-c", "user.email=t@example.com", "-c", "user.name=Test User", "commit", "-qm", "orphan")

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	_, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"remove", wt}, "repo": repo,
	})
	if err == nil {
		t.Fatal("removing a detached HEAD whose commits no ref reaches must be refused")
	}
	for _, want := range []string{"detached HEAD", "no branch, tag or remote", "confirm:true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q; got: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(wt, "orphan.txt")); statErr != nil {
		t.Fatalf("the refused removal still deleted the worktree: %v", statErr)
	}

	// --force does not help: it overrides git's protections, not this one.
	_, err = callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"remove", "--force", wt}, "repo": repo,
	})
	if err == nil || !strings.Contains(err.Error(), "detached HEAD") {
		t.Fatalf("an unconfirmed --force on an orphaned HEAD = %v, want the orphan refusal", err)
	}

	// confirm:true is the deliberate way through.
	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"remove", wt}, "repo": repo, "confirm": true,
	}); err != nil {
		t.Fatalf("confirm:true must let the removal through: %v", err)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Errorf("the worktree still exists after a confirmed removal: %v", statErr)
	}
}

// TestGit_WorktreeRemoveAllowsACleanBranchWorktree is the control for the guard
// above: a worktree on a branch has nothing orphaned, so a plain remove works
// with no confirmation at all.
func TestGit_WorktreeRemoveAllowsACleanBranchWorktree(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "on-a-branch")
	gitRun(t, repo, "worktree", "add", "-b", "topic", wt)

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree", "args": []string{"remove", wt}, "repo": repo,
	}); err != nil {
		t.Fatalf("a clean worktree on a branch needs no confirmation: %v", err)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Errorf("the worktree still exists after removal: %v", statErr)
	}
}
