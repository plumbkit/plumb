package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// initTestRepo creates a temporary git repository with one initial commit.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test User")
	_ = os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init\n"), 0o644)
	run("add", "init.txt")
	run("commit", "-m", "initial commit")
	return dir
}

func callGit(t *testing.T, tool *Git, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tool.Execute(context.Background(), raw)
}

// --- tier classification (pure unit, no git binary needed) ---

func TestClassifyGit(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
		want gitTier
	}{
		{"status", nil, tierRead},
		{"log", []string{"--oneline"}, tierRead},
		{"diff", []string{"HEAD"}, tierRead},
		{"shortlog", nil, tierRead},
		{"check-ignore", []string{"node_modules"}, tierRead},
		{"add", []string{}, tierWrite},
		{"commit", nil, tierWrite},
		{"mv", []string{"a", "b"}, tierWrite},
		{"switch", []string{"main"}, tierWrite},
		{"switch", []string{"-f", "main"}, tierDestructive},
		{"restore", []string{"--staged", "f"}, tierWrite},
		{"restore", []string{"f"}, tierDestructive},
		{"restore", []string{"--staged", "--worktree", "f"}, tierDestructive},
		{"branch", nil, tierRead},
		{"branch", []string{"--list"}, tierRead},
		{"branch", []string{"feature"}, tierWrite},
		{"branch", []string{"-m", "old", "new"}, tierWrite},
		{"branch", []string{"-D", "old"}, tierDestructive},
		{"tag", nil, tierRead},
		{"tag", []string{"-l"}, tierRead},
		{"tag", []string{"v1.0"}, tierWrite},
		{"tag", []string{"-d", "v1.0"}, tierDestructive},
		{"stash", nil, tierWrite},
		{"stash", []string{"list"}, tierRead},
		{"stash", []string{"pop"}, tierWrite},
		{"stash", []string{"drop"}, tierDestructive},
		{"checkout", []string{"-b", "new"}, tierWrite},
		{"checkout", []string{"--", "file.go"}, tierDestructive},
		{"checkout", []string{"main"}, tierDestructive},
		{"reset", []string{"--hard"}, tierDestructive},
		{"clean", []string{"-fd"}, tierDestructive},
		{"rebase", []string{"main"}, tierDestructive},
		{"push", nil, tierNetwork},
		{"fetch", nil, tierNetwork},
		{"pull", nil, tierNetwork},
		{"merge", []string{"main"}, tierReject},
		{"rm", []string{"f"}, tierReject},
		{"filter-branch", nil, tierReject},
		{"config", []string{"core.pager", "x"}, tierReject},
		{"clone", []string{"u"}, tierReject},
		{"submodule", nil, tierReject},
	}
	for _, c := range cases {
		if got := classifyGit(c.sub, c.args); got != c.want {
			t.Errorf("classifyGit(%q, %v) = %d, want %d", c.sub, c.args, got, c.want)
		}
	}
}

// --- global-flag denylist ---

func TestGit_GlobalFlagDenylist(t *testing.T) {
	tool := NewGit(WriteDeps{}, nil)
	for _, args := range [][]string{
		{"--git-dir=/tmp/x"},
		{"--upload-pack", "sh"},
		{"--exec-path=/x"},
		{"--receive-pack=sh -c x"},
		{"-c", "core.pager=sh"},
		{"-C", "/tmp"},
	} {
		_, err := callGit(t, tool, map[string]any{"subcommand": "status", "args": args})
		if err == nil || !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("args %v: expected 'not permitted' error, got %v", args, err)
		}
	}
}

func TestGit_StashUnknownSubSubcommand(t *testing.T) {
	tool := NewGit(WriteDeps{}, nil)
	_, err := callGit(t, tool, map[string]any{"subcommand": "stash", "args": []string{"branch"}})
	if err == nil || !strings.Contains(err.Error(), "sub-command") || !strings.Contains(err.Error(), "branch") {
		t.Fatalf("expected helpful stash sub-command error, got %v", err)
	}
}

// --- gating matrix (pure unit) ---

