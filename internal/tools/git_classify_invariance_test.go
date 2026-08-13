package tools

import "testing"

// gitArgBattery is the argument-vector battery every arg-independent subcommand
// is driven through below.
//
// It is deliberately cross-verb nonsense. `--staged`/`-S`/`--worktree` are
// restore's flags, `-d`/`-D`/`--delete`/`--list` are branch and tag's,
// `-b`/`-B` are checkout's, `--force`/`--discard-changes` are switch's, and
// `list`/`drop` are stash sub-subcommands — every token some OTHER arm of the
// classifier already keys on. classifyGit parses no per-verb grammar, so the
// property under test has to hold for a form that is nonsense for the verb just
// as firmly as for one that reads naturally. A future arm that keys on one of
// these tokens is exactly what that catches.
var gitArgBattery = []struct {
	name string
	args []string
}{
	{"nil", nil},
	{"empty", []string{}},
	// Sequencer state flags.
	{"continue", []string{"--continue"}},
	{"abort", []string{"--abort"}},
	{"quit", []string{"--quit"}},
	{"skip", []string{"--skip"}},
	// Editor and staging control.
	{"e", []string{"-e"}},
	{"edit", []string{"--edit"}},
	{"no-edit", []string{"--no-edit"}},
	{"n", []string{"-n"}},
	{"no-commit", []string{"--no-commit"}},
	// Mainline selection.
	{"m-1", []string{"-m", "1"}},
	{"mainline-1", []string{"--mainline", "1"}},
	// Merge strategy and fast-forward control.
	{"ff", []string{"--ff"}},
	{"no-ff", []string{"--no-ff"}},
	{"s-recursive", []string{"-s", "recursive"}},
	{"strategy-eq-recursive", []string{"--strategy=recursive"}},
	{"strategy-ours", []string{"--strategy", "ours"}},
	{"X-theirs", []string{"-X", "theirs"}},
	// Signing.
	{"S", []string{"-S"}},
	{"gpg-sign", []string{"--gpg-sign"}},
	// Assorted per-verb switches.
	{"x", []string{"-x"}},
	{"allow-empty", []string{"--allow-empty"}},
	{"signoff", []string{"--signoff"}},
	{"amend", []string{"--amend"}},
	{"autostash", []string{"--autostash"}},
	{"no-verify", []string{"--no-verify"}},
	{"root", []string{"--root"}},
	{"i", []string{"-i"}},
	{"interactive", []string{"--interactive"}},
	{"onto-main", []string{"--onto", "main"}},
	{"dry-run", []string{"--dry-run"}},
	{"quiet", []string{"-q"}},
	{"verbose", []string{"-v"}},
	{"all", []string{"--all"}},
	{"A", []string{"-A"}},
	// Revisions, ranges, and pathspec separators.
	{"rev", []string{"abc1234"}},
	{"range", []string{"A..B"}},
	{"range-caret", []string{"abc1234^..def5678"}},
	{"dashdash-path", []string{"--", "file.go"}},
	{"bare-dashdash", []string{"--"}},
	{"remote-branch", []string{"origin", "main"}},
	// Reset modes and clean flags.
	{"hard", []string{"--hard"}},
	{"soft", []string{"--soft"}},
	{"mixed", []string{"--mixed"}},
	{"fd", []string{"-fd"}},
	// Tokens other classifier arms key on — nonsense here, and that is the point.
	{"force", []string{"--force"}},
	{"force-with-lease", []string{"--force-with-lease"}},
	{"discard-changes", []string{"--discard-changes"}},
	{"staged", []string{"--staged"}},
	{"worktree", []string{"--worktree"}},
	{"W", []string{"-W"}},
	{"d", []string{"-d"}},
	{"D", []string{"-D"}},
	{"delete", []string{"--delete"}},
	{"list-flag", []string{"--list"}},
	{"l", []string{"-l"}},
	{"b-new", []string{"-b", "new"}},
	{"B-new", []string{"-B", "new"}},
	{"copy", []string{"--copy"}},
	{"move", []string{"--move"}},
	{"stash-list", []string{"list"}},
	{"stash-drop", []string{"drop"}},
	// A realistic multi-flag call, so a guard that only fires on a lone flag
	// token is not enough to pass.
	{"mixed-bag", []string{"-n", "--ff", "-m", "1", "--", "a", "b"}},
}

// TestClassifyGit_ArgIndependentSubcommandsAreInvariant proves the property
// gitArgIndependent's name asserts and nothing else pinned: for these
// subcommands classifyGit's answer does not move with the arguments at all.
//
// It exists because a hand-written table can only demonstrate the forms someone
// thought to list. Every row of TestClassifyGit for a flat verb runs one
// identical code path, so a later edit that split a verb's arm on an argument —
// "`--ff` is only a write", "`--strategy` just names a merge driver, that's a
// read" — leaves every one of those rows green. Four such mutants survived the
// whole suite before this test existed.
//
// The demotion those mutants perform is not cosmetic, which is why the property
// is worth a guard of its own rather than more sampled rows. tierRead is the
// tier that skips the allow_destructive/confirm gate (gateGit), the write-rate
// limiter, the per-repo serialisation lock (beginSerialisedGit runs only for
// tier != tierRead), and the expected_head plus cross-session ref guard
// (armRefGuard sets check only for write and destructive) — and routes the argv
// through gitReadArgv. One arg-keyed arm would hand all of that to a verb that
// rewrites history.
//
// The guard covers every flat verb, not just the newest: reset, clean, rebase,
// revert and cherry-pick share one arm, and so does the invariant.
func TestClassifyGit_ArgIndependentSubcommandsAreInvariant(t *testing.T) {
	for _, sub := range gitArgIndependent {
		t.Run(sub, func(t *testing.T) {
			base := classifyGit(sub, nil)
			if base == tierReject {
				t.Fatalf("classifyGit(%q, nil) refuses the subcommand, but gitArgIndependent lists it as "+
					"one classifyGit names — the pinned set and the classifier have drifted apart", sub)
			}
			for _, v := range gitArgBattery {
				t.Run(v.name, func(t *testing.T) {
					got := classifyGit(sub, v.args)
					if got != base {
						t.Errorf("classifyGit(%q, %v) = %s, but classifyGit(%q, nil) = %s — %q is pinned as "+
							"argument-INDEPENDENT, so an arg that moves its tier is a policy hole, not a refinement "+
							"(a demotion to read skips the gate, the rate limiter, the per-repo lock and the ref guard)",
							sub, v.args, gitTierNames[got], sub, gitTierNames[base], sub)
					}
				})
			}
		})
	}
}
