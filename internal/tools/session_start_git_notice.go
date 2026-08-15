package tools

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// session_start_git_notice.go renders the one thing the git-policy section could
// not say: that this workspace's .plumb/config.toml asked for a DIFFERENT git
// policy and was overruled.
//
// Why it belongs in orientation. An untrusted project [git] block is forced back
// to the global config (see config.forceCapabilityFieldsToBase) — the right
// default, since cloning a repository ships a .plumb/config.toml and honouring
// it unasked would hand a hostile repo history destruction and pushes with the
// user's credentials. That drop was already announced to the HUMAN (the daemon
// log, `plumb doctor`, `plumb config show`, the TUI) but not to the AGENT, whose
// only view of the policy is this section. So an agent read "Push/fetch/pull:
// off", could not reconcile it with the `allow_push = true` sitting in the repo
// it was looking at, concluded the tier was unimplemented, and shelled out to
// raw git — bypassing the very policy the drop exists to enforce. A safety
// feature that cannot explain itself is indistinguishable from a bug.
//
// The notice is a SNAPSHOT, taken with the policy it annotates. Both come from
// one config apply (internal/cli, applyProjectConfig), so the two can never
// describe different points in time — the failure the remediation text would
// otherwise walk a user straight into: run `plumb trust`, silence the notice,
// and leave the policy exactly as restrictive as before with nothing left on
// screen to explain it.

// ProjectGitKey is one key a project's .plumb/config.toml sets in a
// capability-granting section, with the value the PROJECT asked for — never the
// value in force, which is the global one whenever the request is untrusted.
type ProjectGitKey struct {
	Key   string
	Value any
}

// ProjectGitStatus is a session's captured view of its workspace's
// capability-granting project-config request. Unreadable means the project
// config could not be parsed at all, so none of it — [git] included — is being
// applied; Keys/Trusted are meaningless in that state.
type ProjectGitStatus struct {
	Keys       []ProjectGitKey
	Trusted    bool
	Unreadable bool
}

// gitPolicyField splits a "git.<field>" policy key. Matched case-INSENSITIVELY
// because the spec keeps the spelling the project used and go-toml/v2 binds
// `[GIT]` / `Allow_Push` to the same fields — an exact-prefix check would drop
// exactly the fold variants a reader most needs to be told about.
func gitPolicyField(key string) (string, bool) {
	return strings.CutPrefix(strings.ToLower(key), "git.")
}

// gitKeyInForce reports whether the value a project asked for is already what
// this session resolved, in which case naming the key as "NOT in force" would be
// a false claim with a no-op remediation attached — the common shape being a
// user who set the tiers globally and then cloned a repository that asks for
// them too.
//
// known is false for a field this function cannot judge: a misspelling, or
// a [git] field with no counterpart in the resolved policy at all (`env` is the
// standing case — it reaches the git child's environment, never GitPolicy). The
// two callers need that distinction to point in OPPOSITE directions, so it is
// returned rather than folded into inForce — see droppedGitKeys and
// overriddenGitKeys.
func gitKeyInForce(field string, want any, p GitPolicy) (inForce, known bool) {
	switch field {
	case "allow_writes":
		return boolIs(want, p.AllowWrites), true
	case "allow_destructive":
		return boolIs(want, p.AllowDestructive), true
	case "allow_push":
		return boolIs(want, p.AllowPush), true
	case "commit_trailer":
		return boolIs(want, p.CommitTrailer), true
	case "protected_branches":
		return stringListIs(want, p.ProtectedBranches), true
	}
	return false, false
}

func boolIs(want any, have bool) bool {
	b, ok := want.(bool)
	return ok && b == have
}

// stringListIs compares a raw TOML array against a resolved string slice. A
// value of any other shape is "not equal", so it is reported rather than
// silently treated as satisfied.
func stringListIs(want any, have []string) bool {
	list, ok := want.([]any)
	if !ok || len(list) != len(have) {
		return false
	}
	for i, v := range list {
		s, ok := v.(string)
		if !ok || s != have[i] {
			return false
		}
	}
	return true
}

// droppedGitKeys narrows an UNTRUSTED project's capability request to the [git]
// keys whose requested value is genuinely not what this session resolved.
//
// A field that cannot be compared is REPORTED here: the whole [git] table was
// forced back, so it really was dropped, and over-reporting costs a line the
// reader can check against the policy above. Under-reporting is the silence this
// file exists to end.
func droppedGitKeys(keys []ProjectGitKey, p GitPolicy) []string {
	var out []string
	for _, e := range keys {
		field, ok := gitPolicyField(e.Key)
		if !ok {
			continue
		}
		if inForce, _ := gitKeyInForce(field, e.Value, p); inForce {
			continue
		}
		out = append(out, e.Key)
	}
	return out
}

