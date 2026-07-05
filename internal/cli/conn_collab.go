package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/tools"
)

// conn_collab.go — the connection-side wiring for the phase-2 cross-agent
// sharing tier ([collab] intents/mailbox): the collab.db store accessors (one
// that creates, one that never does), the resolved policy snapshot handed to the
// write tools, the intent-aware write hint, and session-close intent cleanup.
// Everything is advisory (never blocks a write), byte-budgeted, and strictly
// per-workspace.

// collabStoreCreate returns the workspace's collab store, CREATING collab.db on
// first use. Only the intents/mailbox write tools use it — a workspace whose
// flags stay off never gets a collab.db. Nil when no workspace is attached.
func (s *connSession) collabStoreCreate() *collab.Store {
	if s.collabPool == nil {
		return nil
	}
	ws := s.workspace()
	if ws == "" {
		return nil
	}
	return s.collabPool.acquire(ws)
}

// collabStoreIfExists returns the workspace's collab store ONLY when collab.db
// already exists on disk (never creating one), so read/hint/close paths cannot
// materialise a database for a workspace that never used the feature.
func (s *connSession) collabStoreIfExists() *collab.Store {
	if s.collabPool == nil {
		return nil
	}
	ws := s.workspace()
	if ws == "" {
		return nil
	}
	return s.collabPool.get(ws)
}

// collabPolicy resolves the connection's [collab] intents/mailbox snapshot for
// the write tools, off the lock-free view (no per-call config read).
func (s *connSession) collabPolicy() tools.CollabPolicy {
	c := s.collabConfig()
	return tools.CollabPolicy{
		Intents:          c.Intents,
		Mailbox:          c.Mailbox,
		KnowledgeHandoff: c.KnowledgeHandoff,
		IntentTTLMinutes: c.IntentTTLMinutes,
	}
}

// intentHintTimeout bounds the collab.db read on the hot enrich path so a slow
// disk never stalls a tool response for the sake of an advisory hint.
const intentHintTimeout = 200 * time.Millisecond

// intentHint returns a bounded "[Peer intent (claim, unverified): …]" block when
// another live session has declared an intent whose path globs match the tool's
// target file. Gated on [collab] intents (read from the per-connection snapshot).
// Advisory only — it never blocks the write. Labelled as an unverified CLAIM,
// distinct from the phase-1 peer-activity hint's observed fact.
func (s *connSession) intentHint(args []byte, ws string) string {
	ccfg := s.collabConfig()
	if !ccfg.Intents {
		return ""
	}
	rel := hintRelPath(ws, args)
	if rel == "" {
		return ""
	}
	store := s.collabStoreIfExists()
	if store == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(s.ctx, intentHintTimeout)
	defer cancel()
	now := time.Now()
	intents, err := store.LiveIntents(ctx, now)
	if err != nil {
		return ""
	}
	for _, r := range intents {
		if r.AuthorID == s.sessID { // never hint a session about its own intent
			continue
		}
		if !collab.MatchPath(r.PathGlobs, rel) {
			continue
		}
		block := fmt.Sprintf(
			"\n\n[Peer intent (claim, unverified): session %s declared %s ago: %q. This is advisory.]",
			r.AuthorSession, humaniseSince(now.Sub(r.CreatedAt)), r.Body)
		return clampBytes(block, ccfg.HintBudgetBytes)
	}
	return ""
}

// clearSessionIntents removes this session's intents when its connection closes —
// an intent must not outlive its session. Notes are left in place (they survive
// their author). Uses the open-if-exists accessor so close never creates a
// collab.db; best-effort and time-bounded.
func (s *connSession) clearSessionIntents() {
	store := s.collabStoreIfExists()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.ClearSessionIntents(ctx, s.sessID); err != nil {
		s.log().Debug("collab: clear session intents on close", "err", err)
	}
}
