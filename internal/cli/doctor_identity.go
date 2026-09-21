package cli

// doctor_identity.go — the operator-facing half of the shared-connection
// disclosure (PLAN-440).
//
// session_start tells the AGENT, at orientation, when its per-call identity
// channel is dead. That is the right audience for the agent's own next call,
// and the wrong one for the person debugging afterwards: by then the agent
// transcript may be gone, and the symptom reaching the operator is "the agents
// stopped being able to write". doctor answers that question from outside any
// connection, off the session records the daemon already keeps.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/session"
)

// sharedConnectionHealth is the Health marker markSharedConnectionDetected
// writes. Named here so the writer and this reader cannot drift apart on a
// string literal.
const sharedConnectionHealth = "shared_connection_detected"

// checkAgentIdentity reports whether any live session is on a connection that
// is refusing unattributable state-changing calls.
func checkAgentIdentity() []checkResult {
	sessions, err := session.List()
	if err != nil {
		return []checkResult{{
			name:   "Shared connections",
			ok:     true,
			warn:   true,
			detail: fmt.Sprintf("could not read the session records: %v", err),
			fix:    "check the session directory is readable; `plumb sessions` reads the same files",
		}}
	}
	return []checkResult{sharedConnectionCheck(sessions)}
}

// sharedConnectionCheck is the pure half: it decides what to say about a set of
// live sessions, so the decision is testable without a daemon.
//
// A WARNING, never a failure. A shared connection is a supported topology whose
// guard is doing its job, not a broken installation — exiting non-zero would
// make doctor red on a machine with nothing wrong with it. What the operator
// needs is the names, so the affected sessions can be found, and a remedy that
// does not depend on the client honouring anything.
func sharedConnectionCheck(sessions []session.Info) checkResult {
	var affected []string
	for _, s := range sessions {
		if s.Health == sharedConnectionHealth {
			affected = append(affected, s.Name)
		}
	}
	if len(affected) == 0 {
		return checkResult{
			name:   "Shared connections",
			ok:     true,
			detail: "no connection is multiplexing unidentified agents",
		}
	}
	sort.Strings(affected)
	return checkResult{
		name: "Shared connections",
		ok:   true,
		warn: true,
		detail: fmt.Sprintf("%d session(s) share a connection with another agent and refuse state-changing "+
			"calls that carry no per-call identity: %s", len(affected), strings.Join(affected, ", ")),
		// The hook is the cheap remedy where the client honours it, and naming
		// it alone is the misdirection the refusal text already gives: it can be
		// installed, matched and emitting correctly while the client drops the
		// rewrite, which is what `local-agent-mode-plumb` does. So the transport
		// remedy is named first and unconditionally.
		fix: "these connections have carried a per-call identity before, so the channel works on them: " +
			"stamp every call. On Claude Code, `plumb hooks install claude-code`; otherwise a per-call " +
			"_meta identity, or one plumb serve per logical agent. A client that can never stamp is not " +
			"refused at all, so it will not appear here",
	}
}
