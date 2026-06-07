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

// checkPushProtection refuses pushing to an ad-hoc URL and force-pushing any
// protected branch, regardless of confirm.
func checkPushProtection(a gitToolArgs, p GitPolicy, tier gitTier) error {
	if tier != tierNetwork || a.Subcommand != "push" {
		return nil
	}
	for _, arg := range a.Args {
		if looksLikeGitURL(arg) {
			return fmt.Errorf("git push: pushing to an ad-hoc URL is not permitted; use a named remote")
		}
	}
	if !hasForceFlag(a.Args) {
		return nil
	}
	for _, arg := range a.Args {
		if isProtectedBranch(arg, p.ProtectedBranches) {
			return fmt.Errorf("git push: force-pushing protected branch %q is not permitted", arg)
		}
	}
	return nil
}

func hasForceFlag(args []string) bool {
	for _, a := range args {
		if a == "-f" || strings.HasPrefix(a, "--force") {
			return true
		}
	}
	return false
}

func looksLikeGitURL(s string) bool {
	if strings.Contains(s, "://") || strings.HasPrefix(s, "ext::") {
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
