package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestBuildGitArgv_CommitTrailer pins the argv shape: the trailer option must
// precede the `--` pathspec separator of a path-limited commit, and appear
// only when one is supplied.
func TestBuildGitArgv_CommitTrailer(t *testing.T) {
	withTrailer, err := buildGitArgv(gitToolArgs{
		Subcommand: "commit", Message: "msg", Files: []string{"a.go"},
	}, "Plumb-Session: swift-falcon")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"commit", "-m", "msg", "--trailer", "Plumb-Session: swift-falcon", "--", "a.go"}
	if !slices.Equal(withTrailer, want) {
		t.Errorf("argv = %v, want %v", withTrailer, want)
	}

	without, err := buildGitArgv(gitToolArgs{Subcommand: "commit", Message: "msg"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(without, "--trailer") {
		t.Errorf("no trailer requested, got argv %v", without)
	}
}

// TestCommitTrailerToken covers the gate matrix: the knob off (the default),
// a non-commit subcommand, and a missing or blank session name all yield no
// trailer.
func TestCommitTrailerToken(t *testing.T) {
	named := NewGit(WriteDeps{}, nil).WithSession("s1", func() string { return "swift-falcon" })
	blank := NewGit(WriteDeps{}, nil).WithSession("s2", func() string { return "  " })
	anon := NewGit(WriteDeps{}, nil)
	cases := []struct {
		name string
		tool *Git
		p    GitPolicy
		sub  string
		want string
	}{
		{"knob on, commit", named, GitPolicy{CommitTrailer: true}, "commit", "Plumb-Session: swift-falcon"},
		{"knob off (default)", named, GitPolicy{}, "commit", ""},
		{"knob on, not a commit", named, GitPolicy{CommitTrailer: true}, "add", ""},
		{"knob on, no session wired", anon, GitPolicy{CommitTrailer: true}, "commit", ""},
		{"knob on, blank name", blank, GitPolicy{CommitTrailer: true}, "commit", ""},
	}
	for _, c := range cases {
		if got := c.tool.commitTrailerToken(c.p, c.sub); got != c.want {
			t.Errorf("%s: commitTrailerToken = %q, want %q", c.name, got, c.want)
		}
	}
}

// gitTrailers returns the trailer block of the repo's HEAD commit.
func gitTrailers(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", gitNoOptionalLocks, "log", "-1", "--format=%(trailers)")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log trailers: %v", err)
	}
	return string(out)
}

// addAndCommit stages and commits one file through the tool.
func addAndCommit(t *testing.T, tool *Git, repo, file, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, file), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{file}, "repo": repo}); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": message, "repo": repo}); err != nil {
		t.Fatalf("git commit: %v", err)
	}
}

// TestGit_CommitSessionTrailer is the end-to-end proof for the opt-in: with
// [git] commit_trailer on and a session name wired, the commit carries the
// Plumb-Session trailer.
func TestGit_CommitSessionTrailer(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	tool := NewGit(
		WriteDeps{WorkspaceFn: func() string { return repo }},
		func() GitPolicy { return GitPolicy{AllowWrites: true, CommitTrailer: true} },
	).WithSession("sess-1", func() string { return "swift-falcon" })

	addAndCommit(t, tool, repo, "a.txt", "attributed commit")
	if got := gitTrailers(t, repo); !strings.Contains(got, "Plumb-Session: swift-falcon") {
		t.Errorf("commit should carry the Plumb-Session trailer, trailers: %q", got)
	}
}

// TestGit_CommitSessionTrailerDefaultOff proves the default adds nothing: a
// named session committing under the stock policy leaves no trailer behind.
func TestGit_CommitSessionTrailerDefaultOff(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	tool := NewGit(
		WriteDeps{WorkspaceFn: func() string { return repo }},
		func() GitPolicy { return GitPolicy{AllowWrites: true} },
	).WithSession("sess-1", func() string { return "swift-falcon" })

	addAndCommit(t, tool, repo, "a.txt", "unattributed commit")
	if got := gitTrailers(t, repo); strings.Contains(got, "Plumb-Session") {
		t.Errorf("default policy must not stamp a trailer, trailers: %q", got)
	}
}
