package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/topology"
)

// session_start_peers.go builds the tier-1 peer digest ([collab] peer_awareness):
// when other agents are active on the workspace at attach time, session_start
// gains a short block naming them and the areas (directories/packages) they have
// recently touched. Everything here is a verifiable observation — derived from
// writes the daemon itself recorded, never from an agent's stated intent.

// peerDigestWriteLimit caps how many recent writes are scanned to build the
// digest, and peerDigestAreas caps the areas shown per peer.
const (
	peerDigestWriteLimit = 50
	peerDigestAreas      = 3
)

// writeSessionPeers appends the "Active peers" digest when [collab] peer_awareness
// is on and at least one other session is active on this workspace. Bounded by
// the shared [collab] hint_budget_bytes (a peer digest is a peer signal like the
// injected hint), enforced on a UTF-8 boundary.
func (t *SessionStart) writeSessionPeers(sb *strings.Builder, ws string) {
	if t.collabFn == nil {
		return
	}
	enabled, budget := t.collabFn()
	if !enabled {
		return
	}
	peers := t.activePeers(ws)
	if len(peers) == 0 {
		return
	}
	block := t.formatPeerDigest(ws, peers)
	if block == "" {
		return
	}
	sb.WriteString(clampToBytes(block, budget))
}

// activePeers returns the sessions active on ws right now, excluding this one.
func (t *SessionStart) activePeers(ws string) []session.Info {
	all, err := session.List()
	if err != nil {
		return nil
	}
	var peers []session.Info
	for _, p := range all {
		if p.ID == t.selfSessID {
			continue
		}
		if filepath.Clean(p.Folder) == filepath.Clean(ws) {
			peers = append(peers, p)
		}
	}
	return peers
}

// formatPeerDigest renders the digest: one line per active peer, listing the
// distinct areas (workspace-relative directories, topology-annotated where the
// index has the file) that peer recently wrote. Pure aside from the read-only
// stats query + topology lookups it drives.
func (t *SessionStart) formatPeerDigest(ws string, peers []session.Info) string {
	areasBySession := t.peerAreas(ws)
	now := time.Now()

	var sb strings.Builder
	sb.WriteString("\n## Active peers\n\n")
	fmt.Fprintf(&sb, "%d other session(s) active on this workspace right now — the areas below are "+
		"observed writes (facts), not stated intentions:\n", len(peers))
	for _, p := range peers {
		fmt.Fprintf(&sb, "- %s", p.Name)
		if p.ClientName != "" {
			fmt.Fprintf(&sb, " [%s]", p.ClientName)
		}
		if !p.LastSeenAt.IsZero() {
			fmt.Fprintf(&sb, " — last seen %s ago", humaniseAge(now.Sub(p.LastSeenAt)))
		}
		if areas := areasBySession[p.ID]; len(areas) > 0 {
			fmt.Fprintf(&sb, "; recently touched %s", strings.Join(areas, ", "))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// peerAreas returns, per session ID, the distinct areas that session recently
// wrote — a workspace-relative directory optionally annotated with its topology
// package/symbol. Bounded by its own deadline so a slow index never stalls
// session_start.
func (t *SessionStart) peerAreas(ws string) map[string][]string {
	db, err := stats.SharedReadOnly()
	if err != nil || db == nil {
		return nil
	}
	writes, err := db.RecentWritesByWorkspace(ws, writeToolNames, peerDigestWriteLimit)
	if err != nil || len(writes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsSessionsTimeout)
	defer cancel()
	store := t.topoStore()

	out := make(map[string][]string)
	seen := make(map[string]bool) // sessionID+area dedupe
	for _, w := range writes {
		file := fileFromInputJSON(w.InputJSON)
		if file == "" {
			continue
		}
		abs := file
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(ws, abs)
		}
		area := peerArea(ctx, ws, abs, store)
		if area == "" {
			continue
		}
		key := w.SessionID + "\x00" + area
		if seen[key] {
			continue
		}
		if len(out[w.SessionID]) >= peerDigestAreas {
			continue
		}
		seen[key] = true
		out[w.SessionID] = append(out[w.SessionID], area)
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

// topoStore returns the live topology store, or nil when unwired/disabled.
func (t *SessionStart) topoStore() *topology.Store {
	if t.topo == nil {
		return nil
	}
	return t.topo()
}

// peerArea renders the display area for a written file: its workspace-relative
// directory, with a topology package annotation appended when the index has the
// file (e.g. "internal/tools/ (package tools)").
func peerArea(ctx context.Context, ws, absPath string, store *topology.Store) string {
	rel, err := filepath.Rel(ws, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	if dir == "." {
		dir = "(root)"
	} else {
		dir += "/"
	}
	if annot := fileTopologyAnnotation(ctx, store, absPath); annot != "" {
		return fmt.Sprintf("%s (%s)", dir, annot)
	}
	return dir
}

// clampToBytes truncates s to at most budget bytes on a UTF-8 rune boundary,
// appending an ellipsis when it trims. A no-op when s fits or budget <= 0. The
// [collab] budget is named *_bytes, so a multi-byte digest must be measured in
// bytes, mirroring the [memory] budget rule.
func clampToBytes(s string, budget int) string {
	if budget <= 0 || len(s) <= budget {
		return s
	}
	const ell = "…"
	if budget <= len(ell) {
		return truncateToRuneBoundary(s, budget)
	}
	return truncateToRuneBoundary(s, budget-len(ell)) + ell
}

func truncateToRuneBoundary(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
