package tools

import (
	"fmt"
	"strings"
)

// GitPolicy is the resolved per-connection gating policy for the git tool.
// It mirrors the [git] config section but lives in the tools package so the
// package keeps its no-import-config boundary (the daemon adapts config →
// policy via GitPolicyFn).
type GitPolicy struct {
	AllowWrites       bool
	AllowDestructive  bool
	AllowPush         bool
	ProtectedBranches []string
}

// GitPolicyFn resolves the current GitPolicy at call time. nil falls back to a
// safe default (writes on, destructive and network off).
type GitPolicyFn = func() GitPolicy

func gateGit(tier gitTier, p GitPolicy, confirm bool) error {
	switch tier {
	case tierRead:
		return nil
	case tierWrite:
		if !p.AllowWrites {
			return fmt.Errorf("git: write operations are disabled; set [git] allow_writes = true to enable")
		}
		return nil
	case tierDestructive:
		if !p.AllowDestructive {
			return fmt.Errorf("git: destructive operations are disabled; set [git] allow_destructive = true to enable")
		}
		if !confirm {
			return fmt.Errorf("git: this destructive operation requires confirm: true")
		}
		return nil
	case tierNetwork:
		if !p.AllowPush {
			return fmt.Errorf("git: network operations (push/fetch/pull) are disabled; set [git] allow_push = true to enable")
		}
		if !confirm {
			return fmt.Errorf("git: this network operation requires confirm: true")
		}
		return nil
	default:
		return fmt.Errorf("git: subcommand is not permitted")
	}
}

// checkPushProtection guards the network tier (push/fetch/pull): it refuses an
// ad-hoc URL/remote on ANY of them (an ext::/scp-like/:// positional remote, or
// any <transport>:: remote-helper form, is a git remote-helper RCE vector —
// `git fetch ext::sh -c <cmd>` runs the command). For `push` it also enforces
// protected branches against a force push (a -f/--force flag OR a +-prefixed
// refspec): a refspec that NAMES a protected branch is refused, and — because
// this guard matches lexically and does not resolve git's symbolic HEAD — a
// force push that relies on the current branch for its destination (`+HEAD`, or
// no refspec so push.default chooses it) is also refused when any branch is
// protected, since it could land on one. The cost of that safe bias is that a
// force push must name its destination branch explicitly. Enforced regardless of
// confirm.
func checkPushProtection(a gitToolArgs, p GitPolicy, tier gitTier) error {
	if tier != tierNetwork {
		return nil
	}
	for _, arg := range a.Args {
		if looksLikeGitURL(arg) {
			return fmt.Errorf("git %s: using an ad-hoc URL/remote is not permitted; use a named remote", a.Subcommand)
		}
	}
	if a.Subcommand != "push" {
		return nil
	}
	if !hasForceFlag(a.Args) && !hasForceRefspec(a.Args) {
		return nil
	}
	for _, arg := range a.Args {
		if isProtectedBranch(arg, p.ProtectedBranches) {
			return fmt.Errorf("git push: force-pushing protected branch %q is not permitted", arg)
		}
	}
	// A force push that targets the current branch (`+HEAD`, or no refspec) names
	// no branch in argv, so the lexical check above cannot tell whether it lands on
	// a protected branch. Refuse it (safe-bias) when any branch is protected,
	// rather than let a possible protected-branch force-push slip through.
	if len(p.ProtectedBranches) > 0 && forcePushTargetsCurrentBranch(a.Args) {
		return fmt.Errorf("git push: refusing a force push with no explicit destination branch (it may target a protected branch); name the branch, e.g. `git push --force origin <branch>`")
	}
	return nil
}

// forcePushTargetsCurrentBranch reports whether a push relies on git's current
// branch for its destination — an explicit HEAD/+HEAD refspec, or no refspec at
// all (push.default). Such a destination is not present in argv, so it cannot be
// matched against the protected list lexically.
func forcePushTargetsCurrentBranch(args []string) bool {
	var positional []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
		}
	}
	// positional[0] (if any) is the remote; the rest are refspecs.
	refspecs := positional
	if len(refspecs) > 0 {
		refspecs = refspecs[1:]
	}
	if len(refspecs) == 0 {
		return true // push.default chooses the current branch
	}
	for _, r := range refspecs {
		if r == "HEAD" || r == "+HEAD" {
			return true
		}
	}
	return false
}

func hasForceFlag(args []string) bool {
	for _, a := range args {
		if a == "-f" || strings.HasPrefix(a, "--force") {
			return true
		}
	}
	return false
}

// hasForceRefspec reports whether any positional refspec is force-prefixed with
// '+' (e.g. "+main" or "+feature:main"), git's non-flag way to force a
// non-fast-forward update — which hasForceFlag does not catch.
func hasForceRefspec(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if strings.HasPrefix(a, "+") {
			return true
		}
	}
	return false
}

func looksLikeGitURL(s string) bool {
	// A scheme URL (https://, git://, ssh://) or a transport remote-helper of the
	// generic `<transport>::<address>` form. ext:: is git's built-in
	// arbitrary-command transport, but ANY `name::` dispatches to a
	// git-remote-<name> helper on PATH, so match `::` generally, not just ext::.
	if strings.Contains(s, "://") || strings.Contains(s, "::") {
		return true
	}
	at := strings.IndexByte(s, '@')
	colon := strings.IndexByte(s, ':')
	return at >= 0 && colon > at // scp-like user@host:path
}

func isProtectedBranch(arg string, protected []string) bool {
	if strings.HasPrefix(arg, "-") {
		return false
	}
	for _, part := range strings.Split(arg, ":") {
		name := strings.TrimPrefix(part, "+")
		name = strings.TrimPrefix(name, "refs/heads/")
		for _, pb := range protected {
			if name == pb {
				return true
			}
		}
	}
	return false
}