func TestGateGit(t *testing.T) {
	allOff := GitPolicy{}
	writes := GitPolicy{AllowWrites: true}
	destr := GitPolicy{AllowWrites: true, AllowDestructive: true}
	push := GitPolicy{AllowWrites: true, AllowPush: true}

	cases := []struct {
		name    string
		tier    gitTier
		p       GitPolicy
		confirm bool
		wantErr string // "" = no error
	}{
		{"read always", tierRead, allOff, false, ""},
		{"write disabled", tierWrite, allOff, false, "write operations are disabled"},
		{"write enabled", tierWrite, writes, false, ""},
		{"destructive disabled", tierDestructive, writes, true, "destructive operations are disabled"},
		{"destructive needs confirm", tierDestructive, destr, false, "requires confirm"},
		{"destructive ok", tierDestructive, destr, true, ""},
		{"network disabled", tierNetwork, writes, true, "network operations"},
		{"network needs confirm", tierNetwork, push, false, "requires confirm"},
		{"network ok", tierNetwork, push, true, ""},
		{"reject", tierReject, push, true, "not permitted"},
	}
	for _, c := range cases {
		err := gateGit(c.tier, c.p, c.confirm)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}

// --- push protection (pure unit) ---

func TestCheckPushProtection(t *testing.T) {
	p := GitPolicy{ProtectedBranches: []string{"main", "master"}}
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"normal push", []string{"origin", "feature"}, ""},
		{"force non-protected", []string{"-f", "origin", "feature"}, ""},
		{"force protected", []string{"--force", "origin", "main"}, "force-pushing protected"},
		{"force-with-lease protected", []string{"--force-with-lease", "origin", "master"}, "force-pushing protected"},
		{"ad-hoc https url", []string{"https://evil.example/x", "main"}, "ad-hoc URL"},
		{"ad-hoc scp url", []string{"git@evil.example:x/y", "main"}, "ad-hoc URL"},
		{"ext url", []string{"ext::sh -c id", "main"}, "ad-hoc URL"},
	}
	for _, c := range cases {
		a := gitToolArgs{Subcommand: "push", Args: c.args}
		err := checkPushProtection(a, p, tierNetwork)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}

// --- Execute gating (no repo needed; gate runs before exec) ---

func TestGit_WriteBlockedWhenDisabled(t *testing.T) {
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: false} })
	_, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "x"})
	if err == nil || !strings.Contains(err.Error(), "write operations are disabled") {
		t.Fatalf("expected write-disabled error, got %v", err)
	}
}

func TestGit_RejectsUnknownSubcommand(t *testing.T) {
	tool := NewGit(WriteDeps{}, nil)
	_, err := callGit(t, tool, map[string]any{"subcommand": "merge", "args": []string{"main"}})
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected not-permitted error, got %v", err)
	}
}

func TestGit_AddRequiresFiles(t *testing.T) {
	tool := NewGit(WriteDeps{}, nil) // nil policy → writes allowed
	// repo supplied so the call clears the fail-closed boundary check and reaches
	// argv-shape validation (nil Boundary admits any non-empty path).
	_, err := callGit(t, tool, map[string]any{"subcommand": "add", "repo": t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "at least one path is required") {
		t.Fatalf("expected files-required error, got %v", err)
	}
}

func TestGit_CommitRequiresMessage(t *testing.T) {
	tool := NewGit(WriteDeps{}, nil)
	_, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "   ", "repo": t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "message is required") {
		t.Fatalf("expected message-required error, got %v", err)
	}
}

// --- fail-closed repo resolution: the cross-session leak regression ---

// TestGit_RefusesWhenNoRepoResolved is the core security regression: a git call
// with no "repo" arg on a connection whose workspace is unresolved (WorkspaceFn
// returns "") must REFUSE rather than fall through to the daemon's cwd — which
// is shared across connections and may be another project's repository.
func TestGit_RefusesWhenNoRepoResolved(t *testing.T) {
	calls := 0
	deps := WriteDeps{WorkspaceFn: func() string { calls++; return "" }}
	tool := NewGit(deps, nil)
	out, err := callGit(t, tool, map[string]any{"subcommand": "status"})
	if err == nil || !strings.Contains(err.Error(), "no repository resolved") {
		t.Fatalf("expected fail-closed 'no repository resolved' error, got out=%q err=%v", out, err)
	}
	// The error should be self-explaining about the common cause (a daemon
	// restart clearing the pin) and the fix, so a previously-working session
	// isn't left guessing why git suddenly can't resolve a repo.
	for _, want := range []string{"session_start", "restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no-repo error should mention %q: %v", want, err)
		}
	}
}

func TestNoWorkspaceError_SelfExplaining(t *testing.T) {
	msg := noWorkspaceError().Error()
	for _, want := range []string{"no workspace resolved", "session_start", "restart"} {
		if !strings.Contains(msg, want) {
			t.Errorf("noWorkspaceError should mention %q: %q", want, msg)
		}
	}
}

