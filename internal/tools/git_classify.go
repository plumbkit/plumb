package tools

import (
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
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if dangerousGitGlobalFlags[name] {
			return fmt.Errorf("git: flag %q is not permitted", name)
		}
	}
	return nil
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

// classifyGit maps a subcommand + args to a tier. Ambiguous subcommands
// (branch, tag, stash, checkout, switch, restore) inspect their args; the
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
	case "reset", "clean", "rebase", "revert":
		return tierDestructive
	case "push", "fetch", "pull":
		return tierNetwork
	case "rm":
		return tierReject
	default:
		return tierReject
	}
}

func classifySwitch(args []string) gitTier {
	if hasAnyFlag(args, "-f", "--force", "--discard-changes") {
		return tierDestructive
	}
	return tierWrite
}

// classifyRestore: `restore --staged <path>` only touches the index (safe to
// treat as a write); any form that touches the working tree discards changes.
func classifyRestore(args []string) gitTier {
	staged := hasAnyFlag(args, "--staged", "-S")
	worktree := hasAnyFlag(args, "--worktree", "-W")
	if staged && !worktree {
		return tierWrite
	}
	return tierDestructive
}

func classifyBranch(args []string) gitTier {
	if hasAnyFlag(args, "-d", "-D", "--delete") {
		return tierDestructive
	}
	if hasAnyFlag(args, "-m", "-M", "--move", "-c", "-C", "--copy") {
		return tierWrite
	}
	if hasAnyFlag(args, "--list", "-l", "-a", "--all", "-r", "--remotes",
		"-v", "-vv", "--show-current", "--contains", "--merged", "--no-merged") {
		return tierRead
	}
	if hasNonFlagArg(args) {
		return tierWrite // creating a branch
	}
	return tierRead
}

func classifyTag(args []string) gitTier {
	if hasAnyFlag(args, "-d", "--delete") {
		return tierDestructive
	}
	if hasAnyFlag(args, "-l", "--list", "-n", "--contains", "--merged") {
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

// classifyCheckout treats only pure branch creation (-b/-B) as a write; every
// other checkout form can discard the working tree or detach HEAD, so it is
// destructive. Prefer `switch` for safe branch changes.
func classifyCheckout(args []string) gitTier {
	if len(args) > 0 && (args[0] == "-b" || args[0] == "-B") {
		return tierWrite
	}
	return tierDestructive
}

func hasAnyFlag(args []string, flags ...string) bool {
	set := make(map[string]bool, len(flags))
	for _, f := range flags {
		set[f] = true
	}
	for _, a := range args {
		if set[a] {
			return true
		}
	}
	return false
}

func hasNonFlagArg(args []string) bool {
	for _, a := range args {
		if a != "" && !strings.HasPrefix(a, "-") {
			return true
		}
	}
	return false
}
