package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git_read_args_test.go covers the read-tier option refusals: the options on
// read verbs that write a file or read outside the repository.

func TestCheckReadArgs(t *testing.T) {
	refused := []struct {
		sub  string
		args []string
	}{
		{"diff", []string{"--output=/tmp/x", "HEAD"}},
		{"diff", []string{"--outp=/tmp/x"}},      // an abbreviation git expands
		{"diff", []string{"--output", "/tmp/x"}}, // the value as the next argument
		{"diff", []string{"--no-index", "/etc/hosts", "/dev/null"}},
		{"diff", []string{"--no-ind", "a", "b"}},
		{"log", []string{"-p", "--output=/tmp/x"}},
		{"show", []string{"--output=/tmp/x", "HEAD"}},
		{"shortlog", []string{"--output=/tmp/x", "HEAD"}},
		{"stash", []string{"show", "-p", "--output=/tmp/x"}},
		{"blame", []string{"--contents", "/etc/hosts", "f"}},
		{"blame", []string{"--cont=/etc/hosts", "f"}},
		{"blame", []string{"-S", "/etc/hosts", "f"}},
		{"blame", []string{"-wS", "/etc/hosts", "f"}}, // inside a bundle
		{"blame", []string{"--ignore-revs-file=/etc/hosts", "f"}},
	}
	for _, c := range refused {
		if err := checkReadArgs(c.sub, c.args); err == nil {
			t.Errorf("git %s %v must be refused", c.sub, c.args)
		}
		// And through the classifier every caller goes through.
		if _, err := classifyGitCall(gitToolArgs{Subcommand: c.sub, Args: c.args}); err == nil {
			t.Errorf("classifyGitCall(%s %v) must refuse", c.sub, c.args)
		}
	}

	allowed := []struct {
		sub  string
		args []string
	}{
		{"diff", []string{"--stat", "HEAD~1"}},
		{"diff", []string{"--output-indicator-new=+", "HEAD"}}, // a longer option, not an abbreviation
		{"log", []string{"--oneline", "-5"}},
		{"show", []string{"HEAD:go.mod"}},
		{"blame", []string{"-L", "1,5", "--", "f"}},
		{"blame", []string{"-L/S/,+2", "f"}}, // -L takes the rest of the token: no -S here
	}
	for _, c := range allowed {
		if err := checkReadArgs(c.sub, c.args); err != nil {
			t.Errorf("git %s %v must be allowed: %v", c.sub, c.args, err)
		}
	}
}

func TestGit_ReadTierRefusesAnOutputFile(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewGit(WriteDeps{}, nil)
	_, err := callGit(t, tool, map[string]any{"subcommand": "diff", "args": []string{"--output=" + victim, "HEAD"}, "repo": repo})
	if err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("diff --output must be refused, naming the option; got %v", err)
	}
	if data, rerr := os.ReadFile(victim); rerr != nil || string(data) != "keep me\n" {
		t.Fatalf("the refused call must not have touched the file: %q, %v", data, rerr)
	}
	// Positive control: git really would have written it, so the refusal above is
	// what protected the file, not a git that ignores the option.
	cmd := exec.Command("git", "diff", "--output="+victim, "HEAD")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("control: %v\n%s", err, out)
	}
	if data, _ := os.ReadFile(victim); string(data) == "keep me\n" {
		t.Fatal("control: git diff --output did not rewrite the file, so the refusal test proves nothing")
	}
}
