package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestOnSessionIDStoresReplayedID(t *testing.T) {
	var s connSession
	s.onSessionID("sess-123")
	if got := s.view().replayedSessionID; got != "sess-123" {
		t.Fatalf("replayedSessionID = %q, want sess-123", got)
	}
	s.onSessionID("") // an empty id must no-op, not clear
	if got := s.view().replayedSessionID; got != "sess-123" {
		t.Fatalf("empty id cleared replayedSessionID, got %q", got)
	}
}

// TestOnSessionIDAdoptsReplayedID pins the PLAN-296 adoption half: a live
// connSession resumes the proven session ID as its own, so stats/memories/collab
// keyed on sessionID() see one continuous identity across the restart.
//
// The authority is the durable identity record under the proxy session ID — here
// written by the pre-restart connection itself — and NOT the replayed ID. That
// distinction is the point of the test: a `plumb serve` old enough to predate
// the replay channel replays nothing, and gating recovery on the replay left
// exactly those long-lived proxies unable to come back as themselves.
func TestOnSessionIDAdoptsReplayedID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	// Before the restart: a connection whose identity is persisted under proxyX.
	before := newPersistSession(t, store, ss, "proxyX")
	prevID := before.sessionID()
	if prevID == "" {
		t.Fatal("session did not register; the test would prove nothing")
	}
	before.close()

	// After the restart: a fresh connection that has not yet seen its proxy ID,
	// so its generated identity can be captured BEFORE recovery replaces it.
	// newPersistSession fires onProxySession itself, which would make the
	// "before" snapshot already be the "after" one.
	after := newConnSession(context.Background(), detectTestPool(), nil, store, nil, ss, newSharedBudgets())
	t.Cleanup(after.close)
	oldID := after.sessionID()
	if oldID == "" || oldID == prevID {
		t.Fatalf("the fresh connection registered as %q; it must start under its OWN generated "+
			"ID or this test cannot show that recovery replaced anything", oldID)
	}
	// Recovery runs off the proxy secret alone. The replayed ID is delivered
	// afterwards, as the real handshake orders the two hooks, and must change
	// nothing: it is a claim to reconcile, not the authority.
	after.onProxySession("proxyX")
	if got := after.sessionID(); got != prevID {
		t.Fatalf("sessionID() = %q after recovery, want the proven %q — recovery must resolve "+
			"the identity from the proxy-keyed record, with no replayed ID to help it", got, prevID)
	}
	after.onSessionID(prevID)
	if got := after.sessionID(); got != prevID {
		t.Fatalf("sessionID() = %q after adoption, want the persisted %q", got, prevID)
	}
	if got := after.view().replayedSessionID; got != prevID {
		t.Fatalf("replayedSessionID = %q, want %q", got, prevID)
	}
	// The adoption made the predecessor's ID this session's own, so the mailbox
	// inheritance identity is redundant and must be cleared: one ID everywhere.
	if got := after.inheritedSessionIDs(); len(got) != 0 {
		t.Fatalf("inheritedSessionIDs = %v after adoption, want none", got)
	}
	// The old session file must be ended and the adopted one live.
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, info := range live {
		ids[info.ID] = true
	}
	if !ids[prevID] {
		t.Fatal("the adopted ID is not live")
	}
	if ids[oldID] {
		t.Fatal("the pre-adoption ID is still live")
	}
}

// TestOnSessionIDRestartKeepsExternalIDAndIdentity covers the restart contract
// end to end: the same proxy session rehydrates its pin/read state, adopts the
// pre-restart plumb identity, keeps the linked external ID without another
// session_start, and records both sides' tool calls under that one ID.
func TestOnSessionIDRestartKeepsExternalIDAndIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()
	statsStore := newStatsStore()
	defer statsStore.Close()

	root := freshTempDir(t)
	mustGitDir(t, root)
	path := filepath.Join(root, "a.go")
	mtime := time.Unix(1_700_000_000, 444)
	const proxyID = "proxyX"
	const externalID = "external-agent-id"

	// Before restart: an external-ID-linked session has state and one tool call.
	before := newPersistSession(t, store, ss, proxyID)
	before.statsStore = statsStore
	before.attachWorkspace(context.Background(), "file://"+root)
	before.readTracker.Record(path, mtime, "sha-a")
	prevID := before.sessionID()
	session.SetExternalID(prevID, externalID)
	statsStore.Record(root, stats.Call{SessionID: prevID, Tool: "read_file", CalledAt: time.Now(), Success: true})
	before.close()

	// After restart: the proxy reconnects with the same secret and replays its ID.
	after := newPersistSession(t, store, ss, proxyID)
	after.statsStore = statsStore
	after.onSessionID(prevID)
	after.attachWorkspace(context.Background(), "file://"+root)
	if got := after.sessionID(); got != prevID {
		t.Fatalf("sessionID() after restart = %q, want predecessor %q", got, prevID)
	}
	if got := after.readTracker.Mtime(path); !got.Equal(mtime) {
		t.Fatalf("rehydrated mtime = %v, want %v", got, mtime)
	}
	resolved, err := resolveMailSessionFor("external-id", externalID)
	if err != nil {
		t.Fatalf("resolveMailSessionFor external ID after restart: %v", err)
	}
	if resolved.ID != prevID {
		t.Fatalf("external ID resolved session %q, want adopted %q", resolved.ID, prevID)
	}
	statsStore.Record(root, stats.Call{SessionID: after.sessionID(), Tool: "edit_file", CalledAt: time.Now(), Success: true})
	statsStore.Close()

	db, err := stats.OpenReadOnly()
	if err != nil {
		t.Fatalf("stats.OpenReadOnly: %v", err)
	}
	defer db.Close()
	calls, err := db.ToolCallsForSession(root, prevID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ToolCallsForSession: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls under adopted session = %d, want 2 from both sides of restart", len(calls))
	}
}

