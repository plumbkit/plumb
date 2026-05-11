package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func TestGit_DisallowedSubcommand(t *testing.T) {
	requireGit(t)
	tool := NewGit()
	for _, sub := range []string{"commit", "push", "reset", "checkout", "merge", "rm", "add"} {
		raw, _ := json.Marshal(map[string]any{"subcommand": sub})
		_, err := tool.Execute(context.Background(), raw)
		if err == nil {
			t.Errorf("subcommand %q: expected error, got nil", sub)
		}
		if !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("subcommand %q: unexpected error: %v", sub, err)
		}
	}
}

func TestGit_Status(t *testing.T) {
	requireGit(t)
	// Run status against this repo (we know we're inside one).
	tool := NewGit()
	raw, _ := json.Marshal(map[string]any{
		"subcommand": "status",
		"repo":       ".",
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty output")
	}
}

func TestGit_Log(t *testing.T) {
	requireGit(t)
	tool := NewGit()
	raw, _ := json.Marshal(map[string]any{
		"subcommand": "log",
		"args":       []string{"--oneline", "-5"},
		"repo":       ".",
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if out == "" || out == "(no output)" {
		t.Fatal("expected commit log output")
	}
}

func TestGit_Diff(t *testing.T) {
	requireGit(t)
	tool := NewGit()
	// Diff with no unstaged changes should produce "(no output)" or a valid diff.
	raw, _ := json.Marshal(map[string]any{
		"subcommand": "diff",
		"args":       []string{"HEAD"},
		"repo":       ".",
	})
	_, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("git diff HEAD: %v", err)
	}
}

func TestGit_Show(t *testing.T) {
	requireGit(t)
	tool := NewGit()
	raw, _ := json.Marshal(map[string]any{
		"subcommand": "show",
		"args":       []string{"--stat", "HEAD"},
		"repo":       ".",
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	if out == "" {
		t.Fatal("expected output")
	}
}

func TestGit_InvalidRepo(t *testing.T) {
	requireGit(t)
	tool := NewGit()
	raw, _ := json.Marshal(map[string]any{
		"subcommand": "status",
		"repo":       os.TempDir(),
	})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for non-repo path")
	}
}

func TestGit_FileRepo(t *testing.T) {
	requireGit(t)
	// Passing a file path should resolve to its repo root.
	tool := NewGit()
	raw, _ := json.Marshal(map[string]any{
		"subcommand": "log",
		"args":       []string{"--oneline", "-1"},
		"repo":       "git.go", // file in this package
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("git log with file repo: %v", err)
	}
	if out == "" || out == "(no output)" {
		t.Fatal("expected at least one commit")
	}
}
