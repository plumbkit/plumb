package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/textfmt"
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
		CrossProject:     c.CrossProject,
		ChatBudgetBytes:  c.ChatBudgetBytes,
		MaxWaitSeconds:   c.MaxWaitSeconds,
	}
}

// collabGlobalCreate returns the daemon-level cross-project store, CREATING it
// on first use. Only the send path calls it, and only for a message already
// known to cross a project boundary.
func (s *connSession) collabGlobalCreate() *collab.Store {
	if s.collabPool == nil {
		return nil
	}
	return s.collabPool.acquireGlobal()
}

// collabGlobalIfExists returns the daemon-level cross-project store only when it
// already exists, so delivery never creates it.
func (s *connSession) collabGlobalIfExists() *collab.Store {
	if s.collabPool == nil {
		return nil
	}
	return s.collabPool.getGlobal()
}

// peerWorkspace reports which workspace a named peer session is pinned to. It
// decides whether a message stays in this workspace's collab.db or crosses into
// the daemon-level store. A name that matches no live session is reported as not
// found, and the caller then treats the message as same-project — addressing a
// peer that has not connected yet is a legitimate thing to do.
func (s *connSession) peerWorkspace(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	all, err := session.List()
	if err != nil {
		return "", false
	}
	for _, p := range all {
		if p.Name == name {
			return p.Folder, true
		}
	}
	return "", false
}

// peerSessionByID resolves stable identity to the live session's current name
// and workspace. Unlike peerWorkspace, it is safe across rename and name reuse.
func (s *connSession) peerSessionByID(id string) (string, string, bool) {
	if id == "" {
		return "", "", false
	}
	all, err := session.List()
	if err != nil {
		return "", "", false
	}
	for _, p := range all {
		if p.ID == id {
			return p.Name, p.Folder, true
		}
	}
	return "", "", false
}

// collabDeps bundles everything the mailbox tools need. Note the deliberate
// asymmetry between the create and if-exists accessors: only a send may bring a
// database into existence, so every read path is wired to the if-exists variant.
func (s *connSession) collabDeps() tools.CollabDeps {
	return tools.CollabDeps{
		Workspace:           s.workspace,
		SessionName:         s.sessionName,
		SessionID:           s.sessID,
		Policy:              s.collabPolicy,
		Store:               s.collabStoreCreate,
		StoreIfExists:       s.collabStoreIfExists,
		GlobalStore:         s.collabGlobalCreate,
		GlobalStoreIfExists: s.collabGlobalIfExists,
		Notifier:            s.collabPool.notifier(),
		PeerWorkspace:       s.peerWorkspace,
		PeerSessionByID:     s.peerSessionByID,
	}
}

// inbox is this session's message inbox: its address, its resolved policy, and
// the two stores it may read from. Built per call so a config hot-reload takes
// effect on the next delivery.
//
// An unregistered session (session.Register failed, so sessID is empty) has no
// address. Its name never entered the session directory, so no peer's
// uniqueness check can see it and it may well duplicate a live session's name —
// claiming that peer's messages, which are claimed at most once and would then
// simply never reach the intended recipient. Inbox.Claim treats an empty Self as "mailbox off".
func (s *connSession) inbox() tools.Inbox {
	return tools.Inbox{
		Self:      s.addressableName(),
		SelfID:    s.sessID,
		Root:      s.workspace(),
		Policy:    s.collabPolicy(),
		Workspace: s.collabStoreIfExists,
		Global:    s.collabGlobalIfExists,
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
		return textfmt.ClampBytes(block, ccfg.HintBudgetBytes)
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