// TestOnSessionIDRefusesForgedReplay pins the security gate: the replayed plumb
// session ID is client-supplied and disclosed (session_start echoes it), so a
// replay that does not MATCH the ID persisted under the connection's proxy
// session ID is a claim, not proof — adoption must refuse it. The session keeps
// its fresh ID, inherits no mailbox identity, and creates no session file under
// the victim's ID.
func TestOnSessionIDRefusesForgedReplay(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	// The persisted row for proxyX names a DIFFERENT plumb session ID than the
	// one the connection is about to replay. A second live session holds the
	// persisted NAME, so the restore declines on ErrNameTaken and cannot
	// re-persist the row ahead of the refusal — that is what keeps the
	// convergence assertion below honest (and the inheritance assertion exact:
	// a declined rename grants no inherited identity either).
	if err := ss.SaveIdentity("proxyX", sessionstate.Identity{Name: "honest-heron", SessionID: "real-session-id"}); err != nil {
		t.Fatalf("SaveIdentity: %v", err)
	}
	if _, err := session.Register(session.Info{Name: "honest-heron"}); err != nil {
		t.Fatalf("Register name holder: %v", err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	freshID := s.sessionID()
	if freshID == "" {
		t.Fatal("session did not register; the test would prove nothing")
	}
	if got := s.view().persistedIdentity.SessionID; got != "real-session-id" {
		t.Fatalf("persistedSessionID = %q, want real-session-id — the capture is the gate's input", got)
	}

	s.onSessionID("victim-session-id") // the forgery: a victim's disclosed ID

	if got := s.sessionID(); got != freshID {
		t.Fatalf("adopted a forged replay: sessionID() = %q, want the generated %q", got, freshID)
	}
	if got := s.inheritedSessionIDs(); len(got) != 0 {
		t.Fatalf("inheritedSessionIDs = %v, want none — the forged ID bought no mailbox identity", got)
	}
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range live {
		if info.ID == "victim-session-id" {
			t.Fatal("a session file exists under the victim's ID — the forgery was adopted")
		}
	}
	// An authenticated proxy that replayed a stale ID converges the row to the
	// session's REAL ID, so the next reconnect adopts correctly.
	stored, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadIdentity: ok=%v err=%v", ok, err)
	}
	if stored.SessionID != freshID {
		t.Errorf("stored session ID = %q, want the row converged to the real %q", stored.SessionID, freshID)
	}
}

// TestOnSessionIDRefusesWithoutPersistedProof pins that an EMPTY persisted
// session ID means "no proof", never a wildcard: with persist_state off there
// is no row at all, and a row written before the plumb_session_id column
// existed loads with an empty SessionID. Both must refuse adoption.
func TestOnSessionIDRefusesWithoutPersistedProof(t *testing.T) {
	t.Run("persist_state disabled", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		cfg := config.Defaults()
		cfg.Session.PersistState = false
		store := config.NewStore(cfg)
		ss, err := sessionstate.Open()
		if err != nil {
			t.Fatalf("sessionstate.Open: %v", err)
		}
		defer ss.Close()

		s := newPersistSession(t, store, ss, "proxyX")
		generated := s.sessionID()
		s.onSessionID("claimed-id")
		if got := s.sessionID(); got != generated {
			t.Fatalf("sessionID() = %q with persist_state off, want the generated %q — no row, no adoption", got, generated)
		}
	})

	t.Run("row predates the session-ID column", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		store := config.NewStore(config.Defaults())
		ss, err := sessionstate.Open()
		if err != nil {
			t.Fatalf("sessionstate.Open: %v", err)
		}
		defer ss.Close()

		// A row written by an older daemon: a name, no plumb session ID.
		if err := ss.SaveIdentity("proxyX", sessionstate.Identity{Name: "steady-otter", SessionID: ""}); err != nil {
			t.Fatalf("SaveIdentity: %v", err)
		}
		s := newPersistSession(t, store, ss, "proxyX")
		generated := s.sessionID()
		if got := s.view().persistedIdentity.SessionID; got != "" {
			t.Fatalf("persistedSessionID = %q, want empty for a pre-column row", got)
		}
		s.onSessionID("claimed-id")
		if got := s.sessionID(); got != generated {
			t.Fatalf("sessionID() = %q against an empty persisted ID, want the generated %q — empty is no proof, not a wildcard", got, generated)
		}
	})
}

// TestOnSessionIDDeclinesHeldID pins the overlap guard end to end: when the
// replayed ID is held by another live session, the connection keeps its
// generated ID rather than overwriting the holder. With no persisted row this
// connection never reaches session.Adopt — the persisted-pairing gate declines
// first; the ErrIDTaken path with the gate PASSING is pinned by
// TestPersist_PersistedSessionIDCapturedDespiteNameOverlap.
func TestOnSessionIDDeclinesHeldID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	if _, err := session.Register(session.Info{ID: "held-id", Name: "holder"}); err != nil {
		t.Fatalf("Register holder: %v", err)
	}

	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	generated := s.sessionID()

	s.onSessionID("held-id")
	if got := s.sessionID(); got != generated {
		t.Fatalf("sessionID() = %q after a declined adoption, want the generated %q", got, generated)
	}
}
