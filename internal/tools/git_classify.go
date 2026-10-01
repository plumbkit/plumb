package tools

import (
	"errors"
	"fmt"
	"strings"
)

// gitTier classifies a git invocation by blast radius. Higher tiers require
// more permission.
type gitTier int

const (
	tierReject gitTier = iota
	tierRead
	tierWrite
	tierDestructive
	tierNetwork
)

// dangerousGitGlobalFlags are never accepted in args. They are inert when they
// follow the subcommand (git interprets them per-subcommand), but rejecting
// them is defence-in-depth against any future code path that prepends args, and
// blocks the upload-pack/receive-pack remote-helper RCE vectors on push/fetch.
// -c and -C are included because some subcommands pass unknown flags up to git's
// global parser, making -c <key>=<val> a live config-injection vector there.
var dangerousGitGlobalFlags = map[string]bool{
	"-c":             true,
	"-C":             true,
	"--exec-path":    true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--upload-pack":  true,
	"--receive-pack": true,
}

// checkGitGlobalFlags rejects args that name an unconditionally-global,
// never-legitimate-as-a-subcommand-flag option (both bare and key=value forms).
func checkGitGlobalFlags(args []string) error {
	for _, a := range args {
		name := a
		if before, _, ok := strings.Cut(a, "="); ok {
			name = before
		}
		if dangerousGitGlobalFlags[name] {
			return fmt.Errorf("git: flag %q is not permitted", name)
		}
	}
	return nil
}

// gitRemoteLeadingSubcommands are the network verbs whose first positional
// argument is a remote name or URL. They are exactly the calls where a caller
// that repeats the verb inside args — the most common arg-shape slip this tool
// sees — silently hands git a remote named after the verb and gets back the
// cryptic "fatal: '<verb>' does not appear to be a git repository". The guard
// is limited to this set because elsewhere the first argument can legitimately
// repeat the verb (`git stash push`). Accepted trade: a remote deliberately
// named after the verb (`git remote add push ...`) becomes unreachable through
// the tool — perverse naming, and the caller who does it gets a refusal whose
// remedy does not fit; judged not worth complicating the ordinary-slip path for.
var gitRemoteLeadingSubcommands = map[string]bool{
	"push":  true,
	"fetch": true,
	"pull":  true,
}

// rejectDuplicatedLeadingSubcommand refuses a call whose args repeat the
// subcommand as their first element, for the remote-leading verbs where the
// duplicate is unambiguous caller error. It refuses rather than silently
// stripping the duplicate: a caller that passed the token twice may have meant
// something else, and quietly dropping an argument is how a command succeeds
// at the wrong thing.
func rejectDuplicatedLeadingSubcommand(sub string, args []string) error {
	if len(args) == 0 || args[0] != sub || !gitRemoteLeadingSubcommands[sub] {
		return nil
	}
	return fmt.Errorf(
		"git %s: args must not repeat the subcommand — the verb goes only in the subcommand field, "+
			"and args carries its arguments (e.g. subcommand %q, args [\"origin\", \"main\"]). "+
			"As passed, git treats %q as the remote name and fails with %q",
		sub, sub, sub, "'"+sub+"' does not appear to be a git repository",
	)
}

// normaliseSwitchCreate rewrites `git switch -c/-C` to its long form
// (--create/--force-create). git switch's short create flags collide with the
// globally-denylisted -c/-C (the -c key=value config-injection and -C run-in-path
// vectors), so a legitimate `git switch -c <branch>` is otherwise refused with
// "flag -c is not permitted". The rewrite is a pure synonym — git treats -c and
// --create identically — applied ONLY for the switch subcommand (where any -c in
// args is unambiguously the create flag, never git's global config flag, which
// can only precede the subcommand and so never reaches here) and only to a bare
// flag token. Returns the possibly-rewritten args and a note when it acted.
func normaliseSwitchCreate(sub string, args []string) ([]string, string) {
	if sub != "switch" {
		return args, ""
	}
	out := make([]string, len(args))
	rewrote := false
	for i, a := range args {
		switch a {
		case "-c":
			out[i], rewrote = "--create", true
		case "-C":
			out[i], rewrote = "--force-create", true
		default:
			out[i] = a
		}
	}
	if !rewrote {
		return args, ""
	}
	return out, "# plumb-note: rewrote `git switch -c/-C` to its long form (--create/--force-create) — " +
		"the short form collides with git's global -c/-C config flag, which plumb denies.\n"
}

