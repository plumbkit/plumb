package tools

import (
	"fmt"
	"strings"
)

// git_untrusted_note.go explains a tier refusal whose real cause is an
// untrusted project config (#530).
//
// session_start's git-policy section already reports an ignored project [git]
// request, but the refusal an agent actually meets is the git tool's own, and it
// read "network operations (push/fetch/pull) are disabled; set [git] allow_push
// = true to enable" — while the repository in front of it sets exactly that.
// The linked-worktree case made it worse: the same checked-in config was
// honoured on the main checkout and not in the worktree, and nothing said that
// trust is recorded per path. So when the project asked for the refused tier and
// the request is untrusted here, the refusal says so and names the command.

// tierGitField is the [git] key whose value opens each gated tier.
var tierGitField = map[gitTier]string{
	tierWrite:       "allow_writes",
	tierDestructive: "allow_destructive",
	tierNetwork:     "allow_push",
}

// untrustedTierNote returns the text appended to a tier refusal when the
// project config asked for that tier and is untrusted at this path, or "" when
// that is not why the tier is off: a trusted request, one that never named the
// tier's key, one whose value is already in force, or no project config.
func untrustedTierNote(st ProjectGitStatus, tier gitTier, p GitPolicy) string {
	field, gated := tierGitField[tier]
	if !gated || st.Trusted || st.Unreadable || st.Workspace == "" {
		return ""
	}
	var key string
	for _, k := range droppedGitKeys(st.Keys, p) {
		if f, _ := gitPolicyField(k); f == field {
			key = k
			break
		}
	}
	if key == "" {
		return ""
	}
	ws := shellQuote(st.Workspace)
	return fmt.Sprintf(". This workspace's .plumb/config.toml sets %s, but that project config is untrusted at %s, "+
		"so the global [git] policy is in force here. Trust is recorded per path and bound to the config's exact content: "+
		"a linked worktree shares its repository's grant only while its capability config is identical to the approved one. "+
		"To honour it here, review it with `plumb config show --workspace %s`, then run `plumb trust %s`; the grant applies "+
		"when the workspace is next attached (a new session, a re-pin, or `plumb restart`)",
		strings.ToLower(key), st.Workspace, ws, ws)
}
