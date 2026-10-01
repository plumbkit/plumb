package tools

import "testing"

// git_classify_abbrev_test.go pins that the argument-dependent classifiers read
// options as git's parser does (git_options.go). Each "was" row is a form that
// used to land a tier BELOW what git performs — an abbreviated long option
// (`--disc` for --discard-changes), a bundled short flag (`-dr`), or a flag
// hidden after `--` or behind another option's value. The controls are forms
// that must stay where they are, including ones only a correct value model gets
// right (`tag -m -d v1` is a tag whose message is "-d").
func TestClassifyGit_ReadsOptionsAsGitDoes(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
		want gitTier
	}{
		// switch: --discard-changes / --force, abbreviated or bundled.
		{"switch", []string{"--disc", "main"}, tierDestructive},
		{"switch", []string{"--discard", "main"}, tierDestructive},
		{"switch", []string{"-qf", "main"}, tierDestructive},
		{"switch", []string{"-fq", "main"}, tierDestructive},
		{"switch", []string{"--forc", "main"}, tierDestructive},
		// restore: a working-tree restore hidden from the --staged check.
		{"restore", []string{"--staged", "--work", "f"}, tierDestructive},
		{"restore", []string{"--staged", "-qW", "f"}, tierDestructive},
		{"restore", []string{"--", "--staged"}, tierDestructive},
		{"restore", []string{"--pathspec-from-file", "--staged"}, tierDestructive},
		{"restore", []string{"-s", "--staged", "f"}, tierDestructive},
		// branch / tag: delete, abbreviated or bundled.
		{"branch", []string{"--del", "old"}, tierDestructive},
		{"branch", []string{"--de", "old"}, tierDestructive},
		{"branch", []string{"-dr", "origin/old"}, tierDestructive},
		{"branch", []string{"-rd", "origin/old"}, tierDestructive},
		{"branch", []string{"-Df", "old"}, tierDestructive},
		{"tag", []string{"--del", "v1"}, tierDestructive},
		{"tag", []string{"-fd", "v1"}, tierDestructive},
		// checkout -b with a force, which throws away local modifications.
		{"checkout", []string{"-b", "new", "-f"}, tierDestructive},
		{"checkout", []string{"-b", "new", "--for"}, tierDestructive},
		{"checkout", []string{"-b", "new", "-qf"}, tierDestructive},

		// Controls: these stay at their tier.
		{"switch", []string{"--create", "feature"}, tierWrite},
		{"switch", []string{"--force-create", "feature"}, tierDestructive},
		{"switch", []string{"--create", "f-branch", "main"}, tierWrite},
		{"switch", []string{"-m", "main"}, tierWrite},
		{"switch", []string{"--detach", "HEAD~1"}, tierWrite},
		{"restore", []string{"-S", "f"}, tierWrite},
		{"restore", []string{"--stag", "f"}, tierWrite},
		{"restore", []string{"--staged", "-s", "HEAD", "f"}, tierWrite},
		{"restore", []string{"--staged", "--source=HEAD", "f"}, tierWrite},
		{"restore", []string{"--staged", "--", "f"}, tierWrite},
		{"branch", []string{"--move", "old", "new"}, tierWrite},
		{"branch", []string{"--mov", "old", "new"}, tierWrite},
		{"branch", []string{"--li"}, tierRead},
		{"branch", []string{"-vv"}, tierRead},
		{"branch", []string{"--contains", "HEAD"}, tierRead},
		{"branch", []string{"--sort=-committerdate"}, tierRead},
		{"tag", []string{"-n5"}, tierRead},
		{"tag", []string{"--li"}, tierRead},
		{"tag", []string{"-m", "-d", "v1"}, tierWrite},
		{"tag", []string{"--message", "--delete", "v1"}, tierWrite},
		{"checkout", []string{"-b", "new", "origin/main"}, tierWrite},
		{"checkout", []string{"-B", "new"}, tierDestructive},
	}
	for _, c := range cases {
		if got := classifyGit(c.sub, c.args); got != c.want {
			t.Errorf("classifyGit(%q, %q) = %s, want %s", c.sub, c.args, gitTierNames[got], gitTierNames[c.want])
		}
	}
}

// TestClassifyGit_LoweringChecksFollowGit pins the #540 round-2 review: the
// checks that LOWER a tier (branch's list mode to read, restore --staged to
// write) must hold only when git really is in that mode, and the operations
// that move or replace an existing ref are destructive, like `reset --keep`.
func TestClassifyGit_LoweringChecksFollowGit(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
		want gitTier
	}{
		// -v/--verbose do not put `git branch` in list mode once a name is given;
		// with bundles expanded, `-fv` used to reach the read tier and force-move.
		{"branch", []string{"-fv", "side", "main"}, tierDestructive},
		{"branch", []string{"-vf", "old", "HEAD"}, tierDestructive},
		{"branch", []string{"-v", "-f", "old", "HEAD"}, tierDestructive},
		{"branch", []string{"-qv", "newb"}, tierWrite},
		{"branch", []string{"-v", "newb"}, tierWrite},
		{"branch", []string{"--verbose", "newb"}, tierWrite},
		// A later --no-<opt> cancels the list flag.
		{"branch", []string{"--list", "--no-list", "newb"}, tierWrite},
		{"branch", []string{"-l", "--no-list", "newb"}, tierWrite},
		{"branch", []string{"-l", "--no-list", "-f", "old", "HEAD"}, tierDestructive},
		// --end-of-options ends options exactly as -- does.
		{"branch", []string{"--end-of-options", "-l"}, tierWrite},
		{"restore", []string{"--end-of-options", "--staged", "f"}, tierDestructive},
		{"restore", []string{"--staged", "--no-staged", "f"}, tierDestructive},
		// Moving or replacing an existing ref.
		{"checkout", []string{"-B", "main", "HEAD~5"}, tierDestructive},
		{"switch", []string{"--force-create", "main", "HEAD~5"}, tierDestructive},
		{"branch", []string{"-f", "existing"}, tierDestructive},
		{"branch", []string{"--force", "existing", "HEAD~5"}, tierDestructive},
		{"tag", []string{"-f", "v1"}, tierDestructive},
		{"tag", []string{"--force", "v1", "HEAD~5"}, tierDestructive},
		// Branch options that write config are writes, not reads.
		{"branch", []string{"--unset-upstream"}, tierWrite},
		{"branch", []string{"-u", "origin/main"}, tierWrite},
		{"branch", []string{"--set-upstream-to=origin/main"}, tierWrite},
		{"branch", []string{"--edit-description"}, tierWrite},

		// Controls: list mode stays read.
		{"branch", []string{"-v"}, tierRead},
		{"branch", []string{"-vv"}, tierRead},
		{"branch", []string{"--list", "-v"}, tierRead},
		{"branch", []string{"-r"}, tierRead},
		{"branch", []string{"--no-contains", "HEAD"}, tierRead},
		{"branch", []string{"--points-at", "HEAD"}, tierRead},
		{"branch", []string{"--contains", "HEAD", "-v"}, tierRead},
		{"restore", []string{"--staged", "--no-worktree", "f"}, tierWrite},
		{"checkout", []string{"-b", "new"}, tierWrite},
		{"switch", []string{"--create", "new"}, tierWrite},
	}
	for _, c := range cases {
		if got := classifyGit(c.sub, c.args); got != c.want {
			t.Errorf("classifyGit(%q, %q) = %s, want %s", c.sub, c.args, gitTierNames[got], gitTierNames[c.want])
		}
	}
}