// classifyGitCall runs the argument checks that precede tiering — the global
// flag denylist, the duplicated-verb slip, merge's refused flags — and returns
// the call's tier, or the refusal for a subcommand the tool does not permit.
func classifyGitCall(a gitToolArgs) (gitTier, error) {
	if err := checkGitGlobalFlags(a.Args); err != nil {
		return tierReject, err
	}
	if err := rejectDuplicatedLeadingSubcommand(a.Subcommand, a.Args); err != nil {
		return tierReject, err
	}
	if a.Subcommand == "merge" {
		if err := checkMergeArgs(a.Args); err != nil {
			return tierReject, err
		}
	}
	tier := classifyGit(a.Subcommand, a.Args)
	if tier != tierReject {
		return tier, nil
	}
	if a.Subcommand == "stash" && len(a.Args) > 0 {
		return tier, fmt.Errorf("git stash: sub-command %q is not permitted; use list, show, push, pop, apply, drop, or clear", a.Args[0])
	}
	if a.Subcommand == "rm" {
		return tier, errors.New("git: subcommand \"rm\" is not permitted; to remove a tracked file, use delete_file to remove it from disk, then stage the deletion with git add")
	}
	return tier, fmt.Errorf("git: subcommand %q is not permitted", a.Subcommand)
}

// classifyGit maps a subcommand + args to a tier. Ambiguous subcommands
// (branch, tag, stash, checkout, switch, restore, merge) inspect their args; the
// classification is safe-biased — when in doubt it returns the higher tier.
func classifyGit(sub string, args []string) gitTier {
	switch sub {
	case "diff", "log", "show", "blame", "status", "shortlog", "check-ignore":
		return tierRead
	case "add", "commit", "mv":
		return tierWrite
	case "switch":
		return classifySwitch(args)
	case "restore":
		return classifyRestore(args)
	case "branch":
		return classifyBranch(args)
	case "tag":
		return classifyTag(args)
	case "stash":
		return classifyStash(args)
	case "checkout":
		return classifyCheckout(args)
	case "merge":
		return classifyMerge(args) // git_merge.go
	// cherry-pick is flat-classified, like rebase — its closest analogue, and the
	// other sequencer verb here. Arg inspection (classifyStash, classifyBranch,
	// classifyCheckout) exists only where a subcommand's arg space SPANS tiers;
	// cherry-pick's does not. Its state flags are no safer than the bare form:
	// --continue commits and moves HEAD, --skip and --abort reset the working
	// tree (discarding any conflict resolution), and --quit strands a
	// half-applied pick. Every form belongs at the same tier, so splitting them
	// would only invent a distinction the safe-bias rule then has to collapse.
	case "reset", "clean", "rebase", "revert", "cherry-pick":
		return tierDestructive
	case "push", "fetch", "pull":
		return tierNetwork
	case "rm":
		return tierReject
	default:
		return tierReject
	}
}

// The arms below read options through their subcommand's grammar
// (git_options.go), so an abbreviation (`--disc`), a bundle (`-dr`) or a value
// (`tag -m -d`) is read as git reads it. A check that RAISES a tier scans past
// `--` (over-classifying a path that spells an option is the safe error); a
// check that LOWERS one (restore --staged, the list-mode flags) stops there.

func classifySwitch(args []string) gitTier {
	// --force-create (-C) resets an existing branch to the start point, which
	// discards its commits the way `reset --keep` does. Destructive here; once the
	// repository is known, refineRefReset lowers it to a write for a new branch.
	if switchGrammar.has(args, "fC", "force", "discard-changes", "force-create") {
		return tierDestructive
	}
	return tierWrite
}

