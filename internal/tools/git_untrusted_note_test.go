package tools

import (
	"context"
	"strings"
	"testing"
)

// git_untrusted_note_test.go covers #530 item 2's refusal text: when a tier is
// off because this workspace's project config asked for it and is untrusted
// here, the refusal says so and names the exact `plumb trust` command, instead
// of reading as though nothing had asked for the tier at all.

func untrustedNoteGit(ws string, st ProjectGitStatus) *Git {
	return NewGit(
		WriteDeps{WorkspaceFn: func(context.Context) string { return ws }},
		func() GitPolicy { return GitPolicy{AllowWrites: true} },
	).WithProjectPolicy(func() ProjectGitStatus { return st })
}

func TestGit_TierRefusalNamesTheUntrustedProjectConfig(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	asked := ProjectGitStatus{
		Workspace: repo,
		Keys:      []ProjectGitKey{{Key: "git.allow_push", Value: true}, {Key: "git.allow_destructive", Value: true}},
	}
	_, err := callGit(t, untrustedNoteGit(repo, asked), map[string]any{"subcommand": "push", "confirm": true})
	if err == nil {
		t.Fatal("push succeeded with allow_push off")
	}
	msg := err.Error()
	for _, want := range []string{
		"network operations (push/fetch/pull) are disabled",
		"git.allow_push",
		"untrusted at " + repo,
		"plumb trust " + shellQuote(repo),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}

	_, err = callGit(t, untrustedNoteGit(repo, asked), map[string]any{"subcommand": "reset", "args": []string{"--hard"}, "confirm": true})
	if err == nil || !strings.Contains(err.Error(), "git.allow_destructive") || !strings.Contains(err.Error(), "plumb trust") {
		t.Errorf("destructive refusal does not name the untrusted request: %v", err)
	}
}

// TestGit_TierRefusalNoteOnlyWhenItIsTheExplanation holds the note to the one
// case it explains. A trusted request, a project that never asked for the
// refused tier, and no project config at all each keep the plain refusal:
// pointing at `plumb trust` there would send the reader to a grant that changes
// nothing.
func TestGit_TierRefusalNoteOnlyWhenItIsTheExplanation(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	cases := map[string]ProjectGitStatus{
		"trusted":           {Workspace: repo, Trusted: true, Keys: []ProjectGitKey{{Key: "git.allow_push", Value: true}}},
		"other key only":    {Workspace: repo, Keys: []ProjectGitKey{{Key: "git.allow_destructive", Value: true}}},
		"asked for the off": {Workspace: repo, Keys: []ProjectGitKey{{Key: "git.allow_push", Value: false}}},
		"no project config": {},
	}
	for name, st := range cases {
		_, err := callGit(t, untrustedNoteGit(repo, st), map[string]any{"subcommand": "push", "confirm": true})
		if err == nil {
			t.Fatalf("%s: push succeeded with allow_push off", name)
		}
		if strings.Contains(err.Error(), "plumb trust") {
			t.Errorf("%s: refusal points at plumb trust although that is not why the tier is off:\n%s", name, err)
		}
	}
}

// TestProjectGitNotice_NamesASharedGrantsSource: session_start says where a
// linked worktree's shared grant lives, because that is the only place it can be
// revoked; a workspace's own grant adds nothing.
func TestProjectGitNotice_NamesASharedGrantsSource(t *testing.T) {
	p := GitPolicy{AllowWrites: true, AllowPush: true}
	keys := []ProjectGitKey{{Key: "git.allow_push", Value: true}}
	shared := formatProjectGitNotice("/wt", ProjectGitStatus{Trusted: true, Keys: keys, InheritedFrom: "/main"}, p)
	if !strings.Contains(shared, "/main") || !strings.Contains(shared, "plumb trust --revoke '/main'") {
		t.Errorf("shared-grant notice does not name its source and revoke:\n%s", shared)
	}
	if own := formatProjectGitNotice("/main", ProjectGitStatus{Trusted: true, Keys: keys}, p); own != "" {
		t.Errorf("a workspace's own grant produced a notice:\n%s", own)
	}
}
