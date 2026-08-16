package tools

import (
	"fmt"
	"strings"
)

// session_start_collab.go states the resolved [collab] policy — mailbox,
// intents, cross-project, findings — the same way writeSessionGitPolicy
// states the resolved git policy: up front, so an agent learns the gates
// before discovering one via a refused call. Emitted only when a peer is
// active, since the policy is otherwise not yet relevant to this session.

// writeSessionCollabPolicy appends a "## Collab" section naming the resolved
// [collab] gates when another session is active on this workspace. Nil-safe
// (skipped when the mailbox snapshot is unwired).
func (t *SessionStart) writeSessionCollabPolicy(sb *strings.Builder, ws string) {
	if t.mailboxFn == nil || len(t.activePeers(ws)) == 0 {
		return
	}
	_, inbox := t.mailboxFn()
	sb.WriteString("## Collab (live policy)\n\n")
	sb.WriteString(formatCollabPolicy(inbox.Policy))
	sb.WriteString("\n")
}

// formatCollabPolicy renders the collab policy body. Pure — no I/O.
func formatCollabPolicy(p CollabPolicy) string {
	return fmt.Sprintf("collab: mailbox %s, intents %s, cross-project %s, findings %s\n",
		gitGateLabel(p.Mailbox), gitGateLabel(p.Intents), gitGateLabel(p.CrossProject), gitGateLabel(p.KnowledgeHandoff))
}
