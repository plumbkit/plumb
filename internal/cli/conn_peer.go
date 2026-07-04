package cli

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/tools"
)

// conn_peer.go — the connection-side tier-1 cross-agent peer-awareness signal
// ([collab] peer_awareness): the peer-activity hint injected into a path-bearing
// tool response when another currently-active session recently wrote the same
// file. It is advisory only (it never blocks a write), byte-budgeted, and
// strictly per-workspace. The recent-writes feed and the topology-annotated
// digest are surfaced elsewhere (workspace_sessions and session_start); this file
// owns only the hot-path hint.

// peerHintMaxWindow bounds the peer-activity recency window regardless of the
// configured idle threshold: a write older than this never triggers a hint. The
// effective window is min(idle threshold, this).
const peerHintMaxWindow = 30 * time.Minute

// peerWriteCacheTTL is how long a resolved peer-write snapshot is reused before a
// refresh. Short, because a peer's writes change while both agents work; long
// enough that a burst of read_file calls does not re-scan the session directory
// and stats DB on every call.
const peerWriteCacheTTL = 5 * time.Second

// peerWrite records the most recent write to a file by a currently-active peer.
type peerWrite struct {
	session string
	at      time.Time
}

// peerWriteCache caches, per workspace, the most recent write to each file by a
// currently-active peer session. It is refreshed lazily off the session
// directory (active peers) and the stats DB (their writes) so the hot enrich
// path costs a map lookup, mirroring memoryHintCache.
//
// Concurrency: safe for concurrent use.
type peerWriteCache struct {
	mu      sync.Mutex
	ws      string
	selfID  string
	builtAt time.Time
	// byAbsPath maps an absolute file path to the most recent peer write.
	byAbsPath map[string]peerWrite
}

// lookup returns the most recent peer write to absPath within window, refreshing
// the snapshot when stale. ok is false when no active peer wrote the file, the
// write is older than the window, or the caller is the writer.
func (c *peerWriteCache) lookup(ws, selfID, absPath string, now time.Time, window time.Duration) (peerWrite, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ws != ws || c.selfID != selfID || now.Sub(c.builtAt) >= peerWriteCacheTTL {
		c.refresh(ws, selfID, now)
	}
	pw, ok := c.byAbsPath[absPath]
	if !ok || now.Sub(pw.at) > window {
		return peerWrite{}, false
	}
	return pw, true
}

// refresh rebuilds the snapshot: the set of currently-active peer sessions on ws
// (excluding self), intersected with their recent writes from the stats DB. Only
// a write by a session still connected right now counts — a disconnected peer's
// stale write must not hint. Must be called with c.mu held.
func (c *peerWriteCache) refresh(ws, selfID string, now time.Time) {
	c.ws, c.selfID, c.builtAt, c.byAbsPath = ws, selfID, now, make(map[string]peerWrite)

	activePeers := activePeerNames(ws, selfID)
	if len(activePeers) == 0 {
		return
	}
	db, err := stats.SharedReadOnly()
	if err != nil || db == nil {
		return
	}
	writes, err := db.RecentWritesByWorkspace(ws, tools.WriteToolNames(), 50)
	if err != nil {
		return
	}
	for _, w := range writes {
		name, ok := activePeers[w.SessionID]
		if !ok {
			continue // not an active peer (self, or a disconnected session)
		}
		file := tools.FileFromToolInput(w.InputJSON)
		if file == "" {
			continue
		}
		abs := file
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(ws, abs)
		}
		// Writes arrive newest-first, so keep the first seen per file.
		if _, seen := c.byAbsPath[abs]; !seen {
			c.byAbsPath[abs] = peerWrite{session: name, at: w.CalledAt}
		}
	}
}

// activePeerNames returns the currently-active peer sessions on ws (folder
// match, not ended, excluding self), keyed by session ID → display name.
func activePeerNames(ws, selfID string) map[string]string {
	all, err := session.List()
	if err != nil {
		return nil
	}
	out := make(map[string]string)
	for _, p := range all {
		if p.ID == selfID {
			continue
		}
		if filepath.Clean(p.Folder) != filepath.Clean(ws) {
			continue
		}
		out[p.ID] = p.Name
	}
	return out
}

// peerRecencyWindow is min(idle threshold, peerHintMaxWindow): how recently a
// peer must have written a file for the hint to fire. A non-positive configured
// idle threshold falls back to the shipped 30-minute default.
func (s *connSession) peerRecencyWindow() time.Duration {
	idle := time.Duration(s.view().session.IdleThresholdMinutes) * time.Minute
	if idle <= 0 {
		idle = session.IdleSessionThreshold
	}
	if idle < peerHintMaxWindow {
		return idle
	}
	return peerHintMaxWindow
}

// peerHint returns a bounded "[Peer: …]" block when another currently-active
// session recently wrote the tool's target file, or "" otherwise. Gated on
// [collab] peer_awareness (read from the per-connection snapshot, never per-call
// config disk I/O). Advisory only — it never blocks the write.
func (s *connSession) peerHint(args []byte, ws string) string {
	ccfg := s.collabConfig()
	if !ccfg.PeerAwareness || s.peerWrites == nil {
		return ""
	}
	rel := hintRelPath(ws, args)
	if rel == "" {
		return ""
	}
	abs := filepath.Join(ws, rel)
	now := time.Now()
	pw, ok := s.peerWrites.lookup(ws, s.sessID, abs, now, s.peerRecencyWindow())
	if !ok {
		return ""
	}
	block := fmt.Sprintf(
		"\n\n[Peer: session %s edited this file %s ago — consider file_status before editing.]",
		pw.session, humaniseSince(now.Sub(pw.at)),
	)
	return clampBytes(block, ccfg.HintBudgetBytes)
}

// humaniseSince renders a positive duration as a compact age ("just now",
// "3 min", "2 h", "1 day"). Kept local to the peer-hint block wording; reuses
// the package-level plural helper.
func humaniseSince(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n := int(d.Minutes())
		return fmt.Sprintf("%d min%s", n, plural(n))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		n := int(d.Hours() / 24)
		return fmt.Sprintf("%d day%s", n, plural(n))
	}
}
