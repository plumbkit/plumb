package tools

import (
	"context"
	"os/exec"
	"slices"
	"strings"
)

// git_ref_reset.go keeps routine branch creation at the write tier.
//
// `switch -C <name>`, `checkout -B <name>`, `branch -f <name>`, `branch -M
// [<old>] <name>` and `tag -f <name>` RESET <name> when it already exists,
// which discards its commits the way `reset --keep` does, so classifyGit puts
// them in the destructive tier. When <name> does not exist yet they only
// create it, which is everyday agent work (`checkout -B feature` to start a
// branch). So once the repository is known, refineRefReset lowers the call to
// the write tier, but only when it can show that the call creates a ref:
//
//   - the reset option appears exactly once, however it is spelled. git keeps
//     the LAST `-B`, so a call that names one ref first and another later
//     resets the second;
//   - the name is a plain one (plainRefName). git expands `@{-1}`, `@{u}`,
//     `@{push}` and `<branch>@{upstream}` to a real local branch before it
//     acts, so a probe of the literal text answers about a ref that cannot
//     exist while git resets the one it expands to;
//   - git itself accepts the name and reads it back unchanged (refProbe.plainName),
//     which catches whatever the allowlist did not foresee;
//   - the ref does not exist (refProbe.exists).
//
// Everything else stays destructive: a form this file cannot parse (a bundle
// such as `branch -fv`), another destructive flag on the same call
// (`switch -C x --discard-changes`), a name that is not plain, or git not being
// able to answer. This is an allowlist on purpose. Each way git can spell "that
// existing branch" that this file did not think of would otherwise be a way to
// reset it at the write tier.
//
// Accepted window: a peer that creates the ref between this check and the git
// child would see it reset at the write tier. The check runs before the
// per-repository lock, and plumb-mediated writes from other sessions are also
// what the cross-session ref guard reports; an unmediated creator in that
// instant is a race this classification does not try to close.

const (
	branchRefPrefix = "refs/heads/"
	tagRefPrefix    = "refs/tags/"
)

// refResetForm recognises a call whose only destructive effect may be
// resetting an existing ref. It returns the same call with the reset flag
// replaced by its creating counterpart (or removed), and the full ref name the
// reset would apply to. ok is false for anything else, including forms it does
// not parse confidently and names that are not plain.
func refResetForm(sub string, args []string) (neutral []string, ref string, ok bool) {
	switch sub {
	case "switch":
		neutral, ref, ok = switchResetForm(args)
	case "checkout":
		neutral, ref, ok = checkoutResetForm(args)
	case "branch":
		neutral, ref, ok = branchResetForm(args)
	case "tag":
		neutral, ref, ok = tagResetForm(args)
	}
	if !ok || !plainRefName(ref) {
		return nil, "", false
	}
	return neutral, ref, true
}

// plainRefName reports whether the name in a full refs/heads/ or refs/tags/ ref
// is plain: ASCII letters, digits and `.` `_` `-` `/` only, no leading `-`, no
// `..`, and not ending in `.lock` or `/`. The set is deliberately narrower than
// what git accepts. Every character left out (`@`, `{`, `~`, `^`, `:`, a space)
// is one git gives a meaning of its own, and the name of an ordinary branch or
// release tag does not need them.
func plainRefName(ref string) bool {
	name, ok := strings.CutPrefix(ref, branchRefPrefix)
	if !ok {
		name, ok = strings.CutPrefix(ref, tagRefPrefix)
	}
	if !ok || name == "" || name[0] == '-' || strings.Contains(name, "..") ||
		strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, "/") {
		return false
	}
	for i := range len(name) {
		if !plainRefByte(name[i]) {
			return false
		}
	}
	return true
}

func plainRefByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-' || c == '/'
}

// switchResetForm handles `switch -C <name>` / `--force-create[=]<name>` (the
// tool rewrites -C to --force-create before classification). The create flags
// must occur exactly once between them: a token-level read finds the one
// standalone flag whose name it takes, and git's own option reading
// (bundles such as `-qC`, joined `-Cname`, abbreviations, option values) must
// find that same one and no other.
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
	if at < 0 || name == "" || switchGrammar.count(args, "cC", "create", "force-create") != 1 {
		return nil, "", false
	}
	neutral := slices.Clone(args)
	if _, v, joined := strings.Cut(args[at], "="); joined {
		neutral[at] = "--create=" + v
	} else {
		neutral[at] = "--create"
	}
	return neutral, branchRefPrefix + name, true
}

// checkoutResetForm handles `checkout -B <name> …`, with -B first. -b and -B
// together occur exactly once: git keeps the last of a repeated -B, so the name
// right after the first one is not the ref that gets reset.
func checkoutResetForm(args []string) ([]string, string, bool) {
	if len(args) < 2 || args[0] != "-B" || checkoutGrammar.count(args, "bB") != 1 {
		return nil, "", false
	}
	return append([]string{"-b"}, args[1:]...), branchRefPrefix + args[1], true
}

