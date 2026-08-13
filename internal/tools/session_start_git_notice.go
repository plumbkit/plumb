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
// An unrecognised field (a misspelling, or a [git] field added later and not
// classified here) cannot be compared, so it is reported. Over-reporting costs a
// line the reader can check; under-reporting is the silence this file exists to
// end.
func gitKeyInForce(field string, want any, p GitPolicy) bool {
	switch field {
	case "allow_writes":
		return boolIs(want, p.AllowWrites)
	case "allow_destructive":
		return boolIs(want, p.AllowDestructive)
	case "allow_push":
		return boolIs(want, p.AllowPush)
	case "commit_trailer":
		return boolIs(want, p.CommitTrailer)
	case "protected_branches":
		return stringListIs(want, p.ProtectedBranches)
	}
	return false
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

// droppedGitKeys narrows a project's capability request to the [git] keys whose
// requested value is genuinely not what this session resolved.
func droppedGitKeys(keys []ProjectGitKey, p GitPolicy) []string {
	var out []string
	for _, e := range keys {
		field, ok := gitPolicyField(e.Key)
		if !ok || gitKeyInForce(field, e.Value, p) {
			continue
		}
		out = append(out, e.Key)
	}
	return out
}

// formatProjectGitNotice renders the ignored-project-[git] warning, or "" when
// there is nothing to say.
//
// Silent in the quiet cases, which is most sessions: no [git] keys at all
// (nothing was asked for), trusted (what was asked for is already in force, so
// the policy printed above IS the project's), and every asked-for key already
// matching the resolved policy.
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
		return ""
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
	fmt.Fprintf(&sb, "To apply this project's request HERE: review it with `plumb config show --workspace %q`, then run "+
		"`plumb trust %q` (it prompts; `--yes` grants it non-interactively). To set the policy everywhere: use the global "+
		"config (`plumb config show` prints its path) or the PLUMB_GIT_* environment variables (PLUMB_GIT_ALLOW_WRITES, "+
		"PLUMB_GIT_ALLOW_DESTRUCTIVE, PLUMB_GIT_ALLOW_PUSH, PLUMB_GIT_COMMIT_TRAILER).\n", ws, ws)
	sb.WriteString("Either takes effect when this workspace is next attached (a new session, `plumb restart`, or a re-pin), " +
		"NOT mid-session: the policy above and this notice are one snapshot, so both will keep saying this until then.\n")
	return sb.String()
}

// unreadableProjectConfigNotice covers the last silent case: a .plumb/config.toml
// that cannot be parsed is skipped whole, so its [git] block is just as ignored
// as an untrusted one — and with nothing said, the agent has even less to go on,
// because there is no `plumb trust` to reach for.
func unreadableProjectConfigNotice(ws string) string {
	return fmt.Sprintf("\nIGNORED — this project's .plumb/config.toml could not be parsed, so NOTHING in it is being applied, "+
		"[git] included: the policy above is what plumb resolved without it. Check the file, then see the parse error with "+
		"`plumb config show --workspace %q`.\n", ws)
}
