package cli

// conn_clear_markers.go — the daemon's memory of conversations Claude Code said
// it had just started with /clear (#564).
//
// Claude Code's /clear keeps the `plumb serve` connection and starts a NEW
// conversation id. Without more, that newcomer is indistinguishable from a second
// concurrent conversation on a shared connection (Claude desktop's Code tab), and
// the two want opposite answers: a successor to the linked conversation should
// inherit the connection's identity, name and mail, while a second conversation
// must leave them alone (conn_link_external.go).
//
// The one thing that tells them apart is the client saying so. Claude Code fires
// its SessionStart hook with `source: "clear"` for exactly this event, and the hook
// forwards the NEW conversation id over the control socket (hooks_claude_clear.go).
// A marker is that announcement, held until the conversation's first call arrives.
//
// A marker is deliberately weak, and every limit below is load-bearing:
//
//   - It is keyed by the NEW conversation id and consumed by the first stamped call
//     of that id the daemon sees. One-shot: a second use needs a second /clear.
//   - It decides nothing about any other connection. The only thing it can do is
//     let the connection that receives the conversation's first call hand its OWN
//     linked identity to that conversation (conn_clear_handover.go). It names no
//     connection and no predecessor, so there is nothing for it to move elsewhere.
//   - It expires (clearMarkerTTL) and is bounded (clearMarkerCap), so a hook that
//     fires for a conversation which never reaches plumb leaves nothing behind.
//   - It lives in memory only. A daemon restart forgets it, and the conversation is
//     then treated as the newcomer it cannot be shown not to be.
//
// The control socket is local and same-user, so the authority behind a marker is
// the user's own (threat-model A6): the same user can already run any plumb tool
// as any agent.

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// clearMarkerTTL is how long a marker waits for its conversation's first call.
	// The hook runs before the model sees anything, so the call normally follows
	// within seconds; ten minutes covers a long first turn and nothing more.
	clearMarkerTTL = 10 * time.Minute
	// clearMarkerCap bounds the table. A full table evicts the marker closest to
	// expiry, so a flood cannot grow memory and a fresh /clear always lands.
	clearMarkerCap = 256
	// clearMarkerMaxIDLen bounds one id. Claude Code's are 36 bytes.
	clearMarkerMaxIDLen = 128
)

// clearMarkers is the set of pending /clear announcements, safe for concurrent
// use. The zero value is not usable; build one with newClearMarkers. A nil
// *clearMarkers is a valid empty set that records nothing, which is what a test
// connection built without a registry has.
type clearMarkers struct {
	mu      sync.Mutex
	expires map[string]time.Time // conversation id -> when the marker lapses
	// pendingN mirrors len(expires) so the per-call check costs one atomic load
	// while no /clear is outstanding, which is nearly always.
	pendingN atomic.Int64
	now      func() time.Time // a seam for the expiry tests
}

func newClearMarkers() *clearMarkers {
	return &clearMarkers{expires: make(map[string]time.Time), now: time.Now}
}

// validClearID reports whether id may be marked: a plain conversation id, which is
// non-empty, bounded, single-token, and not a `<conversation>/<agent>` stamp. A
// subagent never starts a conversation, so a marker for one is not a /clear.
func validClearID(id string) bool {
	return id != "" && len(id) <= clearMarkerMaxIDLen &&
		!strings.ContainsFunc(id, func(r rune) bool { return r <= ' ' || r == '/' })
}

// mark records that the conversation id was started by /clear, replacing any
// earlier marker for it. An id that could not be a conversation is ignored.
func (c *clearMarkers) mark(id string) {
	if c == nil {
		return
	}
	id = strings.TrimSpace(id)
	if !validClearID(id) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, exp := range c.expires {
		if !now.Before(exp) {
			delete(c.expires, k)
		}
	}
	if _, replacing := c.expires[id]; !replacing && len(c.expires) >= clearMarkerCap {
		c.evictOldestLocked()
	}
	c.expires[id] = now.Add(clearMarkerTTL)
	c.pendingN.Store(int64(len(c.expires)))
}

// evictOldestLocked drops the marker closest to expiry. Caller holds c.mu.
func (c *clearMarkers) evictOldestLocked() {
	var oldest string
	var oldestAt time.Time
	for k, exp := range c.expires {
		if oldest == "" || exp.Before(oldestAt) {
			oldest, oldestAt = k, exp
		}
	}
	delete(c.expires, oldest)
}

// pending reports whether any marker is outstanding. It is the lock-free gate in
// front of take, so a daemon with no /clear in flight pays one atomic load per call.
func (c *clearMarkers) pending() bool {
	return c != nil && c.pendingN.Load() > 0
}

// take consumes the marker for id and reports whether a live one existed. It
// consumes an expired marker too, and says no. Whatever the answer, a second take
// for the same id finds nothing.
func (c *clearMarkers) take(id string) bool {
	if !c.pending() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.expires[id]
	if !ok {
		return false
	}
	delete(c.expires, id)
	c.pendingN.Store(int64(len(c.expires)))
	return c.now().Before(exp)
}

// size is the number of markers held, expired ones included. For tests.
func (c *clearMarkers) size() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.expires)
}

// ctrlConversationClearedCommand is the control-socket command the Claude Code
// SessionStart hook sends when a conversation began with /clear:
// `conversation-cleared <new session id>` (hooks_claude_clear.go).
const ctrlConversationClearedCommand = "conversation-cleared"

// handleClearCommand answers ctrlConversationClearedCommand, reporting whether line
// was one so handleCtrlConn can stop. The reply is a single line, "ok" when the
// marker was taken (an id that cannot be a conversation is dropped quietly: the
// hook is advisory and has nothing to do with the answer) or an "error: …" line
// when the daemon has no marker table.
func handleClearCommand(conn net.Conn, line string, h ctrlHandlers) bool {
	id, ok := strings.CutPrefix(line, ctrlConversationClearedCommand+" ")
	if !ok {
		return false
	}
	if h.conversationCleared == nil {
		fmt.Fprint(conn, "error: conversation markers unavailable\n")
		return true
	}
	id = strings.TrimSpace(id)
	h.conversationCleared(id)
	slog.Debug("daemon: conversation started by /clear announced over the control socket", "conversation", logicalAgentLabel(id))
	fmt.Fprint(conn, "ok\n")
	return true
}