// TestGit_DefaultRepoFollowsWorkspace asserts an omitted repo resolves to the
// connection's pinned workspace (not the cwd, not another connection's root).
func TestGit_DefaultRepoFollowsWorkspace(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	deps := WriteDeps{WorkspaceFn: func() string { return dir }}
	tool := NewGit(deps, nil)
	if _, err := callGit(t, tool, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("status against pinned workspace should succeed, got %v", err)
	}
}

// TestGit_DefaultRepoBoundaryEnforced asserts the resolved default repo is
// boundary-checked: a WorkspaceFn pointing outside this connection's PathPolicy
// is rejected (the check now always runs, never skipped for an empty arg).
func TestGit_DefaultRepoBoundaryEnforced(t *testing.T) {
	outside := t.TempDir()
	deps := WriteDeps{
		WorkspaceFn: func() string { return outside },
		Boundary:    func(string) error { return fmt.Errorf("workspace boundary violation") },
	}
	tool := NewGit(deps, nil)
	_, err := callGit(t, tool, map[string]any{"subcommand": "status"})
	if err == nil || !strings.Contains(err.Error(), "boundary") {
		t.Fatalf("expected boundary rejection for out-of-policy default repo, got %v", err)
	}
}

// --- read subcommands against the live repo ---

func TestGit_ReadSubcommands(t *testing.T) {
	requireGit(t)
	tool := NewGit(WriteDeps{}, nil)
	for _, c := range []struct {
		sub  string
		args []string
	}{
		{"status", nil},
		{"log", []string{"--oneline", "-5"}},
		{"diff", []string{"HEAD"}},
		{"show", []string{"--stat", "HEAD"}},
	} {
		args := map[string]any{"subcommand": c.sub, "repo": "."}
		if c.args != nil {
			args["args"] = c.args
		}
		if _, err := callGit(t, tool, args); err != nil {
			t.Errorf("git %s: %v", c.sub, err)
		}
	}
}

func TestGit_InvalidRepo(t *testing.T) {
	requireGit(t)
	tool := NewGit(WriteDeps{}, nil)
	_, err := callGit(t, tool, map[string]any{"subcommand": "status", "repo": os.TempDir()})
	if err == nil {
		t.Fatal("expected error for non-repo path")
	}
}

// --- add + commit happy path against a temp repo ---

func TestGit_AddAndCommit(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	newFile := filepath.Join(dir, "new.txt")
	_ = os.WriteFile(newFile, []byte("content\n"), 0o644)

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	out, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{newFile}, "repo": dir})
	if err != nil {
		t.Fatalf("git add: %v", err)
	}
	if !strings.Contains(out, "staged 1 file") {
		t.Errorf("expected staged summary, got %q", out)
	}

	out, err = callGit(t, tool, map[string]any{"subcommand": "commit", "message": "add new.txt", "repo": dir})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if !strings.Contains(out, "add new.txt") {
		t.Errorf("expected subject in output, got %q", out)
	}
	hash := strings.SplitN(out, " ", 2)[0]
	if len(hash) != 7 {
		t.Errorf("expected 7-char short hash, got %q in %q", hash, out)
	}
}

// --- path-limited commit: commit only named paths, leaving unrelated staged work ---

func TestGit_PathLimitedCommit(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	mine := filepath.Join(dir, "mine.txt")
	peer := filepath.Join(dir, "peer.txt")
	_ = os.WriteFile(mine, []byte("mine\n"), 0o644)
	_ = os.WriteFile(peer, []byte("peer\n"), 0o644)

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	// Stage BOTH files (as a broad `git add` or a peer's staging would).
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{mine, peer}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}

	// Commit ONLY mine.txt via the path-limited form.
	if _, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "add only mine", "files": []string{mine}, "repo": dir}); err != nil {
		t.Fatalf("path-limited commit: %v", err)
	}

	// The commit must contain mine.txt and NOT peer.txt.
	show, err := callGit(t, tool, map[string]any{"subcommand": "show", "args": []string{"--stat", "--name-only", "HEAD"}, "repo": dir})
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	if !strings.Contains(show, "mine.txt") {
		t.Errorf("commit should contain mine.txt:\n%s", show)
	}
	if strings.Contains(show, "peer.txt") {
		t.Errorf("path-limited commit must NOT contain peer.txt:\n%s", show)
	}

	// peer.txt must remain staged in the index, untouched.
	staged, err := callGit(t, tool, map[string]any{"subcommand": "diff", "args": []string{"--cached", "--name-only"}, "repo": dir})
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if !strings.Contains(staged, "peer.txt") {
		t.Errorf("peer.txt should still be staged after a path-limited commit:\n%s", staged)
	}
}

