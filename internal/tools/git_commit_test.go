package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

func callGitCommit(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewGitCommit(WriteDeps{}).Execute(context.Background(), raw)
}

func TestGitCommit_MissingMessage(t *testing.T) {
	requireGit(t)
	_, err := callGitCommit(t, map[string]any{"repo": "."})
	if err == nil {
		t.Fatal("expected error for missing message")
	}
	if !strings.Contains(err.Error(), "message is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGitCommit_BlankMessage(t *testing.T) {
	requireGit(t)
	_, err := callGitCommit(t, map[string]any{"message": "   ", "repo": "."})
	if err == nil {
		t.Fatal("expected error for blank message")
	}
	if !strings.Contains(err.Error(), "message is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGitCommit_StageExplicitFile(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	newFile := filepath.Join(dir, "new.txt")
	_ = os.WriteFile(newFile, []byte("content\n"), 0o644)
	out, err := callGitCommit(t, map[string]any{
		"message": "add new.txt",
		"files":   []string{newFile},
		"repo":    dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "add new.txt") {
		t.Errorf("expected subject in output, got: %q", out)
	}
	cmd := exec.Command("git", "log", "-1", "--format=%s")
	cmd.Dir = dir
	logOut, _ := cmd.Output()
	if strings.TrimSpace(string(logOut)) != "add new.txt" {
		t.Errorf("expected commit in log, got: %q", string(logOut))
	}
}

func TestGitCommit_StageTrackedModification(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	_ = os.WriteFile(filepath.Join(dir, "init.txt"), []byte("modified\n"), 0o644)
	out, err := callGitCommit(t, map[string]any{
		"message": "modify init.txt",
		"repo":    dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "modify init.txt") {
		t.Errorf("expected subject in output, got: %q", out)
	}
}

func TestGitCommit_NothingToCommit(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	_, err := callGitCommit(t, map[string]any{
		"message": "empty",
		"repo":    dir,
	})
	if err == nil {
		t.Fatal("expected error when nothing to commit")
	}
}

func TestGitCommit_UntrackedNotAutoStaged(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	_ = os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("untracked\n"), 0o644)
	// git add -u won't pick up the untracked file, so nothing to commit.
	_, err := callGitCommit(t, map[string]any{
		"message": "should fail",
		"repo":    dir,
	})
	if err == nil {
		t.Fatal("expected error: untracked file should not be auto-staged")
	}
}

func TestGitCommit_ReturnsShortHash(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	newFile := filepath.Join(dir, "hash_test.txt")
	_ = os.WriteFile(newFile, []byte("test\n"), 0o644)
	out, err := callGitCommit(t, map[string]any{
		"message": "hash test",
		"files":   []string{newFile},
		"repo":    dir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := strings.SplitN(out, " ", 2)
	if len(parts[0]) != 7 {
		t.Errorf("expected 7-char short hash, got %q in output %q", parts[0], out)
	}
}
