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

func stageFile(t *testing.T, dir, path string) {
	t.Helper()
	cmd := exec.Command("git", "add", "--", path)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
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

func TestGitCommit_CommitsStagedFiles(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	newFile := filepath.Join(dir, "new.txt")
	_ = os.WriteFile(newFile, []byte("content\n"), 0o644)
	stageFile(t, dir, newFile)
	out, err := callGitCommit(t, map[string]any{
		"message": "add new.txt",
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

func TestGitCommit_NothingStaged(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	_, err := callGitCommit(t, map[string]any{
		"message": "empty",
		"repo":    dir,
	})
	if err == nil {
		t.Fatal("expected error when nothing is staged")
	}
}

func TestGitCommit_ReturnsShortHash(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	newFile := filepath.Join(dir, "hash_test.txt")
	_ = os.WriteFile(newFile, []byte("test\n"), 0o644)
	stageFile(t, dir, newFile)
	out, err := callGitCommit(t, map[string]any{
		"message": "hash test",
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