// overriddenGitKeys narrows a TRUSTED project's [git] request to the keys whose
// requested value is PROVABLY not what this session resolved.
//
// The asymmetry with droppedGitKeys is deliberate and runs the other way. A
// trusted [git] table is applied whole, so a field this package cannot compare
// is in force — `git.env` has no counterpart in GitPolicy at all — and naming it
// would invent an override that does not exist. Only a recognised field that
// genuinely differs is reported, because only that can be checked by the reader
// against the policy printed above.
func overriddenGitKeys(keys []ProjectGitKey, p GitPolicy) []string {
	var out []string
	for _, e := range keys {
		field, ok := gitPolicyField(e.Key)
		if !ok {
			continue
		}
		inForce, known := gitKeyInForce(field, e.Value, p)
		if !known || inForce {
			continue
		}
		out = append(out, e.Key)
	}
	return out
}

// shellQuote renders a path for a command line an agent may copy verbatim.
//
// Go's %q is NOT a shell quote. It handles spaces, quotes and backslashes, but
// leaves `$` untouched, so a workspace under a directory literally named `$WORK`
// would be expanded by the shell into a different path — the same silently
// wrong-target failure the space case fixed, reached by a rarer spelling. POSIX
// single quotes suppress every expansion; the only character needing care inside
// them is the single quote itself, spliced out and back with the standard POSIX
// close-escape-reopen idiom.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// formatProjectGitNotice renders the ignored-project-[git] warning, or "" when
// there is nothing to say.
//
// Silent in the quiet cases, which is most sessions: no [git] keys at all
// (nothing was asked for), and every asked-for key already matching the resolved
// policy — including the ordinary trusted session, where the grant is what put
// them there.
//
// Trust is NOT a short-circuit, and that is the fix for the bug this notice was
// written to end reappearing under a narrower stencil. LoadProjectWithPolicy
// applies PLUMB_GIT_* AFTER forcing an untrusted [git] back to base, so env is
// the highest layer either way: a trusted `allow_push = true` plus
// PLUMB_GIT_ALLOW_PUSH=0 resolves to push OFF. Returning early on Trusted put
// exactly that state back into the original silence — `Push/fetch/pull: off.`
// against an approved `allow_push = true`, with nothing on screen to explain it.
// It reads as reachable-but-harmless because the per-key comparison below
// already keeps the trusted-AND-applied case quiet, which is the whole of what
// the short-circuit was doing.
//
// Presence, not value, is what triggers it for a key that DOES differ. The keys
// come from the project's raw TOML, so `allow_destructive = false` against an
// open tier is reported exactly like `= true` against a closed one: both were
// read and overruled, and the user who wrote either has the same question.
func formatProjectGitNotice(ws string, st ProjectGitStatus, p GitPolicy) string {
	if st.Unreadable {
		return unreadableProjectConfigNotice(ws)
	}
	if st.Trusted {
		return trustedGitOverrideNotice(ws, overriddenGitKeys(st.Keys, p))
	}
	dropped := droppedGitKeys(st.Keys, p)
	if len(dropped) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\nIGNORED — this project's .plumb/config.toml sets %d [git] %s that %s NOT in force: %s.\n",
		len(dropped),
		textfmt.Plural(len(dropped), "key", "keys"),
		textfmt.Plural(len(dropped), "is", "are"),
		strings.Join(dropped, ", "))
	sb.WriteString("The policy above is the global one. A project config is untrusted input — cloning a repository ships one — " +
		"so plumb takes the WHOLE [git] table from the global config until this project's request is approved. " +
		"That runs in both directions: unapproved, a project can neither open the destructive or network tier, shorten the " +
		"protected-branch list, nor turn writes or the commit trailer off.\n")
	fmt.Fprintf(&sb, "To apply this project's request HERE: review it with `plumb config show --workspace %s`, then run "+
		"`plumb trust %s` (it prompts; `--yes` grants it non-interactively). To set the policy everywhere: use the global "+
		"config (`plumb config show` prints its path) or the PLUMB_GIT_* environment variables (PLUMB_GIT_ALLOW_WRITES, "+
		"PLUMB_GIT_ALLOW_DESTRUCTIVE, PLUMB_GIT_ALLOW_PUSH, PLUMB_GIT_COMMIT_TRAILER).\n",
		shellQuote(ws), shellQuote(ws))
	sb.WriteString(gitRemediationTiming)
	return sb.String()
}

