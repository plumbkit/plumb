package tools

// state_changing.go — the set the shared-connection write gate asks about,
// owned by the gate rather than borrowed from a display feed.
//
// The gate (internal/cli refuseSharedStateChange) refuses a mutating call that
// arrives on a connection serving several logical agents with no per-call
// identity, because such a call cannot be attributed to the agent that issued
// it. It used to ask WriteToolNames — a list assembled for the peer-awareness
// recent-writes FEED, where the cost of omitting a tool is a missing line in a
// listing. Reused as a security gate the same omission means an unattributable
// call runs, so the list's coverage was arbitrary with respect to the question
// being asked of it, and seven mutating tools were ungated.
//
// Two lists, because they answer two questions and drift apart honestly:
// WriteToolNames stays the feed's (what is worth SHOWING a peer), and this one
// is the gate's (what must not run unattributed). A parity test derives the
// gate's universe from the live tool registration, so a new tool fails until
// somebody classifies it — see TestStateChangeGateCoversEveryMutatingTool.
// PLAN-440 item 1.

// stateChangingToolNames is every registered tool whose call changes state a
// peer agent on the same connection could observe or lose: the filesystem, the
// repository, the workspace's memories and collab store, the session registry,
// or anything an arbitrary command can reach.
//
// session_start is deliberately ABSENT. It is state-changing — its workspace
// argument re-pins — but it is also the only channel through which an agent on
// a connection that cannot stamp per-call identity declares itself. Refusing it
// anonymously would make identity undeclarable and the connection permanently
// unusable, so its state change is guarded where it happens instead: repinAgent's
// sticky-pin guard refuses a cross-agent move, which is the specific damage
// (issue #182). Recorded here rather than left to be rediscovered as an omission.
var stateChangingToolNames = []string{
	// Filesystem writes.
	"write_file",
	"edit_file",
	"delete_file",
	"rename_file",
	"copy_file",
	"transaction_apply",
	"find_replace",
	"undo_edit",
	// Semantic (symbol) writes.
	"rename_symbol",
	"replace_symbol_body",
	"insert_before_symbol",
	"insert_after_symbol",
	"safe_delete_symbol",
	"move_symbol",
	// Repository.
	"git",
	"git_init",
	// Arbitrary execution: whatever the command or task touches.
	"run_command",
	"run_task",
	"mutation_test",
	// Workspace memory.
	"write_memory",
	"delete_memory",
	"share_findings",
	// Session registry: a rename moves the address peers deliver mail to.
	"rename_session",
	// Configuration.
	"agent_config",
	// Collab store. check_messages mutates because delivery is exactly-once:
	// an unattributable poll consumes a message addressed to somebody else,
	// and the observed incident included an approval acted on by the wrong
	// agent four minutes later.
	"share_intent",
	"leave_note",
	"check_messages",
}

// StateChangingToolNames returns a copy of the tools the shared-connection gate
// refuses for an unattributable caller.
func StateChangingToolNames() []string {
	return append([]string(nil), stateChangingToolNames...)
}
