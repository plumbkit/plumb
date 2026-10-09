package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git_worktree_test.go covers PLAN-454's worktree half: the tier of each
// sub-verb, the acceptance that the newly readable calls mutate nothing, and the
// confirm guard on the one form that destroys work.

func TestClassifyWorktree_Tiers(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want gitTier
	}{
		{"list is a read", []string{"list"}, tierRead},
		{"list --porcelain is a read", []string{"list", "--porcelain"}, tierRead},
		{"add is a write", []string{"add", "wt"}, tierWrite},
		{"add -b is a write", []string{"add", "-b", "topic", "wt"}, tierWrite},
		{"add -B resets a branch, so destructive", []string{"add", "-B", "topic", "wt"}, tierDestructive},
		{"add --force-create resets a branch", []string{"add", "--force-create", "topic", "wt"}, tierDestructive},
		{"add -f lets two worktrees share a branch, so destructive", []string{"add", "-f", "wt"}, tierDestructive},
		{"add --force is destructive for the same reason", []string{"add", "--force", "wt"}, tierDestructive},
		{"remove is a write", []string{"remove", "wt"}, tierWrite},
		{"lock is a write", []string{"lock", "wt"}, tierWrite},
		{"unlock is a write", []string{"unlock", "wt"}, tierWrite},
		{"move rewrites administration", []string{"move", "wt", "wt2"}, tierDestructive},
		{"prune deletes administration", []string{"prune"}, tierDestructive},
		{"repair rewrites administration", []string{"repair"}, tierDestructive},
		{"a bare worktree is refused", nil, tierReject},
		{"an unknown sub-verb is refused", []string{"clone"}, tierReject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyWorktree(tc.args); got != tc.want {
				t.Errorf("classifyWorktree(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestGit_WorktreeReadsMutateNothing is the card's acceptance for the newly
// readable calls: each runs through plumb's git tool and leaves HEAD, the index
// and the working tree exactly as they were. merge-tree is included because it
// is the one that can write unreferenced objects into the object store — a
// reviewer's question must not be able to move a ref or dirty a worktree.
func TestGit_WorktreeReadsMutateNothing(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	// A second branch and a linked worktree, so `worktree list` has real content
	// to report rather than a single entry it could get right by accident.
	linked := filepath.Join(t.TempDir(), "linked")
	gitRun(t, dir, "worktree", "add", "-b", "topic", linked)

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	calls := []struct {
		name string
		args map[string]any
	}{
		{"merge-tree", map[string]any{
			"subcommand": "merge-tree",
			"args":       []string{"--write-tree", "HEAD", "topic"}, "repo": dir,
		}},
		{"check-attr", map[string]any{
			"subcommand": "check-attr",
			"args":       []string{"-a", "init.txt"}, "repo": dir,
		}},
		{"worktree list", map[string]any{
			"subcommand": "worktree",
			"args":       []string{"list"}, "repo": dir,
		}},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			before := snapshotRepoState(t, dir)
			out, err := callGit(t, tool, call.args)
			if err != nil {
				if call.name == "merge-tree" && strings.Contains(err.Error(), "unknown option") {
					t.Skipf("this git does not support `merge-tree --write-tree`: %v", err)
				}
				t.Fatalf("%s: %v", call.name, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("%s returned nothing", call.name)
			}
			if after := snapshotRepoState(t, dir); after != before {
				t.Errorf("%s mutated the repository:\n before %+v\n after  %+v", call.name, before, after)
			}
		})
	}
}

// TestGit_WorktreeAddRemoveRoundTrip proves the write tier is not merely
// assigned: adding and removing a clean worktree through the tool works, and a
// removal git would allow (a clean worktree, confirmed) is not blocked by the
// guard.
func TestGit_WorktreeAddRemoveRoundTrip(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "round-trip")

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree",
		"args":       []string{"add", "-b", "round-trip", wt}, "repo": dir,
	}); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "init.txt")); err != nil {
		t.Fatalf("the worktree was not checked out: %v", err)
	}
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree",
		"args":       []string{"list"}, "repo": dir,
	})
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	if !strings.Contains(out, "round-trip") {
		t.Errorf("worktree list does not show the worktree just added:\n%s", out)
	}
	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree",
		"args":       []string{"remove", wt}, "repo": dir, "confirm": true,
	}); err != nil {
		t.Fatalf("worktree remove of a clean worktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("the clean worktree still exists after remove: %v", err)
	}
}

// TestGit_WorktreeRemoveGuardAsksBeforeDiscardingWork pins the guard itself:
// uncommitted work is refused with the file named, an unconfirmed --force is
// refused for the force, and confirm:true is what lets it through. The guard's
// job is to make the caller decide, not to forbid.
func TestGit_WorktreeRemoveGuardAsksBeforeDiscardingWork(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	wt := filepath.Join(t.TempDir(), "probe")
	gitRun(t, dir, "worktree", "add", "-b", "probe", wt)
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	_, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree",
		"args":       []string{"remove", wt}, "repo": dir,
	})
	if err == nil {
		t.Fatal("removing a dirty worktree without confirm must be refused")
	}
	for _, want := range []string{"uncommitted entry", "dirty.txt", "confirm:true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q; got: %v", want, err)
		}
	}
	if _, statErr := os.Stat(filepath.Join(wt, "dirty.txt")); statErr != nil {
		t.Fatalf("the refused removal still deleted work: %v", statErr)
	}

	_, err = callGit(t, tool, map[string]any{
		"subcommand": "worktree",
		"args":       []string{"remove", "--force", wt}, "repo": dir,
	})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("an unconfirmed --force = %v, want a refusal naming --force", err)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("the refused --force still removed the worktree: %v", statErr)
	}

	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree",
		"args":       []string{"remove", "--force", wt}, "repo": dir, "confirm": true,
	}); err != nil {
		t.Fatalf("confirm:true must let the removal through: %v", err)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Errorf("the worktree still exists after a confirmed removal: %v", statErr)
	}
}

// TestGit_WorktreeTierIsGated pins that the new write tier is really gated:
// with [git] allow_writes off, adding a worktree is refused rather than run.
func TestGit_WorktreeTierIsGated(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })
	_, err := callGit(t, tool, map[string]any{
		"subcommand": "worktree",
		"args":       []string{"add", "-b", "blocked", filepath.Join(t.TempDir(), "blocked")}, "repo": dir,
	})
	if err == nil || !strings.Contains(err.Error(), "allow_writes") {
		t.Fatalf("worktree add with writes disabled = %v, want the write-tier refusal", err)
	}
}

// repoSnapshot is the card's mutation witness: HEAD, the index bytes and the
// working tree. Compared by value, so any change to any of the three fails.
type repoSnapshot struct {
	head   string
	index  string
	status string
}

// snapshotRepoState reads those three without itself touching the index:
// --no-optional-locks is the same flag plumb's read tier uses, because a plain
// `git status` refreshes and rewrites .git/index, which would make the witness
// report a mutation the call under test never caused.
func snapshotRepoState(t *testing.T, dir string) repoSnapshot {
	t.Helper()
	head := strings.TrimSpace(gitReadOutput(t, dir, "rev-parse", "HEAD"))
	status := gitReadOutput(t, dir, "status", "--porcelain")
	index, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatalf("reading the index: %v", err)
	}
	return repoSnapshot{
		head:   head,
		index:  fmt.Sprintf("%x", sha256.Sum256(index)),
		status: status,
	}
}

func gitReadOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--no-optional-locks"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