// gitRemediationTiming states when each remediation above actually lands. The
// three routes differ, and a single "not mid-session" for all of them is wrong
// in BOTH directions: it under-promises the global config, which the daemon
// watches and re-applies to live sessions, and it over-promises PLUMB_GIT_*,
// which is read from the daemon PROCESS's environment — so an agent that
// exports the variable, re-runs session_start and sees no change lands straight
// back in the incident this notice exists to end.
const gitRemediationTiming = "The three routes land at DIFFERENT moments. " +
	"`plumb trust` writes a file nothing watches, so its grant reaches this session only when the workspace is next " +
	"attached — a new session, `plumb restart`, or a re-pin — NOT mid-session; until then the policy above and this " +
	"notice are one snapshot and will keep saying exactly this. " +
	"Editing the GLOBAL config DOES take effect mid-session: the daemon watches that file and re-applies it to every " +
	"live session (`plumb config reload` forces the same pass). " +
	"A PLUMB_GIT_* variable is read from the DAEMON's environment, not yours, so exporting it and then starting a new " +
	"session or re-pinning changes NOTHING — the daemon has to be restarted with the variable already set.\n"

// trustedGitOverrideNotice covers the state the trust grant does not settle: the
// request was approved and STILL is not what the session resolved.
//
// It is reachable because env is applied last (see formatProjectGitNotice), and
// it needs its own wording because the untrusted notice's advice is actively
// wrong here — `plumb trust` is already granted, so recommending it sends the
// reader to re-approve something that is not the obstacle.
func trustedGitOverrideNotice(ws string, overridden []string) string {
	if len(overridden) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\nOVERRIDDEN — this project's [git] request is TRUSTED, yet %d [git] %s %s still NOT in force: %s.\n",
		len(overridden),
		textfmt.Plural(len(overridden), "key", "keys"),
		textfmt.Plural(len(overridden), "is", "are"),
		strings.Join(overridden, ", "))
	sb.WriteString("`plumb trust` will NOT help — the grant is already given, and the policy above is what plumb resolved " +
		"WITH it. A higher layer is winning: the PLUMB_GIT_* environment variables (PLUMB_GIT_ALLOW_WRITES, " +
		"PLUMB_GIT_ALLOW_DESTRUCTIVE, PLUMB_GIT_ALLOW_PUSH, PLUMB_GIT_COMMIT_TRAILER) are applied AFTER the project " +
		"config, so one set in the environment the DAEMON was started with beats your approved value.\n")
	fmt.Fprintf(&sb, "Confirm with `plumb config show --workspace %s`, which prints each value's provenance. To let this "+
		"project's value win, unset the variable and restart the daemon (`plumb restart`): it is read from the daemon's "+
		"own environment, so a new session or a re-pin against the running daemon picks up nothing.\n", shellQuote(ws))
	return sb.String()
}

// unreadableProjectConfigNotice covers the last silent case: a .plumb/config.toml
// that cannot be parsed is skipped whole, so its [git] block is just as ignored
// as an untrusted one — and with nothing said, the agent has even less to go on,
// because there is no `plumb trust` to reach for.
//
// It states what the FILE contributed (nothing) AND what the policy therefore
// is, which it can only do because applyProjectConfig now reverts to the global
// config on a parse failure rather than leaving the last successful load
// standing.
//
// This notice previously said the opposite — that the policy was "whatever this
// session last resolved successfully", possibly another workspace's trusted
// grant — and that was accurate while the carryover existed (PLAN-309). It is
// now false, and saying it would be false in the direction that matters: a
// reader told the tier might not be theirs will go looking for an elevation that
// is no longer possible. internal/cli's TestProjectGitStatus_RePinDropsThePreviousGrant
// pins the behaviour this sentence depends on.
func unreadableProjectConfigNotice(ws string) string {
	return fmt.Sprintf("\nIGNORED — this project's .plumb/config.toml could not be parsed, so it was skipped WHOLE: nothing "+
		"it asks for, [git] included, was read from it. The policy above is the GLOBAL one: a config that cannot be "+
		"read is treated as no config at all, so nothing from an earlier readable version of this file, and nothing "+
		"from a workspace this session was previously pinned to, is still standing. Fix the file and it applies on the "+
		"next watcher pass — see the parse error with `plumb config show --workspace %s`.\n",
		shellQuote(ws))
}
