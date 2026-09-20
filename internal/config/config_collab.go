package config

// CollabConfig controls cross-agent sharing — the passive peer-awareness layer
// that surfaces what other sessions on the same workspace have done. Phase 1 is
// strictly advisory and derived from writes the daemon itself performed or
// watched (never from agent claims): topology-annotated recent writes, a bounded
// peer-activity hint on path-bearing tool responses, and a session_start peer
// digest. No environment override.
//
// Split by trust: the four CHANNEL switches (Intents, Mailbox, CrossProject,
// KnowledgeHandoff) are gated on `plumb trust` — a project may ask, but a channel
// it can open for itself is not one the user consented to (see
// forceCapabilityFieldsToBase). PeerAwareness and the budgets stay freely
// project-overridable in both directions; tuning opens nothing.
//
// Concurrency: read-only after Load returns.
type CollabConfig struct {
	// PeerAwareness turns on the tier-1 peer-awareness signals: topology-annotated
	// recent_writes in workspace_sessions, the peer-activity hint injected into
	// path-bearing tool responses, and the session_start peer digest. Default true
	// — it is a richer version of behaviour plumb already ships. Set false (globally
	// or per project, either direction) to fall back to bare, unannotated output.
	PeerAwareness bool `toml:"peer_awareness"`
	// HintBudgetBytes caps any injected peer-signal block (the peer-activity hint
	// and the session_start peer digest) in bytes, enforced on a UTF-8 boundary.
	// Default 512, mirroring [memory].hint_budget_bytes.
	HintBudgetBytes int `toml:"hint_budget_bytes"`
	// Intents gates the phase-2 tier: the share_intent tool, its listing in
	// workspace_sessions, and the intent-aware write hint. Opt-in, default false —
	// it introduces agent-authored claims (unlike PeerAwareness's observed facts).
	// Gated on `plumb trust`: a project may request it, the user grants it.
	Intents bool `toml:"intents"`
	// Mailbox gates the agent-to-agent mailbox: the leave_note and check_messages
	// tools, message delivery at session_start and on ordinary tool results, and
	// pending-message listing in workspace_sessions. Default true, and scoped to
	// the workspace: sessions on the SAME project talk to each other, so the
	// messages are about shared work and the bodies never leave the project.
	// Reaching another project is a separate, off-by-default decision (see
	// CrossProject). Messages are agent-authored and advisory — never blocking.
	Mailbox bool `toml:"mailbox"`
	// AllowUnidentifiedWrites turns OFF the shared-connection write ceiling: the
	// refusal of a state-changing call that arrives on a connection serving
	// several logical agents with no per-call identity to attribute it to.
	//
	// Opt-in, default false, and GLOBAL-ONLY — a project's .plumb/config.toml
	// cannot set it, because a cloned repository must not be able to disable a
	// guard on the machine that opened it.
	//
	// It exists because the ceiling can otherwise have no reachable remedy. Its
	// refusal tells the caller to identify itself, which assumes the client has
	// a channel to do that with; a client whose runtime drops the per-call
	// identity stamp has none, so on a shared connection EVERY write is refused
	// permanently and the advice cannot be followed. Observed in the field: a
	// user on such a client lost their write lane entirely.
	//
	// What it costs is real and is the whole reason it defaults off: with the
	// ceiling down, a write from an unattributable caller lands in whichever
	// agent's shard the connection resolves to, so one agent's edit can be
	// recorded against another's tracker, and a peer's pin can be reset under
	// it. That is a trade a single human on their own machine may reasonably
	// accept and an orchestrator running untrusted agents must not.
	AllowUnidentifiedWrites bool `toml:"allow_unidentified_writes"`
	// CrossProject lets this session RECEIVE messages from sessions pinned to a
	// different workspace. Opt-in, default false, and deliberately the recipient's
	// decision rather than the sender's, so another project can never inject text
	// into this one's context uninvited. leave_note checks this before sending:
	// an un-opted-in recipient refuses the send up front, naming the reason,
	// rather than accepting a message that would otherwise sit unclaimed until it
	// expires.
	//
	// Gated on `plumb trust`: "the recipient's decision" would mean nothing if the
	// recipient's own repository — an untrusted surface a clone ships — could flip
	// it unasked. A project may request it; the user grants it per workspace.
	CrossProject bool `toml:"cross_project"`
	// MaxExchanges caps how many messages one conversation may hold before further
	// replies are refused — the backstop against two agents answering each other
	// indefinitely with no human in the loop. plumb cannot observe a human turn, so
	// it counts total messages in a thread, not consecutive replies. Default 10.
	MaxExchanges int `toml:"max_exchanges"`
	// ChatBudgetBytes caps a single delivered message body (UTF-8 boundary).
	// Separate from HintBudgetBytes: a message is content to act on, not a pointer
	// to look up. Default 2048.
	ChatBudgetBytes int `toml:"chat_budget_bytes"`
	// MaxWaitSeconds caps how long check_messages blocks waiting for a message,
	// below the client's own MCP call timeout so a wait expires cleanly. Default 55.
	MaxWaitSeconds int `toml:"max_wait_seconds"`
	// WakeWindowSeconds is the BASE window the Claude Code Stop-hook watcher polls
	// this session's mailbox for after a turn ends. It is what a session with no
	// live peer costs: one detached plumb process, polling, for this long. Default
	// 300. Read by `plumb hooks run-claude`, not by the daemon.
	//
	// The installed handler's own timeout is derived from the CEILING below, so a
	// change here or below is only live once `plumb hooks install claude-code` has
	// rewritten it — `plumb hooks` reports the mismatch as stale.
	WakeWindowSeconds int `toml:"wake_window_seconds"`
	// WakePeerWindowSeconds is the ceiling that window may extend to while another
	// live session shares this workspace — the only case where a peer can write to
	// this mailbox at all. The watcher slides its deadline forward by one base
	// window per poll for as long as a peer is there, so a solo session keeps the
	// base window and its cost, and a real multi-agent session stays reachable for
	// up to this long. Default 3600. Set 0 to disable the extension, which restores
	// the single fixed window this key replaced.
	WakePeerWindowSeconds int `toml:"wake_peer_window_seconds"`
	// IntentTTLMinutes is the expiry (in minutes) applied to a new intent or note.
	// Rows past expiry are pruned on the daemon session-reaper tick and filtered
	// from every read regardless. Default 120. A non-positive value falls back to
	// the compiled default at the point of use.
	IntentTTLMinutes int `toml:"intent_ttl_minutes"`
	// NoteTTLMinutes is the expiry (in minutes) applied to a new note while it
	// is UNREAD — delivery under KeepDeliveredNotes supersedes it, so this is
	// the unread window, not the row's lifetime. A non-positive value follows
	// IntentTTLMinutes, which is the expiry notes shared before this key
	// existed; that fallback is what makes the key a no-change upgrade.
	NoteTTLMinutes int `toml:"note_ttl_minutes"`
	// KeepDeliveredNotes stamps a far-future expiry over each note at delivery:
	// the read flag becomes the permanence trigger, and a conversation survives
	// as a transcript while unclaimed mail still ages out per the TTLs above.
	// Default false. Retention, not a channel — and the RECIPIENT's policy:
	// whoever claims a row decides its retention, including in the shared
	// daemon-level cross-project store.
	KeepDeliveredNotes bool `toml:"keep_delivered_notes"`
	// KnowledgeHandoff gates the phase-3 tier: the share_findings tool, which
	// flushes an agent's findings through the episodic-memory pipeline on demand
	// (redact → provenance → markdown under .plumb/memories/ → FTS index →
	// generated_memory_keep retention) rather than only when a session goes idle.
	// Opt-in, default false — the finding is agent-authored generated content,
	// lower-confidence than a user-written memory. Gated on `plumb trust`: a
	// project may request it, the user grants it.
	KnowledgeHandoff bool `toml:"knowledge_handoff"`
}