// classifyRestore: `restore --staged <path>` only touches the index (safe to
// treat as a write); any form that touches the working tree discards changes.
func classifyRestore(args []string) gitTier {
	staged := restoreGrammar.final(args, true, gitOptName{'S', "staged"})
	worktree := restoreGrammar.has(args, "W", "worktree")
	if staged && !worktree {
		return tierWrite
	}
	return tierDestructive
}

// branchListMode are the options that put `git branch` in list mode even with
// a name given (the name is then a pattern or a value). -v/--verbose are NOT
// among them: with a name they create, and with -f they force-move.
var branchListMode = []gitOptName{
	{'l', "list"},
	{'a', "all"},
	{'r', "remotes"},
	{0, "show-current"},
	{0, "contains"},
	{0, "no-contains"},
	{0, "merged"},
	{0, "no-merged"},
	{0, "points-at"},
}

func classifyBranch(args []string) gitTier {
	// -f/--force moves or replaces an existing branch, like `reset --keep`; -M and
	// -C are the short forms of --move --force and --copy --force, which overwrite
	// their destination, and -D is --delete --force. Every one stays destructive
	// whether or not the branch exists: unlike checkout -B, switch -C and tag -f,
	// branch is not lowered to a write for a new name (git_ref_reset.go says why).
	// The global-flag denylist refuses only a bare -C token, so the C here is what
	// catches it inside a bundle (-qC, -Cq).
	if branchGrammar.has(args, "dDfMC", "delete", "force") {
		return tierDestructive
	}
	// Lower-case -c (copy without --force) is refused by the denylist as a bare
	// token, and git itself refuses to copy over an existing branch, so only the
	// long --copy and a bundle reach here, and both are writes. The upstream and
	// description options write the repository's config.
	if branchGrammar.has(args, "mu", "move", "copy", "set-upstream-to", "unset-upstream", "edit-description") {
		return tierWrite
	}
	if branchGrammar.final(args, true, branchListMode...) {
		return tierRead
	}
	if hasNonFlagArg(args) {
		return tierWrite // creating a branch
	}
	return tierRead
}

func classifyTag(args []string) gitTier {
	// -f/--force replaces an existing tag (refineRefReset: a write for a new one).
	if tagGrammar.has(args, "df", "delete", "force") {
		return tierDestructive
	}
	if tagGrammar.final(args, true, gitOptName{'l', "list"}, gitOptName{'n', ""}, gitOptName{0, "contains"}, gitOptName{0, "merged"}) {
		return tierRead
	}
	if hasNonFlagArg(args) {
		return tierWrite // creating a tag
	}
	return tierRead
}

func classifyStash(args []string) gitTier {
	if len(args) == 0 {
		return tierWrite // bare `git stash` pushes (mutates working tree)
	}
	switch args[0] {
	case "list", "show":
		return tierRead
	case "push", "save", "pop", "apply", "create", "store":
		return tierWrite
	case "drop", "clear":
		return tierDestructive
	default:
		return tierReject // unknown stash sub-subcommand; caller reports "not permitted"
	}
}

// classifyCheckout treats only pure branch creation (-b) as a write; every
// other checkout form can discard the working tree or detach HEAD, so it is
// destructive — and so is -B, which resets an existing branch like `reset
// --keep` (refineRefReset lowers it to a write for a new branch), and creation
// with -f, which throws away local modifications. A -B anywhere after a leading
// -b is not creation either: today git refuses `-b x -B y`, but the call is not
// one this classifier can call a pure create. Prefer `switch` for safe branch
// changes.
func classifyCheckout(args []string) gitTier {
	if len(args) > 0 && args[0] == "-b" && !checkoutGrammar.has(args, "fB", "force") {
		return tierWrite
	}
	return tierDestructive
}

// hasNonFlagArg reports whether args carry a positional argument: one not
// starting with "-", or anything after "--" / "--end-of-options" (where git
// reads `-l` as a name). It does not skip option values, so a value counts as
// a positional too — over-counting only raises the tier it decides.
func hasNonFlagArg(args []string) bool {
	for i, a := range args {
		if isEndOfOptions(a) {
			return i+1 < len(args)
		}
		if a != "" && !strings.HasPrefix(a, "-") {
			return true
		}
	}
	return false
}
