package stats

import (
	"strings"
	"unicode"

	"github.com/plumbkit/plumb/internal/textfmt"
)

// AgentLabel renders who made a recorded call, for the recent-writes feed, the
// TUI history and `plumb stats`: the session name alone when the row carries
// no logical-agent id, and the session name qualified by the agent when it
// does. Nothing is stored for the label itself, so changing this wording never
// needs a migration.
//
// The row carries an id only when the connection was SHARED when the call was
// recorded (see the recorder). That is what makes the bare name honest rather
// than ambiguous: a row with no id was made when the connection had exactly
// one agent, so the session name names it exactly. It also means the label is
// stable — it depends on nothing that can change after the row is written,
// which an "is this the main thread?" test against the session's external id
// could not promise, since that id outlives the session by a different rule
// than the row does.
//
// A Claude Code subagent is stamped `<conversation>/<agent>`, so only the
// agent half distinguishes it from its parent and only that half is rendered.
// A `_meta`-stamping client's ids are distinct strings with no slash, and
// render whole (truncated). Four agents on one connection are four labels,
// which is the point (PLAN-401).
//
// The id is a client-supplied string. It is sanitised here as well as capped
// at record time, because this output lands in a peer-facing attribution feed
// where a newline would forge a row, and truncation is by RUNE so a multi-byte
// id cannot be cut into invalid UTF-8.
func AgentLabel(sessionName, logicalAgent string) string {
	agent := agentDisplayID(logicalAgent)
	if agent == "" {
		return sessionName
	}
	return sessionName + "/" + agent
}

// agentDisplayID is the part of a logical-agent id worth showing: the agent
// half of a `<conversation>/<agent>` stamp, or the whole of a plain id,
// sanitised and shortened to eight runes.
func agentDisplayID(logicalAgent string) string {
	if i := strings.LastIndexByte(logicalAgent, '/'); i >= 0 {
		logicalAgent = logicalAgent[i+1:]
	}
	return textfmt.Ellipsis(sanitiseAgentID(logicalAgent), agentIDDisplayRunes)
}

// agentIDDisplayRunes is how much of an agent id a label shows. Eight runes of
// a random id distinguish concurrent agents without swallowing the column.
const agentIDDisplayRunes = 8

// AgentIDStorageBytes caps what the recorder stores. The id arrives from the
// client with no length or character validation of its own, and every row is
// one insert: an uncapped id is unbounded write amplification, the same reason
// input_json and output_text are capped.
const AgentIDStorageBytes = 128

// SanitiseAgentID is the record-time guard: control characters (a newline
// above all, which would forge a second row in the recent-writes feed) are
// dropped, and the result is truncated on a rune boundary.
func SanitiseAgentID(s string) string {
	return textfmt.TruncateBytes(sanitiseAgentID(s), AgentIDStorageBytes)
}

func sanitiseAgentID(s string) string {
	return strings.Map(func(r rune) rune {
		if r == utf8Replacement || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// utf8Replacement is what a decoder leaves behind for an invalid byte; an id
// carrying one is already damaged and it must not reach a rendered column.
const utf8Replacement = '�'
