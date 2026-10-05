package tools

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// git_ref_reset_test.go pins that `checkout -B`, `switch -C` and `tag -f` are
// destructive when they reset an EXISTING ref and a write when they create a
// new one, and that every forced `git branch` form is destructive whether or
// not the name exists (#540 review: branch's option grammar defeated the
// lowering in each of four rounds, so branch is not lowered at all).

// stubRefs is a refProbe over a fixed set of existing refs. Its plainName
// answers plain, so a row isolates the allowlist and the existence check: what
// git's own name check decides is tested against real git below.
func stubRefs(existing map[string]bool, plain bool, err error) *refProbe {
	return &refProbe{
		plainName: func(string) bool { return plain },
		exists:    func(ref string) (bool, error) { return existing[ref], err },
	}
}

func TestRefineRefReset(t *testing.T) {
	existing := map[string]bool{"refs/heads/main": true, "refs/heads/side": true, "refs/tags/v1": true}
	known := stubRefs(existing, true, nil)
	broken := stubRefs(nil, true, errors.New("git: not available"))
	unplain := stubRefs(nil, false, nil) // git rejects or rewrites every name
	cases := []struct {
		sub   string
		args  []string
		probe *refProbe
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
		// branch is never lowered: a forced form is destructive for a new name too.
		{"branch", []string{"-f", "feat"}, known, tierDestructive},
		{"branch", []string{"--force", "feat", "main"}, known, tierDestructive},
		{"tag", []string{"-f", "v9"}, known, tierWrite},
		{"tag", []string{"-f", "-m", "v1", "v9"}, known, tierWrite},
		{"tag", []string{"--for", "v9"}, known, tierWrite},
		// git cannot be asked: fail safe.
		{"switch", []string{"--force-create", "feat"}, broken, tierDestructive},
		{"checkout", []string{"-B", "feat"}, broken, tierDestructive},
		{"branch", []string{"-f", "feat"}, broken, tierDestructive},
		{"tag", []string{"-f", "v9"}, broken, tierDestructive},
		{"checkout", []string{"-B", "feat"}, nil, tierDestructive},
		// git does not read the name as the plain one it spells: valid but
		// expanded, or not valid at all.
		{"switch", []string{"--force-create", "feat"}, unplain, tierDestructive},
		{"checkout", []string{"-B", "feat"}, unplain, tierDestructive},
		{"branch", []string{"-f", "feat"}, unplain, tierDestructive},
		{"branch", []string{"-M", "feat"}, unplain, tierDestructive},
		{"tag", []string{"-f", "v9"}, unplain, tierDestructive},
		// Something else on the call is destructive, or the form is not one this
		// parses confidently: the ref's novelty cannot lower it.
		{"switch", []string{"--force-create", "feat", "--discard-changes"}, known, tierDestructive},
		{"switch", []string{"-f", "--force-create", "feat"}, known, tierDestructive},
		{"checkout", []string{"-B", "feat", "-f"}, known, tierDestructive},
		{"branch", []string{"-f", "-d", "feat"}, known, tierDestructive},
		{"branch", []string{"-fv", "feat"}, known, tierDestructive},
		{"tag", []string{"-f", "-d", "v9"}, known, tierDestructive},

		// B1 (#540 round 3): git expands these to a real local branch before it
		// acts, but the probe asks about the literal "refs/heads/@{-1}", which
		// never exists. A name that is not a plain one is never lowered.
		{"checkout", []string{"-B", "@{-1}", "main"}, known, tierDestructive},
		{"switch", []string{"--force-create", "@{-1}"}, known, tierDestructive},
		{"switch", []string{"--force-create=@{-1}"}, known, tierDestructive},
		{"switch", []string{"--force-c=@{-1}"}, known, tierDestructive},
		{"branch", []string{"-f", "@{-1}", "main"}, known, tierDestructive},
		{"branch", []string{"-f", "-m", "@{-1}"}, known, tierDestructive},
		{"checkout", []string{"-B", "@{u}", "main"}, known, tierDestructive},
		{"checkout", []string{"-B", "main@{upstream}", "main"}, known, tierDestructive},
		{"checkout", []string{"-B", "@{push}", "main"}, known, tierDestructive},
		{"switch", []string{"--force-create", "@{upstream}"}, known, tierDestructive},
		{"tag", []string{"-f", "@{-1}"}, known, tierDestructive},
		// The rest of what is not a plain name: every character git treats
		// specially, a leading dash, "..", a ".lock" or "/" ending, an empty name.
		{"checkout", []string{"-B", "@", "main"}, known, tierDestructive},
		{"checkout", []string{"-B", "a@b"}, known, tierDestructive},
		{"checkout", []string{"-B", "a{b"}, known, tierDestructive},
		{"checkout", []string{"-B", "a..b"}, known, tierDestructive},
		{"checkout", []string{"-B", "feat.lock"}, known, tierDestructive},
		{"checkout", []string{"-B", "feat/"}, known, tierDestructive},
		{"checkout", []string{"-B", "feat~1"}, known, tierDestructive},
		{"checkout", []string{"-B", "feat^"}, known, tierDestructive},
		{"checkout", []string{"-B", "a:b"}, known, tierDestructive},
		{"checkout", []string{"-B", "a b"}, known, tierDestructive},
		{"checkout", []string{"-B", "a*"}, known, tierDestructive},
		{"checkout", []string{"-B", "a?"}, known, tierDestructive},
		{"checkout", []string{"-B", "a[b"}, known, tierDestructive},
		{"checkout", []string{"-B", `a\b`}, known, tierDestructive},
		{"checkout", []string{"-B", "caf\u00e9"}, known, tierDestructive},
		{"checkout", []string{"-B", ""}, known, tierDestructive},
		{"checkout", []string{"-B", "-x"}, known, tierDestructive},
		{"branch", []string{"-f", "--", "-x", "main"}, known, tierDestructive},
		{"tag", []string{"-f", "v1..2"}, known, tierDestructive},
		{"tag", []string{"-f", "v1.lock"}, known, tierDestructive},
		// Controls: plain names, including the characters the allowlist admits.
		{"checkout", []string{"-B", "feat/x-1_2.3", "main"}, known, tierWrite},
		{"switch", []string{"--force-create", "Feature/ABC-123"}, known, tierWrite},
		{"branch", []string{"-f", "a.b/c_d-e", "main"}, known, tierDestructive},
		{"tag", []string{"-f", "v1.2.3-rc.1"}, known, tierWrite},

		// B2 (#540 round 3): git keeps the LAST -B, but the form read only the
		// first. The reset flag must appear exactly once, however it is spelled.
		{"checkout", []string{"-B", "newb", "-B", "side", "main"}, known, tierDestructive},
		{"checkout", []string{"-B", "newb", "-Bside", "main"}, known, tierDestructive},
		{"checkout", []string{"-B", "newb", "-b", "side", "main"}, known, tierDestructive},
		{"checkout", []string{"-b", "newb", "-B", "side", "main"}, known, tierDestructive},
		{"checkout", []string{"-B", "newb", "-qB", "side", "main"}, known, tierDestructive},
		{"switch", []string{"--force-create", "newb", "--force-create", "side", "main"}, known, tierDestructive},
		{"switch", []string{"--force-create", "newb", "--force-create=side", "main"}, known, tierDestructive},
		{"switch", []string{"--force-create", "newb", "--force-c", "side"}, known, tierDestructive},
		{"switch", []string{"--force-create=newb", "-qC", "side"}, known, tierDestructive},
		{"switch", []string{"--force-create", "newb", "--create", "side"}, known, tierDestructive},
		{"switch", []string{"--create", "newb", "--force-create", "side"}, known, tierDestructive},
		{"branch", []string{"-f", "newb", "-f", "side"}, known, tierDestructive},
		{"branch", []string{"-f", "newb", "--force", "side"}, known, tierDestructive},
		{"branch", []string{"-f", "newb", "-qf", "side"}, known, tierDestructive},
		{"branch", []string{"-f", "-M", "newb", "side"}, known, tierDestructive},
		{"tag", []string{"-f", "v9", "-f", "v1"}, known, tierDestructive},
		{"tag", []string{"-f", "v9", "--force", "v1"}, known, tierDestructive},
		// The -f that is the only one is really --sort's value; the destructive
		// flag is the -D after it. Dropping that -f would leave --sort to take -D,
		// and the rest would read as a harmless create of newb.
		{"branch", []string{"--sort", "-f", "-D", "newb"}, known, tierDestructive},
		{"tag", []string{"--sort", "-f", "-d", "v9"}, known, tierDestructive},
		// A "-B" that is another option's value is not a second reset flag.
		{"checkout", []string{"-B", "newb", "--conflict", "-B", "main"}, known, tierWrite},

		// -M is --move --force, so it overwrites the target like -f does. A forced
		// branch form is destructive whether or not the name exists (see
		// TestRefineRefReset_BranchIsNeverLowered for the full set).
		{"branch", []string{"-M", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-M", "side"}, known, tierDestructive},
		{"branch", []string{"-M", "main", "@{-1}"}, known, tierDestructive},
		{"branch", []string{"-M", "main", "feat"}, known, tierDestructive},
		{"branch", []string{"-M", "feat"}, known, tierDestructive},
		{"branch", []string{"-f", "-m", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-f", "--move", "main", "feat"}, known, tierDestructive},
		{"branch", []string{"-f", "--copy", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-f", "--copy", "main", "feat"}, known, tierDestructive},
		{"branch", []string{"-M", "main", "side", "extra"}, known, tierDestructive},
		{"branch", []string{"-M", "-d", "feat"}, known, tierDestructive},
		{"branch", []string{"-Mq", "main", "feat"}, known, tierDestructive},

		// B1 (#540 round 4): a negation cancels the move/copy mode, so git resets
		// the FIRST positional while the lowering probed the last (a start point).
		{"branch", []string{"-f", "--move", "--no-move", "side", "tip"}, known, tierDestructive},
		{"branch", []string{"-f", "--copy", "--no-copy", "side", "tip"}, known, tierDestructive},
		{"branch", []string{"-f", "-m", "--no-move", "side", "tip"}, known, tierDestructive},
		{"branch", []string{"-f", "-qm", "--no-mo", "side", "tip"}, known, tierDestructive},
		// B2 (#540 round 4): -C is --copy --force, and git reads it inside a bundle
		// where the global-flag denylist, which matches the bare token, does not.
		{"branch", []string{"-C", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-qC", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-vC", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-Cq", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-iC", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-qC", "--", "main", "side"}, known, tierDestructive},
		{"branch", []string{"-qC", "main", "feat"}, known, tierDestructive},
		// N1 (#540 round 4): --recurse-submodules makes -f reset the submodules'
		// branch of that name as well, which the superproject probe never asks about.
		{"branch", []string{"-f", "--recurse-submodules", "newb", "main"}, known, tierDestructive},
	}
	for _, c := range cases {
		got := refineRefReset(c.sub, c.args, classifyGit(c.sub, c.args), c.probe)
		if got != c.want {
			t.Errorf("%s %q: tier %s, want %s", c.sub, c.args, gitTierNames[got], gitTierNames[c.want])
		}
	}
}

// TestRefineRefReset_BranchIsNeverLowered pins the cut that ended #540's review
// loop: every `git branch` form that classifyBranch rates destructive stays
// destructive, whether or not the name exists and whatever the probe answers.
// Four rounds each found a spelling that git's option grammar read as one mode
// and the lowering read as another (round 4: `--no-move`, and `-C` in a
// bundle), so branch is not lowered at all.
func TestRefineRefReset_BranchIsNeverLowered(t *testing.T) {
	// The most permissive probes: nothing exists and every name is plain, or
	// the names exist already.
	fresh := stubRefs(nil, true, nil)
	known := stubRefs(map[string]bool{"refs/heads/main": true, "refs/heads/side": true}, true, nil)
	for _, args := range [][]string{
		{"-f", "newb"},
		{"-f", "newb", "main"},
		{"--force", "newb", "main"},
		{"--for", "newb", "main"},
		{"-M", "newb"},
		{"-M", "main", "newb"},
		{"--move", "--force", "main", "newb"},
		{"-f", "--move", "main", "newb"},
		{"--copy", "--force", "main", "newb"},
		{"-f", "--copy", "main", "newb"},
		{"-C", "main", "newb"},
		{"-D", "newb"},
		{"-d", "newb"},
		{"--delete", "newb"},
		// B1: git resets the first positional, the start point is the last.
		{"-f", "--move", "--no-move", "side", "tip"},
		{"-f", "--copy", "--no-copy", "side", "tip"},
		{"-f", "-m", "--no-move", "side", "tip"},
		{"-f", "-qm", "--no-mo", "side", "tip"},
		{"-f", "--move", "--no-move", "newb", "main"},
		// B2: -C inside a bundle, and after the end of options.
		{"-qC", "main", "side"},
		{"-vC", "main", "side"},
		{"-Cq", "main", "side"},
		{"-iC", "main", "side"},
		{"-qC", "--", "main", "side"},
		{"-qC", "main", "newb"},
		// N1: the submodules' branch of that name is reset too.
		{"-f", "--recurse-submodules", "newb", "main"},
	} {
		if got := classifyGit("branch", args); got != tierDestructive {
			t.Errorf("classifyGit(branch %q) = %s, want destructive", args, gitTierNames[got])
		}
		for name, probe := range map[string]*refProbe{"fresh": fresh, "known": known, "no probe": nil} {
			if got := refineRefReset("branch", args, tierDestructive, probe); got != tierDestructive {
				t.Errorf("branch %q with a %s probe: tier %s, want destructive", args, name, gitTierNames[got])
			}
		}
		if _, ref, ok := refResetForm("branch", args); ok {
			t.Errorf("refResetForm(branch %q) = %q: branch has no lowering form", args, ref)
		}
	}
	// Controls: a branch call that creates, renames or copies without forcing is
	// a write, so the table above is about --force and not about every branch
	// call that names something.
	for _, args := range [][]string{
		{"newb"},
		{"newb", "main"},
		{"-m", "main", "newb"},
		{"--copy", "main", "newb"},
		{"-qc", "main", "newb"}, // lower-case -c copies without forcing
	} {
		if got := classifyGit("branch", args); got != tierWrite {
			t.Errorf("classifyGit(branch %q) = %s, want write", args, gitTierNames[got])
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
// A forced `branch` is refused either way: it is never lowered.
func TestGit_ForceCreateTierFollowsTheRef(t *testing.T) {
	t.Parallel()
	repo := mergeFixture(t)
	runGitDirect(t, repo, "tag", "v1")
	tool := writesOnlyGit(repo)
	for _, args := range []map[string]any{
		{"subcommand": "checkout", "args": []string{"-B", "fresh"}},
		{"subcommand": "switch", "args": []string{"-C", "fresher"}},
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
		{"subcommand": "branch", "args": []string{"-f", "newer", "main"}}, // a new name: not lowered
		{"subcommand": "tag", "args": []string{"-f", "v1", "side"}},
	} {
		_, err := callGit(t, tool, args)
		if err == nil || !strings.Contains(err.Error(), "destructive operations are disabled") {
			t.Errorf("%v resetting an existing ref: want the destructive-tier refusal, got %v", args["args"], err)
		}
	}
}

// TestGitRefProbe_ReadsNamesAsGit runs the real probe against a repository in
// which `@{-1}`, `@{u}` and `@{push}` all expand to the existing branch "side".
// This is the second line of defence behind plainRefName: the allowlist already
// refuses these spellings, so this test is what keeps git's own name check
// honest if the allowlist is ever widened.
func TestGitRefProbe_ReadsNamesAsGit(t *testing.T) {
	t.Parallel()
	repo := mergeFixture(t)
	for _, s := range [][]string{
		{"switch", "-q", "side"},
		{"switch", "-q", "main"}, // @{-1} is now side
		{"config", "branch.main.remote", "."},
		{"config", "branch.main.merge", "refs/heads/side"},
		{"config", "branch.main.pushRemote", "."},
		{"config", "push.default", "upstream"},
		{"tag", "v1", "side"},
	} {
		runGitDirect(t, repo, s...)
	}
	probe := writesOnlyGit(repo).refProbeIn(context.Background(), gitToolArgs{})
	if probe == nil {
		t.Fatal("no probe for a repository inside the workspace")
	}
	for _, c := range []struct {
		ref   string
		plain bool
	}{
		// Plain names.
		{"refs/heads/side", true},
		{"refs/heads/nope", true},
		{"refs/heads/feat/x-1.2", true},
		{"refs/heads/Main", true},
		{"refs/tags/v1.2.3", true},
		// Valid, but git acts on the branch they expand to: it prints that
		// branch's name back, not the one it was given.
		{"refs/heads/@{-1}", false},
		{"refs/heads/@{u}", false},
		{"refs/heads/@{upstream}", false},
		{"refs/heads/main@{upstream}", false},
		{"refs/heads/@{push}", false},
		{"refs/heads/main@{push}", false},
		// Not valid ref names at all.
		{"refs/heads/a//b", false},
		{"refs/heads/a/.b", false},
		{"refs/heads/a.lock/b", false},
		{"refs/heads/a..b", false},
		{"refs/heads/HEAD", false},
		{"refs/tags/a//b", false},
		{"refs/tags/a.lock/b", false},
		{"refs/tags/a.lock", false},
		{"refs/tags/x..y", false},
		{"refs/tags/a b", false},
		{"refs/tags/@{-1}", false},
	} {
		if got := probe.plainName(c.ref); got != c.plain {
			t.Errorf("plainName(%q) = %v, want %v", c.ref, got, c.plain)
		}
	}
	for _, c := range []struct {
		ref    string
		exists bool
	}{
		{"refs/heads/side", true},
		{"refs/heads/main", true},
		{"refs/heads/nope", false},
		{"refs/tags/v1", true},
		{"refs/tags/v2", false},
		{"refs/heads/@{-1}", false}, // the literal text names no ref: the reason expansion has to be refused first
	} {
		got, err := probe.exists(c.ref)
		if err != nil || got != c.exists {
			t.Errorf("exists(%q) = %v, %v; want %v", c.ref, got, err, c.exists)
		}
	}
}

// refSnapshot is every branch and tag with its commit, plus the current branch:
// what a call that moved a ref would change.
func refSnapshot(t *testing.T, repo string) string {
	t.Helper()
	refs, err := exec.Command("git", "-C", repo, "for-each-ref",
		"--format=%(refname)=%(objectname)", "refs/heads", "refs/tags").Output()
	if err != nil {
		t.Fatalf("git for-each-ref: %v", err)
	}
	cur, err := exec.Command("git", "-C", repo, "symbolic-ref", "--short", "-q", "HEAD").Output()
	if err != nil {
		cur = []byte("(detached)")
	}
	return strings.TrimSpace(string(refs)) + "\ncurrent=" + strings.TrimSpace(string(cur))
}

// TestGit_ForceCreateOfASpelledDifferentlyRefStaysDestructive is the end-to-end
// form of the #540 round-3 and round-4 reviews (B1, B2, N1 of each): each call
// below resets or overwrites a branch whatever it spells, so under the default
// policy (writes on, destructive off) it must be refused as destructive and
// leave every ref where it was. Before the fix each one ran at the write tier.
// The forced `branch` rows at the end create or move a branch that need not
// exist: branch is not lowered, so they are refused for a new name too.
func TestGit_ForceCreateOfASpelledDifferentlyRefStaysDestructive(t *testing.T) {
	t.Parallel()
	previous := [][]string{{"switch", "-q", "side"}, {"switch", "-q", "main"}} // @{-1} is side
	detour := [][]string{{"switch", "-q", "side"}, {"switch", "-q", "-c", "third"}}
	renamed := [][]string{
		{"switch", "-q", "side"},
		{"switch", "-q", "main"},
		{"switch", "-q", "-c", "third"},
		{"switch", "-q", "side"},
		{"switch", "-q", "main"}, // @{-1} is side again, with main checked out
	}
	upstream := [][]string{{"config", "branch.main.remote", "."}, {"config", "branch.main.merge", "refs/heads/side"}}
	push := append([][]string{{"config", "branch.main.pushRemote", "."}, {"config", "push.default", "upstream"}}, upstream...)
	tip := [][]string{{"tag", "tip", "main"}} // a start point that is not a branch name
	cases := []struct {
		name  string
		setup [][]string
		sub   string
		args  []string
	}{
		// B1: names git expands to an existing local branch.
		{"checkout -B @{-1}", previous, "checkout", []string{"-B", "@{-1}", "main"}},
		{"switch -C @{-1}", previous, "switch", []string{"-C", "@{-1}"}},
		{"switch --force-create=@{-1}", previous, "switch", []string{"--force-create=@{-1}"}},
		{"switch --force-c=@{-1}", previous, "switch", []string{"--force-c=@{-1}"}},
		{"branch -f @{-1}", detour, "branch", []string{"-f", "@{-1}", "main"}},
		{"branch -f -m @{-1}", renamed, "branch", []string{"-f", "-m", "@{-1}"}},
		{"checkout -B @{u}", upstream, "checkout", []string{"-B", "@{u}", "main"}},
		{"checkout -B main@{upstream}", upstream, "checkout", []string{"-B", "main@{upstream}", "main"}},
		{"checkout -B @{push}", push, "checkout", []string{"-B", "@{push}", "main"}},
		// B2: git keeps the last reset flag, so the first name is a decoy.
		{"checkout -B newb -B side", nil, "checkout", []string{"-B", "newb", "-B", "side", "main"}},
		{"checkout -B newb -Bside", nil, "checkout", []string{"-B", "newb", "-Bside", "main"}},
		{"switch -C newb -C side", nil, "switch", []string{"-C", "newb", "-C", "side", "main"}},
		{"switch --force-create newb --force-create=side", nil, "switch", []string{"--force-create", "newb", "--force-create=side", "main"}},
		// N1: -M is --move --force.
		{"branch -M main side", nil, "branch", []string{"-M", "main", "side"}},
		{"branch -M side (rename current over side)", nil, "branch", []string{"-M", "side"}},
		// B1 (round 4): a negation cancels the move/copy mode, so git resets the
		// first positional, `side`, to the start point `tip`.
		{"branch -f --move --no-move side tip", tip, "branch", []string{"-f", "--move", "--no-move", "side", "tip"}},
		{"branch -f --copy --no-copy side tip", tip, "branch", []string{"-f", "--copy", "--no-copy", "side", "tip"}},
		{"branch -f -m --no-move side tip", tip, "branch", []string{"-f", "-m", "--no-move", "side", "tip"}},
		{"branch -f -qm --no-mo side tip", tip, "branch", []string{"-f", "-qm", "--no-mo", "side", "tip"}},
		// B2 (round 4): -C is --copy --force, and the denylist matches only the
		// bare token, so a bundle reaches git as a forced copy over `side`.
		{"branch -qC main side", nil, "branch", []string{"-qC", "main", "side"}},
		{"branch -vC main side", nil, "branch", []string{"-vC", "main", "side"}},
		{"branch -Cq main side", nil, "branch", []string{"-Cq", "main", "side"}},
		{"branch -iC main side", nil, "branch", []string{"-iC", "main", "side"}},
		{"branch -qC -- main side", nil, "branch", []string{"-qC", "--", "main", "side"}},
		// N1 (round 4): with --recurse-submodules, -f resets the submodules' branch
		// of the same name, which no probe of the superproject can see.
		{"branch -f --recurse-submodules newb main", nil, "branch", []string{"-f", "--recurse-submodules", "newb", "main"}},
		// A forced branch is destructive for a new name as well.
		{"branch -f newb main", nil, "branch", []string{"-f", "newb", "main"}},
		{"branch -M main renamed", nil, "branch", []string{"-M", "main", "renamed"}},
		{"branch -f --move main renamed", nil, "branch", []string{"-f", "--move", "main", "renamed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := mergeFixture(t)
			for _, s := range c.setup {
				runGitDirect(t, repo, s...)
			}
			before := refSnapshot(t, repo)
			_, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": c.sub, "args": c.args})
			if err == nil || !strings.Contains(err.Error(), "destructive operations are disabled") {
				t.Errorf("%s %v: want the destructive-tier refusal, got %v", c.sub, c.args, err)
			}
			if after := refSnapshot(t, repo); after != before {
				t.Errorf("%s %v moved a ref:\nbefore %s\nafter  %s", c.sub, c.args, before, after)
			}
		})
	}
}

// TestGit_ForceCreateOfANewPlainNameStillRuns is the control for the test above:
// `checkout -B`, `switch -C` and `tag -f` on a plain name that does not exist
// yet are routine creation and run at the write tier, once given, and so do the
// unforced `branch` forms (create, rename, copy).
func TestGit_ForceCreateOfANewPlainNameStillRuns(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		sub  string
		args []string
	}{
		{"checkout -B newb main", "checkout", []string{"-B", "newb", "main"}},
		{"switch -C newb", "switch", []string{"-C", "newb"}},
		{"switch -C feat/x-1.2", "switch", []string{"-C", "feat/x-1.2"}},
		{"tag -f v9", "tag", []string{"-f", "v9"}},
		{"branch newb main", "branch", []string{"newb", "main"}},
		{"branch -m side renamed", "branch", []string{"-m", "side", "renamed"}},
		{"branch --copy main copied", "branch", []string{"--copy", "main", "copied"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := mergeFixture(t)
			if _, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": c.sub, "args": c.args}); err != nil {
				t.Errorf("%s %v on a new name: %v", c.sub, c.args, err)
			}
		})
	}
}
