package cli

// hooks_claude_clear.go — the SessionStart hook's half of the /clear handover
// (#564): telling the daemon that the conversation which just started was begun by
// /clear, so the connection can pass its identity on to it (conn_clear_handover.go).
//
// Claude Code fires SessionStart with `source` set to startup, resume, clear or
// compact. Only clear starts a new conversation id on a connection that already
// carries the old one, so it is the only source that sends anything. The hook has
// no way to name the conversation that was cleared, and sends none: the daemon
// matches the new id against the first call that carries it.
//
// Failure policy, as for every hook in this package: fail OPEN, and say nothing.
// The notification is best effort and its absence costs only the handover — the new
// conversation then gets an identity of its own — so it must never delay, break or
// add a byte to the hook's output, which is the linkage sentence and nothing else.

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// clearNotifyBudget is how long the hook may spend telling the daemon: the dial and
// the reply together. The hook's own timeout is 5 s (claudeHookEntries); a starved
// but alive daemon answers in well under a second, and a wedged one must not hold
// the start of a conversation.
const clearNotifyBudget = time.Second

// sessionSourceClear is SessionStart's `source` for a conversation begun by /clear,
// in both Claude Code's and Codex's hook input (hooks_codex.go).
const sessionSourceClear = "clear"

// runClaudeSessionStart is the SessionStart command body: tell the daemon when the
// conversation began with /clear, then print the linkage sentence. notify is
// injected so tests need no daemon. The notification comes first, so the daemon
// holds the marker before the model has been told anything it could call with.
func runClaudeSessionStart(input claudeHookInput, notify func(conversationID string), stdout io.Writer) {
	id := strings.TrimSpace(input.SessionID)
	if id == "" {
		return
	}
	if input.Source == sessionSourceClear && notify != nil {
		notify(id)
	}
	// Plain stdout reaches the agent for this event, so the linkage sentence needs
	// no JSON envelope.
	fmt.Fprintln(stdout, sessionLinkageSentence(id, "conversation"))
}

// notifyConversationCleared sends conversation-cleared <id> to the daemon's control
// socket. Every failure is swallowed: no daemon, a daemon too old to know the
// command (it answers `error: unknown command`), a timeout.
func notifyConversationCleared(conversationID string) {
	if strings.ContainsAny(conversationID, "\r\n") {
		return // one command per line; an id that spans lines is not an id
	}
	_, _, _ = askDaemonCtrl(ctrlConversationClearedCommand+" "+conversationID, time.Now().Add(clearNotifyBudget))
}
