package tools

import (
	"context"
	"os/exec"
	"strings"
)

// git_ref_reset.go keeps routine branch creation at the write tier.
//
// `switch -C <name>`, `checkout -B <name>`, `branch -f <name>` and `tag -f
// <name>` RESET <name> when it already exists, which discards its commits the
// way `reset --keep` does, so classifyGit puts them in the destructive tier.
// When <name> does not exist yet they only create it, which is everyday agent
// work (`checkout -B feature` to start a branch). So once the repository is
// known, refineRefReset asks git whether the ref exists and lowers the call to
// the write tier only when it does not.
//
// Everything uncertain stays destructive: a form this file cannot parse (a
// bundle such as `branch -fv`), another destructive flag on the same call
// (`switch -C x --discard-changes`), or git not being able to answer.
//
// Accepted window: a peer that creates the ref between this check and the git
// child would see it reset at the write tier. The check runs before the
// per-repository lock, and plumb-mediated writes from other sessions are also
// what the cross-session ref guard reports; an unmediated creator in that
// instant is a race this classification does not try to close.

// refResetForm recognises a call whose only destructive effect may be
// resetting an existing ref. It returns the same call with the reset flag
// replaced by its creating counterpart (or removed), and the full ref name the
// reset would apply to. ok is false for anything else, including forms it does
// not parse confidently.
func refResetForm(sub string, args []string) (neutral []string, ref string, ok bool) {
	switch sub {
	case "switch":
		return switchResetForm(args)
	case "checkout":
		if len(args) >= 2 && args[0] == "-B" {
			return append([]string{"-b"}, args[1:]...), "refs/heads/" + args[1], true
		}
	case "branch":
		return forceFlagResetForm(branchGrammar, args, "refs/heads/")
	case "tag":
		return forceFlagResetForm(tagGrammar, args, "refs/tags/")
	}
	return nil, "", false
}

// switchResetForm handles `switch -C <name>` / `--force-create[=]<name>` (the
// tool rewrites -C to --force-create before classification).
func switchResetForm(args []string) ([]string, string, bool) {
	at, name := -1, ""
	for i, a := range args {
		long, isLong := strings.CutPrefix(a, "--")
		if a != "-C" && (!isLong || !isLongPrefix(strings.SplitN(long, "=", 2)[0], "force-create", len("force-"))) {
			continue
		}
		if at >= 0 {
			return nil, "", false // two reset flags: not a form to second-guess
		}
		at = i
		if _, v, joined := strings.Cut(long, "="); isLong && joined {
			name = v
		} else if i+1 < len(args) {
			name = args[i+1]
		}
	}
	if at < 0 || name == "" {
		return nil, "", false
	}
	neutral := append([]string(nil), args...)
	if _, v, joined := strings.Cut(args[at], "="); joined {
		neutral[at] = "--create=" + v
	} else {
		neutral[at] = "--create"
	}
	return neutral, "refs/heads/" + name, true
}

// forceFlagResetForm handles `branch -f <name>` and `tag -f <name>`: exactly one
// standalone -f/--force token, and the first positional argument as the name.
func forceFlagResetForm(g gitOptionGrammar, args []string, refPrefix string) ([]string, string, bool) {
	at := -1
	for i, a := range args {
		long, isLong := strings.CutPrefix(a, "--")
		if a != "-f" && (!isLong || !isLongPrefix(long, "force", len("for"))) {
			continue
		}
		if at >= 0 {
			return nil, "", false
		}
		at = i
	}
	if at < 0 {
		return nil, "", false
	}
	neutral := append(append([]string(nil), args[:at]...), args[at+1:]...)
	pos := g.positionals(neutral)
	if len(pos) == 0 {
		return nil, "", false
	}
	return neutral, refPrefix + pos[0], true
}

// refineRefReset lowers a destructive ref-reset call to the write tier when the
// ref it names does not exist and nothing else in the call is destructive.
// refExists reports whether a full ref resolves in the target repository; an
// error, or a nil refExists, keeps the call destructive.
func refineRefReset(sub string, args []string, tier gitTier, refExists func(string) (bool, error)) gitTier {
	if tier != tierDestructive || refExists == nil {
		return tier
	}
	neutral, ref, ok := refResetForm(sub, args)
	if !ok || classifyGit(sub, neutral) != tierWrite {
		return tier
	}
	exists, err := refExists(ref)
	if err != nil || exists {
		return tier
	}
	return tierWrite
}

// refineTier applies refineRefReset to a call, building the repository probe
// only for a call that could be lowered.
func (t *Git) refineTier(ctx context.Context, a gitToolArgs, tier gitTier) gitTier {
	if tier != tierDestructive {
		return tier
	}
	if _, _, ok := refResetForm(a.Subcommand, a.Args); !ok {
		return tier
	}
	return refineRefReset(a.Subcommand, a.Args, tier, t.refExistsIn(ctx, a))
}

// refExistsIn returns the ref-existence probe for the repository a call
// targets, or nil when that repository cannot be resolved inside the
// workspace boundary — which keeps the call destructive.
func (t *Git) refExistsIn(ctx context.Context, a gitToolArgs) func(string) (bool, error) {
	repo, err := t.defaultRepo(ctx, a.Repo)
	if err != nil || repo == "" || t.deps.checkBoundary(ctx, repo) != nil {
		return nil
	}
	root, err := findGitRoot(repo)
	if err != nil {
		return nil
	}
	return func(ref string) (bool, error) {
		cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "rev-parse", "--verify", "--quiet", ref)
		cmd.Dir = root
		out, err := cmd.Output()
		switch {
		case err == nil:
			return true, nil
		case isExitCode(err, 1) && len(out) == 0:
			return false, nil // --verify --quiet: the ref does not exist
		default:
			return false, err
		}
	}
}
