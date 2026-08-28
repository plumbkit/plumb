package tools

// git_defaultrepo_test.go — the repo argument is resolved against the pinned
// workspace, never against the daemon's working directory.

import (
	"context"
	"strings"
	"testing"
)

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
			g := &Git{deps: WriteDeps{WorkspaceFn: func(context.Context) string { return c.workspace }}}
			if got, err := g.defaultRepo(context.Background(), c.repo); err != nil {
				t.Fatalf("defaultRepo(%q): %v", c.repo, err)
			} else if got != c.want {
				t.Fatalf("defaultRepo(%q) with workspace %q = %q, want %q", c.repo, c.workspace, got, c.want)
			}
		})
	}
}

// TestDefaultRepo_NilWorkspaceFn preserves the zero-value WriteDeps{} contract
// used across the tools unit tests.
// TestDefaultRepo_ContestedRefusesEmptyRepoOnly pins the git half of section 3
// (fail closed on contested): only the empty-repo fallback — which resolves to
// the pinned workspace being fought over — is refused. An explicit repo names a
// root of its own and proceeds unchanged.
func TestDefaultRepo_ContestedRefusesEmptyRepoOnly(t *testing.T) {
	contested := func() bool { return true }
	g := &Git{deps: WriteDeps{WorkspaceFn: func(context.Context) string { return "/ws" }, Contested: contested}}

	if got, err := g.defaultRepo(context.Background(), ""); err == nil {
		t.Fatalf("empty repo on contested resolved to %q, want a refusal", got)
	} else if !strings.Contains(strings.ToLower(err.Error()), "repo") {
		t.Errorf("contested empty-repo refusal does not name the repo remedy: %v", err)
	}

	if got, err := g.defaultRepo(context.Background(), "/abs/repo"); err != nil || got != "/abs/repo" {
		t.Errorf("explicit repo on contested = %q, %v; want /abs/repo, nil", got, err)
	}
}

func TestDefaultRepo_NilWorkspaceFn(t *testing.T) {
	g := &Git{deps: WriteDeps{}}
	if got, err := g.defaultRepo(context.Background(), ""); err != nil || got != "" {
		t.Fatalf("defaultRepo(\"\") with nil WorkspaceFn = %q, %v; want \"\", nil", got, err)
	}
	if got, err := g.defaultRepo(context.Background(), "/abs/repo"); err != nil || got != "/abs/repo" {
		t.Fatalf("absolute repo mangled to %q, %v", got, err)
	}
}