// branchResetForm handles `branch -f <name>` and `branch -M [<old>] <name>`.
// `-M` is `--move --force`, so it is read as `-m` with the force taken off, and
// a forced move or copy overwrites its LAST positional (the destination), where
// a forced create names the ref in its first.
func branchResetForm(args []string) ([]string, string, bool) {
	at, ok := forceFlagAt(branchGrammar, args, "fM")
	if !ok {
		return nil, "", false
	}
	neutral := slices.Clone(args)
	if args[at] == "-M" {
		neutral[at] = "-m"
	} else {
		neutral = slices.Delete(neutral, at, at+1)
	}
	pos := branchGrammar.positionals(neutral)
	if len(pos) == 0 || len(pos) > 2 {
		return nil, "", false
	}
	name := pos[0]
	if branchGrammar.has(neutral, "mcC", "move", "copy") {
		name = pos[len(pos)-1]
	}
	return neutral, branchRefPrefix + name, true
}

// tagResetForm handles `tag -f <name>`: exactly one standalone -f/--force, and
// the first positional argument as the name.
func tagResetForm(args []string) ([]string, string, bool) {
	at, ok := forceFlagAt(tagGrammar, args, "f")
	if !ok {
		return nil, "", false
	}
	neutral := slices.Delete(slices.Clone(args), at, at+1)
	pos := tagGrammar.positionals(neutral)
	if len(pos) == 0 {
		return nil, "", false
	}
	return neutral, tagRefPrefix + pos[0], true
}

// forceFlagAt returns the index of the one standalone token that spells a
// ref-moving option: --force (or an abbreviation of at least three letters) or
// a short flag in shorts. It reports false for none, for several, and when
// git's reading of args finds a different number — a bundle (`-fq`), a joined
// value (`--force=x`), an abbreviation shorter than the token read accepts, or
// a `-f` that is really the value of another option.
func forceFlagAt(g gitOptionGrammar, args []string, shorts string) (int, bool) {
	at := -1
	for i, a := range args {
		long, isLong := strings.CutPrefix(a, "--")
		short := len(a) == 2 && a[0] == '-' && strings.IndexByte(shorts, a[1]) >= 0
		if !short && (!isLong || !isLongPrefix(long, "force", len("for"))) {
			continue
		}
		if at >= 0 {
			return 0, false
		}
		at = i
	}
	return at, at >= 0 && g.count(args, shorts, "force") == 1
}

// refProbe asks git about the refs of the repository a call targets. Its funcs
// close over the call's context and repository, so a probe is never reused
// across calls.
type refProbe struct {
	// plainName reports whether git accepts the full ref name and reads it as
	// exactly the ref it spells: valid, and not expanded or rewritten to another.
	plainName func(ref string) bool
	// exists reports whether the full ref resolves. An error means git could not
	// say.
	exists func(ref string) (bool, error)
}

// refineRefReset lowers a destructive ref-reset call to the write tier when the
// ref it names is a plain name that does not exist and nothing else in the call
// is destructive. A nil probe, one git cannot answer, and every other case keep
// the call destructive.
func refineRefReset(sub string, args []string, tier gitTier, probe *refProbe) gitTier {
	if tier != tierDestructive || probe == nil {
		return tier
	}
	neutral, ref, ok := refResetForm(sub, args)
	if !ok || classifyGit(sub, neutral) != tierWrite || !probe.plainName(ref) {
		return tier
	}
	exists, err := probe.exists(ref)
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
	return refineRefReset(a.Subcommand, a.Args, tier, t.refProbeIn(ctx, a))
}

// refProbeIn returns the ref probe for the repository a call targets, or nil
// when that repository cannot be resolved inside the workspace boundary —
// which keeps the call destructive.
func (t *Git) refProbeIn(ctx context.Context, a gitToolArgs) *refProbe {
	repo, err := t.defaultRepo(ctx, a.Repo)
	if err != nil || repo == "" || t.deps.checkBoundary(ctx, repo) != nil {
		return nil
	}
	root, err := findGitRoot(repo)
	if err != nil {
		return nil
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{gitNoOptionalLocks}, args...)...) //nolint:gosec // G204: argv is literals plus a full ref (it starts with refs/) or a branch name that gitReadsRefPlainly refuses when dash-leading, so it cannot be read as an option
		cmd.Dir = root
		return cmd.Output()
	}
	return &refProbe{
		plainName: func(ref string) bool { return gitReadsRefPlainly(run, ref) },
		exists:    func(ref string) (bool, error) { return gitRefExists(run, ref) },
	}
}

// gitReadsRefPlainly asks git's own name check about a full ref. For a branch,
// `check-ref-format --branch` expands `@{-N}`, `@{upstream}` and the like, so
// the name it prints back differs from the one given exactly when git would
// have acted on another branch. A tag name has no such expansion, so it only
// has to be valid.
func gitReadsRefPlainly(run func(args ...string) ([]byte, error), ref string) bool {
	if name, ok := strings.CutPrefix(ref, branchRefPrefix); ok {
		if strings.HasPrefix(name, "-") {
			return false // git would read it as an option
		}
		out, err := run("check-ref-format", "--branch", name)
		return err == nil && strings.TrimSuffix(string(out), "\n") == name
	}
	_, err := run("check-ref-format", ref)
	return err == nil
}

// gitRefExists asks `rev-parse --verify --quiet`, which exits 1 with no output
// for a ref that does not exist and fails differently for one git cannot read.
func gitRefExists(run func(args ...string) ([]byte, error), ref string) (bool, error) {
	out, err := run("rev-parse", "--verify", "--quiet", ref)
	switch {
	case err == nil:
		return true, nil
	case isExitCode(err, 1) && len(out) == 0:
		return false, nil
	default:
		return false, err
	}
}
