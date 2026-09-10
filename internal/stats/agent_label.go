package stats

import "strings"

// AgentLabel renders who made a recorded call, for the recent-writes feed,
// the TUI history and `plumb stats`: the session name alone when the call
// carried no logical-agent id, and the session name qualified by the agent
// when it did. Nothing is stored for this; the label is derived from the id's
// shape on every render, so a change of wording never needs a migration.
//
// Shapes. A Claude Code subagent is stamped `<conversation>/<agent>`, and
// renders as `<name>/agent-<first eight of agent>`. A plain id that equals
// the session's own external id is the main thread — the session itself — so
// it renders as the bare name rather than repeating the conversation id on
// every row. Any other plain id (a `_meta`-stamping client's per-agent ids,
// which are distinct strings with no slash) renders as `<name>/<first eight>`,
// so four agents on one connection are four labels, which is the whole point
// (PLAN-401). A blank id is a blank claim: the bare name, never a guess.
func AgentLabel(sessionName, sessionExternalID, logicalAgent string) string {
	if logicalAgent == "" {
		return sessionName
	}
	if i := strings.IndexByte(logicalAgent, '/'); i >= 0 {
		return sessionName + "/agent-" + shortAgentID(logicalAgent[i+1:])
	}
	if sessionExternalID != "" && logicalAgent == sessionExternalID {
		return sessionName
	}
	return sessionName + "/" + shortAgentID(logicalAgent)
}

func shortAgentID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
