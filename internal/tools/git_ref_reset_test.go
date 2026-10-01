package tools

import (
	"errors"
	"strings"
	"testing"
)

// git_ref_reset_test.go pins that resetting an EXISTING ref is destructive
// while creating a new one with the same flags stays a write (#540 review).

func TestRefineRefReset(t *testing.T) {
	existing := map[string]bool{"refs/heads/main": true, "refs/tags/v1": true}
	known := func(ref string) (bool, error) { return existing[ref], nil }
	broken := func(string) (bool, error) { return false, errors.New("git: not available") }
	cases := []struct {
		sub   string
		args  []string
		probe func(string) (bool, error)
		want  gitTier
	}{
		// The ref exists: resetting it discards commits, like `reset --keep`.
		{"switch", []string{"--force-create", "main"}, known, tierDestructive},
		{"switch", []string{"--force-create=main"}, known, tierDestructive},
		{"checkout", []string{"-B", "main", "HEAD~5"}, known, tierDestructive},
		{"branch", []string{"-f", "main", "HEAD~1"}, known, tierDestructive},
		{"tag", []string{"-f", "v1"}, known, tierDestructive},
		// A new ref: routine creation.
		{"switch", []string{"--force-create", "feat"}, known, tierWrite},
		{"switch", []string{"--force-create=feat", "origin/main"}, known, tierWrite},
		{"checkout", []string{"-B", "feat"}, known, tierWrite},
		{"checkout", []string{"-B", "feat", "origin/main"}, known, tierWrite},
		{"branch", []string{"-f", "feat"}, known, tierWrite},
		{"branch", []string{"--force", "feat", "main"}, known, tierWrite},
		{"tag", []string{"-f", "v9"}, known, tierWrite},
		{"tag", []string{"-f", "-m", "v1", "v9"}, known, tierWrite},
		{"tag", []string{"--for", "v9"}, known, tierWrite},
		// git cannot be asked: fail safe.
		{"switch", []string{"--force-create", "feat"}, broken, tierDestructive},
		{"checkout", []string{"-B", "feat"}, broken, tierDestructive},
		{"branch", []string{"-f", "feat"}, broken, tierDestructive},
		{"tag", []string{"-f", "v9"}, broken, tierDestructive},
		{"checkout", []string{"-B", "feat"}, nil, tierDestructive},
		// Something else on the call is destructive, or the form is not one this
		// parses confidently: the ref's novelty cannot lower it.
		{"switch", []string{"--force-create", "feat", "--discard-changes"}, known, tierDestructive},
		{"switch", []string{"-f", "--force-create", "feat"}, known, tierDestructive},
		{"checkout", []string{"-B", "feat", "-f"}, known, tierDestructive},
		{"branch", []string{"-f", "-d", "feat"}, known, tierDestructive},
		{"branch", []string{"-fv", "feat"}, known, tierDestructive},
		{"tag", []string{"-f", "-d", "v9"}, known, tierDestructive},
	}
	for _, c := range cases {
		got := refineRefReset(c.sub, c.args, classifyGit(c.sub, c.args), c.probe)
		if got != c.want {
			t.Errorf("%s %q: tier %s, want %s", c.sub, c.args, gitTierNames[got], gitTierNames[c.want])
		}
	}
}

// TestRefResetForm_NamesTheRefItWouldReset: the probe must ask about the ref
// the call would actually reset — the name, not the start point or a message.
func TestRefResetForm_NamesTheRefItWouldReset(t *testing.T) {
	for _, c := range []struct {
		sub  string
		args []string
		ref  string
	}{
		{"switch", []string{"--force-create", "feat", "main"}, "refs/heads/feat"},
		{"switch", []string{"--force-create=feat"}, "refs/heads/feat"},
		{"checkout", []string{"-B", "feat", "main"}, "refs/heads/feat"},
		{"branch", []string{"-f", "feat", "main"}, "refs/heads/feat"},
		{"branch", []string{"--track", "-f", "feat", "origin/x"}, "refs/heads/feat"},
		{"tag", []string{"-f", "-m", "v1", "v9", "HEAD~1"}, "refs/tags/v9"},
	} {
		if _, ref, ok := refResetForm(c.sub, c.args); !ok || ref != c.ref {
			t.Errorf("%s %q: ref %q (ok=%v), want %q", c.sub, c.args, ref, ok, c.ref)
		}
	}
}

// TestGit_ForceCreateTierFollowsTheRef is the end-to-end form: under the
// default policy (writes on, destructive off), creating a branch or tag with a
// reset flag works, and resetting an existing one is refused as destructive.
func TestGit_ForceCreateTierFollowsTheRef(t *testing.T) {
	repo := mergeFixture(t)
	runGitDirect(t, repo, "tag", "v1")
	tool := writesOnlyGit(repo)
	for _, args := range []map[string]any{
		{"subcommand": "checkout", "args": []string{"-B", "fresh"}},
		{"subcommand": "switch", "args": []string{"-C", "fresher"}},
		{"subcommand": "branch", "args": []string{"-f", "newer", "main"}},
		{"subcommand": "tag", "args": []string{"-f", "v2"}},
	} {
		if _, err := callGit(t, tool, args); err != nil {
			t.Errorf("%v creating a new ref: %v", args["args"], err)
		}
	}
	for _, args := range []map[string]any{
		{"subcommand": "checkout", "args": []string{"-B", "side", "main"}},
		{"subcommand": "switch", "args": []string{"-C", "main", "side"}},
		{"subcommand": "branch", "args": []string{"-f", "side", "main"}},
		{"subcommand": "tag", "args": []string{"-f", "v1", "side"}},
	} {
		_, err := callGit(t, tool, args)
		if err == nil || !strings.Contains(err.Error(), "destructive operations are disabled") {
			t.Errorf("%v resetting an existing ref: want the destructive-tier refusal, got %v", args["args"], err)
		}
	}
}
