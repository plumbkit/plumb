package tools

import (
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// workspace_sessions_who.go — the "who" column of the recent-writes feed
// (PLAN-401). A row records the session that wrote AND the logical-agent id
// the call carried; on a shared connection several agents write through one
// session, and naming only the session made four agents look like one.

// writerLabels renders the who column — stats.AgentLabel over the row's
// session name and logical-agent id — memoising each session's external id,
// which the label needs to tell a main thread's own id from a peer agent's
// and which costs a session-file read per lookup.
type writerLabels struct {
	extIDs map[string]string
}

func newWriterLabels() *writerLabels { return &writerLabels{extIDs: map[string]string{}} }

func (l *writerLabels) label(w stats.RecentCall) string {
	if w.LogicalAgent == "" {
		return w.SessionName
	}
	ext, ok := l.extIDs[w.SessionID]
	if !ok {
		ext = session.ExternalIDOf(w.SessionID)
		l.extIDs[w.SessionID] = ext
	}
	return stats.AgentLabel(w.SessionName, ext, w.LogicalAgent)
}