// --- check-ignore: a read-tier query of gitignore status ---

func TestGit_CheckIgnore(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.log\n"), 0o644)

	tool := NewGit(WriteDeps{}, nil)

	// An ignored path is echoed back.
	out, err := callGit(t, tool, map[string]any{"subcommand": "check-ignore", "args": []string{"ignored.log"}, "repo": dir})
	if err != nil {
		t.Fatalf("check-ignore (ignored path): %v", err)
	}
	if !strings.Contains(out, "ignored.log") {
		t.Errorf("expected ignored.log in output, got %q", out)
	}

	// A non-ignored path yields the friendly empty result (git exits 1 here).
	out, err = callGit(t, tool, map[string]any{"subcommand": "check-ignore", "args": []string{"kept.txt"}, "repo": dir})
	if err != nil {
		t.Fatalf("check-ignore (non-ignored path) must not be an error: %v", err)
	}
	if !strings.Contains(out, "none of the listed paths") {
		t.Errorf("expected friendly no-match result, got %q", out)
	}
}

// --- gitWorkingTreeSummary: session_start diffstat of uncommitted tracked changes ---

func TestGitWorkingTreeSummary(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)

	// Clean tree → empty summary.
	if s := gitWorkingTreeSummary(dir, 12); s != "" {
		t.Errorf("clean tree should yield empty summary, got %q", s)
	}

	// Modify a tracked file → it appears in the diffstat.
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := gitWorkingTreeSummary(dir, 12)
	if !strings.Contains(s, "init.txt") {
		t.Errorf("expected init.txt in working-tree summary, got %q", s)
	}
}

// --- rate limit: write tiers consume a slot, reads do not ---

func TestGit_RateLimitWritesOnly(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	deps := WriteDeps{Limiter: NewRateLimiter(1, time.Minute)}
	tool := NewGit(deps, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	// Reads do not consume the limiter, even repeated.
	for range 3 {
		if _, err := callGit(t, tool, map[string]any{"subcommand": "status", "repo": dir}); err != nil {
			t.Fatalf("read consumed limiter or failed: %v", err)
		}
	}

	f1 := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(f1, []byte("a\n"), 0o644)
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{f1}, "repo": dir}); err != nil {
		t.Fatalf("first write should pass rate limit: %v", err)
	}
	// Second write exhausts the single slot.
	f2 := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(f2, []byte("b\n"), 0o644)
	_, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{f2}, "repo": dir})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "rate") {
		t.Fatalf("expected rate-limit error on second write, got %v", err)
	}
}

// TestGit_AddStagesTrackedDeletion proves the -A semantics: staging a tracked
// file that has been deleted from disk succeeds (and stages the removal). The
// pre-0.7.26 "add -- <path>" form failed here with "pathspec did not match".
func TestGit_AddStagesTrackedDeletion(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	f := filepath.Join(dir, "doomed.txt")
	if err := os.WriteFile(f, []byte("bye\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{f}, "repo": dir}); err != nil {
		t.Fatalf("git add (create): %v", err)
	}
	if _, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "add doomed", "repo": dir}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	out, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{f}, "repo": dir})
	if err != nil {
		t.Fatalf("git add of a tracked deletion should succeed with -A semantics, got: %v", err)
	}
	if !strings.Contains(out, "D\t") || !strings.Contains(out, "doomed.txt") {
		t.Errorf("expected the deletion to be staged (D\tdoomed.txt), got: %q", out)
	}
}

// TestGit_RmRejectedWithDeleteFileHint asserts the actionable rejection message
// for git rm (not just the tier classification) points the agent at delete_file.
func TestGit_RmRejectedWithDeleteFileHint(t *testing.T) {
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	_, err := callGit(t, tool, map[string]any{"subcommand": "rm", "args": []string{"some.txt"}})
	if err == nil {
		t.Fatal("git rm should be rejected")
	}
	if !strings.Contains(err.Error(), "delete_file") {
		t.Errorf("rm rejection should point to delete_file, got: %v", err)
	}
}
