package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git_read_args_test.go covers #636's three read verbs end to end and the
// read-tier option refusals that came with them: the options on read verbs that
// write a file, run a program, or read outside the repository.

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
		{"grep", []string{"-O", "foo"}},
		{"grep", []string{"-Ovim", "foo"}},
		{"grep", []string{"-nO", "foo"}},
		{"grep", []string{"--open-files-in-pager=vim", "foo"}},
		{"grep", []string{"--open", "foo"}},
		{"grep", []string{"--no-index", "foo", "/etc"}},
		{"grep", []string{"--no-ind", "foo"}},
		{"grep", []string{"-e", "x", "-O"}}, // -e took "x"; -O is live
		{"blame", []string{"--output=/tmp/x", "f"}},
		{"blame", []string{"-CL", "-S", "/etc/hosts", "f"}},         // -C took "L" as its score
		{"blame", []string{"-ML", "--contents", "/etc/hosts", "f"}}, // likewise -M
		{"grep", []string{"-f", "/etc/hosts"}},
		{"grep", []string{"-nf/etc/hosts"}},
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
		{"blame", []string{"-C", "-L", "1,5", "f"}},
		{"grep", []string{"-e", "-O", "HEAD"}}, // -e takes "-O" as the pattern
		{"grep", []string{"-e", "--no-index"}}, // likewise
		{"grep", []string{"-n", "--untracked", "x"}},
		{"cherry", []string{"-v", "origin/main", "HEAD"}},
		{"ls-files", []string{"--error-unmatch", "go.mod"}},
	}
	for _, c := range allowed {
		if err := checkReadArgs(c.sub, c.args); err != nil {
			t.Errorf("git %s %v must be allowed: %v", c.sub, c.args, err)
		}
	}
}

func TestGit_ReadVerbsRunWithTheOutputWindow(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("alpha\nneedle one\nbeta\nneedle two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "notes.txt")
	run("commit", "-m", "notes")
	run("branch", "base", "HEAD~1")
	tool := NewGit(WriteDeps{}, nil)

	// cherry: the one commit not upstream of base.
	out, err := callGit(t, tool, map[string]any{"subcommand": "cherry", "args": []string{"-v", "base", "HEAD"}, "repo": repo})
	if err != nil || !strings.Contains(out, "+ ") || !strings.Contains(out, "notes") {
		t.Fatalf("cherry: %v\n%s", err, out)
	}
	// ls-files: is a path tracked?
	out, err = callGit(t, tool, map[string]any{"subcommand": "ls-files", "args": []string{"--", "notes.txt"}, "repo": repo})
	if err != nil || strings.TrimSpace(out) != "notes.txt" {
		t.Fatalf("ls-files: %v\n%q", err, out)
	}
	// grep a historical revision, windowed by pattern like every read.
	out, err = callGit(t, tool, map[string]any{
		"subcommand": "grep", "args": []string{"-n", "needle", "HEAD"}, "repo": repo, "pattern": "two",
	})
	if err != nil || !strings.Contains(out, "needle two") || strings.Contains(out, "needle one") {
		t.Fatalf("grep with a pattern window: %v\n%s", err, out)
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

func TestGit_ReadTierRefusesAnArgumentOutsideTheRepo(t *testing.T) {
	// git diff leaves the repository with NO option at all: given two paths, one
	// outside the working tree, it switches to no-index mode and prints the
	// outside file. The structural check refuses any argument naming an existing
	// path outside the repository, however git would have used it.
	requireGit(t)
	repo := initTestRepo(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret-line-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(repo, outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	tool := NewGit(WriteDeps{}, nil)
	for _, args := range [][]string{
		{rel, "init.txt"},               // implicit no-index, relative
		{"--stat", outside, "init.txt"}, // absolute
		{"--", "init.txt", outside},     // after --
		{"escape", "init.txt"},          // a symlink inside that resolves outside
	} {
		out, err := callGit(t, tool, map[string]any{"subcommand": "diff", "args": args, "repo": repo})
		if err == nil || strings.Contains(out, "secret-line-1") {
			t.Errorf("diff %v must be refused before git runs; got %v\n%s", args, err, out)
		}
	}
	// Positive control: git really does print the outside file for the first form.
	cmd := exec.Command("git", "diff", rel, "init.txt")
	cmd.Dir = repo
	if out, _ := cmd.CombinedOutput(); !strings.Contains(string(out), "secret-line-1") {
		t.Fatalf("control: git diff did not read the outside file, so the refusal proves nothing:\n%s", out)
	}
	// Reads that stay inside are untouched: a path, a revision, a missing pathspec.
	for _, args := range [][]string{{"HEAD", "--", "init.txt"}, {"HEAD~0"}, {"--", "no/such/path"}} {
		if _, err := callGit(t, tool, map[string]any{"subcommand": "diff", "args": args, "repo": repo}); err != nil {
			t.Errorf("diff %v must run: %v", args, err)
		}
	}
}
