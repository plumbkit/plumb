package tools

// git_defaultrepo_test.go — the repo argument is resolved against the pinned
// workspace, never against the daemon's working directory.

import "testing"

// TestDefaultRepo_AnchorsRelativeToWorkspace covers the field report where
// `git diff -- ism-viewer.html` with repo:"ism-viewer.html" was refused as "in a
// different project", even though the file sat at the root of the pinned
// workspace. A relative repo used to reach checkBoundary unresolved, where
// filepath.Abs anchored it to the daemon cwd.
func TestDefaultRepo_AnchorsRelativeToWorkspace(t *testing.T) {
	ws := "/Users/dev/proj"
	cases := []struct {
		name      string
		repo      string
		workspace string
		want      string
	}{
		{name: "empty repo becomes the workspace", repo: "", workspace: ws, want: ws},
		{name: "bare filename anchors to the workspace", repo: "ism-viewer.html", workspace: ws, want: ws + "/ism-viewer.html"},
		{name: "relative dir anchors to the workspace", repo: "sub/dir", workspace: ws, want: ws + "/sub/dir"},
		{name: "absolute repo is untouched", repo: "/other/repo", workspace: ws, want: "/other/repo"},
		{name: "unpinned empty repo stays empty so checkBoundary refuses", repo: "", workspace: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &Git{deps: WriteDeps{WorkspaceFn: func() string { return c.workspace }}}
			if got := g.defaultRepo(c.repo); got != c.want {
				t.Fatalf("defaultRepo(%q) with workspace %q = %q, want %q", c.repo, c.workspace, got, c.want)
			}
		})
	}
}

// TestDefaultRepo_NilWorkspaceFn preserves the zero-value WriteDeps{} contract
// used across the tools unit tests.
func TestDefaultRepo_NilWorkspaceFn(t *testing.T) {
	g := &Git{deps: WriteDeps{}}
	if got := g.defaultRepo(""); got != "" {
		t.Fatalf("defaultRepo(\"\") with nil WorkspaceFn = %q, want \"\"", got)
	}
	if got := g.defaultRepo("/abs/repo"); got != "/abs/repo" {
		t.Fatalf("absolute repo mangled to %q", got)
	}
}
