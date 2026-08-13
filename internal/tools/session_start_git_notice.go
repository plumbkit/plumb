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

// projectGitKeys narrows a project's capability-granting policy keys to the
// [git] ones. Matched case-INSENSITIVELY because the spec keeps the spelling the
// project used and go-toml/v2 binds `[GIT]` / `Allow_Push` to the same fields —
// an exact-prefix check would drop exactly the fold variants a reader most needs
// to be told about.
func projectGitKeys(keys []string) []string {
	var out []string
	for _, k := range keys {
		if strings.HasPrefix(strings.ToLower(k), "git.") {
			out = append(out, k)
		}
	}
	return out
}

// formatProjectGitNotice renders the ignored-project-[git] warning, or "" when
// there is nothing to say.
//
// Silent in both quiet cases, which is most sessions: no [git] keys at all
// (nothing was asked for), and trusted (what was asked for is already in force,
// so the policy printed above IS the project's — reporting it would be noise
// about a working feature).
//
// Presence, not value, is what triggers it. The keys come from the project's raw
// TOML, so `allow_destructive = false` is reported as ignored exactly like
// `= true`: the two are indistinguishable in the decoded config, and a user who
// wrote either one and saw nothing happen has the same question.
func formatProjectGitNotice(ws string, keys []string, trusted bool) string {
	gitKeys := projectGitKeys(keys)
	if len(gitKeys) == 0 || trusted {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\nIGNORED — this project's .plumb/config.toml sets %d [git] %s that %s NOT in force: %s.\n",
		len(gitKeys),
		textfmt.Plural(len(gitKeys), "key", "keys"),
		textfmt.Plural(len(gitKeys), "is", "are"),
		strings.Join(gitKeys, ", "))
	sb.WriteString("The policy above is the global one. A project config is untrusted input — cloning a repository ships one — " +
		"so it cannot open the destructive or network git tier, or shorten the protected-branch list, on its own.\n")
	fmt.Fprintf(&sb, "To apply this project's request HERE: review it with `plumb config show --workspace %s`, then run `plumb trust %s`. "+
		"To set the policy everywhere: use the global config (`plumb config show` prints its path) "+
		"or the PLUMB_GIT_* environment variables (PLUMB_GIT_ALLOW_DESTRUCTIVE, PLUMB_GIT_ALLOW_PUSH).\n", ws, ws)
	return sb.String()
}
