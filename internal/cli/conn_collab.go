package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
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
		MaxExchanges:     c.MaxExchanges,
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

// daemonWideConversationsFetchLimit is how many daemon-wide conversations a
// dashboard may ask for. It is a small, fixed cap rather than a caller-chosen
// one: this is a display surface (TUI/web dashboards), not a paginated
// listing, and every candidate costs one extra ConversationWorkspaces query.
const daemonWideConversationsFetchLimit = 8

// targetAllowsCrossProject reports whether the named workspace — which is NOT
// necessarily this connection's own pinned workspace — has opted in to
// [collab] cross_project, with no live connection to that project required.
// It serves two callers with the same question:
//
//   - leave_note's RECIPIENT consent check (via collabDeps), which refuses a
//     cross-project send the addressee would never read;
//   - the daemon-wide dashboards (see daemonWideConversations), which have no
//     single recipient to ask and so must resolve the opt-in per participating
//     workspace rather than off the cached per-connection config snapshot.
//
// Thin wrapper: config.TargetAllowsCrossProject does the actual LoadProject +
// `plumb trust` resolution (an untrusted project's own config.toml cannot
// grant itself the channel), and fails closed on every shape of "cannot
// determine consent" — an unresolvable workspace, or a config that will not
// load. Consent must be affirmatively readable; the alternative is a send
// accepted and reported successful while the recipient silently never reads
// it, or a conversation displayed on the strength of participants nobody
// could place.
func (s *connSession) targetAllowsCrossProject(workspace string) bool {
	return config.TargetAllowsCrossProject(s.store.Current(), workspace)
}

// daemonWideConversations returns live conversations from the daemon-level
// store, filtered to those where EVERY participating workspace has opted in
// to [collab] cross_project — the daemon dashboards have no single recipient
// to ask consent of, so consent must be unanimous among everyone in the
// thread (see collab.(*Store).FilterDaemonWideConversations for the "any one"
// vs "none at all" reasoning). Read-only: never creates the global store, and
// answers nothing when it does not already exist.
func (s *connSession) daemonWideConversations(ctx context.Context) ([]collab.ConversationSummary, error) {
	g := s.collabGlobalIfExists()
	if g == nil {
		return nil, nil
	}
	return g.FilterDaemonWideConversations(ctx, time.Now(), daemonWideConversationsFetchLimit, s.targetAllowsCrossProject)
}

// resolvePeer reports the live session answering to a name: the workspace it is
// pinned to (which decides whether a message stays in this workspace's collab.db
// or crosses into the daemon-level store) and its stable session ID (which binds
// the message to that session alone).
//
// A name that matches no live session is reported as not found, and the caller
// then treats the message as same-project, unbound — addressing a peer that has
// not connected yet is a legitimate thing to do. So is a name matching SEVERAL
// live sessions: Register and Rename refuse a name a live session already holds,
// so it should not happen, but binding to a guess when it does would deliver the
// message to the wrong agent while telling the sender it reached the right one.
// Matching is exact, as delivery is; the uniqueness check is case-insensitive
// and so can only be stricter.
func (s *connSession) resolvePeer(name string) (tools.PeerSession, bool) {
	if name == "" {
		return tools.PeerSession{}, false
	}
	all, err := session.List()
	if err != nil {
		return tools.PeerSession{}, false
	}
	var found tools.PeerSession
	matches := 0
	for _, p := range all {
		if p.Name == name {
			found = tools.PeerSession{Workspace: p.Folder, ID: p.ID}
			matches++
		}
	}
	if matches != 1 {
		return tools.PeerSession{}, false
	}
	return found, true
}

// collabDeps bundles everything the mailbox tools need. Note the deliberate
// asymmetry between the create and if-exists accessors: only a send may bring a
// database into existence, so every read path is wired to the if-exists variant.
func (s *connSession) collabDeps() tools.CollabDeps {
	return tools.CollabDeps{
		Workspace:                s.workspace,
		SessionName:              s.sessionName,
		SessionID:                s.sessionID,
		Policy:                   s.collabPolicy,
		Store:                    s.collabStoreCreate,
		StoreIfExists:            s.collabStoreIfExists,
		GlobalStore:              s.collabGlobalCreate,
		GlobalStoreIfExists:      s.collabGlobalIfExists,
		Notifier:                 s.collabPool.notifier(),
		ResolvePeer:              s.resolvePeer,
		InheritedSessionIDs:      s.inheritedSessionIDs,
		TargetAllowsCrossProject: s.targetAllowsCrossProject,
	}
}

// inbox is this session's message inbox: its address, its resolved policy, and
// the two stores it may read from. Built per call so a config hot-reload takes
// effect on the next delivery.
//
// An unregistered session (session.Register failed, so sessID is empty) has no
// address. Its name never entered the session directory, so no peer's
// uniqueness check can see it and it may well duplicate a live session's name —
// claiming that peer's messages, which are delivered exactly once and would
// simply never arrive. Inbox.Claim treats an empty Self as "mailbox off".
func (s *connSession) inbox() tools.Inbox {
	return tools.Inbox{
		Self:         s.addressableName(),
		SelfID:       s.sessID,
		InheritedIDs: s.inheritedSessionIDs(),
		Root:         s.workspace(),
		Policy:       s.collabPolicy(),
		Workspace:    s.collabStoreIfExists,
		Global:       s.collabGlobalIfExists,
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
