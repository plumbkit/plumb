package cli

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// conn_inherit_test.go pins mailbox identity inheritance across a daemon
// restart, and — the part that matters far more — pins that it CANNOT be
// obtained any other way.
//
// Binding a message to a session stopped a later holder of the name reading it.
// The cost was that a daemon restart, which registers the reconnected
// connection under a fresh session ID, stranded every unread bound message.
// Inheritance buys that back. It is safe only for as long as the grant is
// authorised by the proxy's own unguessable session ID and never by a name, so
// the negative tests below are the load-bearing ones: if any of them starts
// passing an inherited identity to a session that merely answers to the right
// name, the original vulnerability is back with extra steps.

// openStateStore opens a sessionstate store rooted in a temp XDG dir.
func openStateStore(t *testing.T) *sessionstate.Store {
	t.Helper()
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	t.Cleanup(ss.Close)
	return ss
}

// TestInherit_OnlyThroughTheProxyAuthenticatedPath is the security crux. Four
// sessions ask for the same predecessor identity; only the one presenting the
// predecessor's PROXY session ID may have it. Everything else — a different
// proxy, no proxy at all, or simply holding the name — must get nothing.
func TestInherit_OnlyThroughTheProxyAuthenticatedPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	// A session ran under proxy "proxy-victim", answering to a name, and ended.
	victim := newPersistSession(t, store, ss, "proxy-victim")
	victimName, victimID := victim.sessionName(), victim.sessID
	if victimID == "" {
		t.Fatal("the victim session did not register; the test would prove nothing")
	}
	victim.close()

	t.Run("the same proxy inherits", func(t *testing.T) {
		heir := newPersistSession(t, store, ss, "proxy-victim")
		t.Cleanup(heir.close)
		if got := heir.sessionName(); got != victimName {
			t.Fatalf("reconnect came back as %q, want the persisted %q", got, victimName)
		}
		if got := heir.inheritedSessionIDs(); len(got) != 1 || got[0] != victimID {
			t.Fatalf("inherited = %v, want exactly [%s]", got, victimID)
		}
		if heir.sessID == victimID {
			t.Fatal("the heir reused the predecessor's session ID; inheritance is not being exercised")
		}
	})

	t.Run("a different proxy inherits nothing", func(t *testing.T) {
		attacker := newPersistSession(t, store, ss, "proxy-attacker")
		t.Cleanup(attacker.close)
		if got := attacker.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("a session under its own proxy inherited %v — the identity is being handed "+
				"out on something other than the proxy ID", got)
		}
	})

	t.Run("no proxy at all inherits nothing", func(t *testing.T) {
		bare := newConnSession(context.Background(), detectTestPool(), nil, store, nil, ss, newSharedBudgets())
		t.Cleanup(bare.close)
		if got := bare.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("a session that presented no proxy ID inherited %v", got)
		}
	})

	// The headline attack: take the name, by the one call built to take names.
	t.Run("taking the name inherits nothing", func(t *testing.T) {
		impostor := newPersistSession(t, store, ss, "proxy-impostor")
		t.Cleanup(impostor.close)
		if _, err := impostor.renameSession(victimName); err != nil {
			t.Fatalf("rename to the now-free %q failed, so the attack is not being exercised: %v",
				victimName, err)
		}
		if got := impostor.sessionName(); got != victimName {
			t.Fatalf("impostor is called %q, want %q", got, victimName)
		}
		if got := impostor.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("a session inherited %v by ADOPTING THE NAME %q — this is the original "+
				"impersonation hole, reopened through the inheritance path", got, victimName)
		}
	})
}

// withMailbox gives a persist-path session the collab wiring the delivery path
// needs. Without it messageHint returns "" because chatWatch is nil, which would
// make every negative assertion below pass for the wrong reason — the positive
// assertion on the heir is what keeps them honest, and this is what lets it hold.
func withMailbox(t *testing.T, s *connSession, ws string, cc config.CollabConfig) *connSession {
	t.Helper()
	s.collabPool = newCollabPool()
	s.chatWatch = &chatWatch{}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws; v.collab = cc })
	t.Cleanup(s.collabPool.closeAll)
	return s
}

// TestInherit_ImpostorCannotReadBoundMailEndToEnd walks the attack all the way
// to the mailbox rather than stopping at the accessor: the impostor holds the
// victim's name, is pinned to the same workspace, and must still read nothing,
// while the properly reconnected session receives the message.
func TestInherit_ImpostorCannotReadBoundMailEndToEnd(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)
	ws := t.TempDir()
	cc := config.CollabConfig{Mailbox: true, ChatBudgetBytes: 512}

	victim := newPersistSession(t, store, ss, "proxy-victim")
	victimName, victimID := victim.sessionName(), victim.sessID
	victim.close()

	// A peer wrote to the victim while it was live, so the row is BOUND to it.
	seed := func(s *connSession) {
		st := s.collabPool.acquire(ws)
		if st == nil {
			t.Fatal("acquire collab store")
		}
		if _, err := st.PutNote(context.Background(), collab.NoteInput{
			AuthorSession: "bob", AuthorID: "id-bob", Body: "bound to the pre-restart session",
			Addressee: victimName, AddresseeID: victimID, TTL: time.Hour,
		}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	impostor := withMailbox(t, newPersistSession(t, store, ss, "proxy-impostor"), ws, cc)
	if _, err := impostor.renameSession(victimName); err != nil {
		t.Fatalf("rename: %v", err)
	}
	seed(impostor)
	if got := impostor.messageHint(context.Background()); got != "" {
		t.Fatalf("an impostor holding the name read bound mail: %q", got)
	}
	impostor.close() // free the name for the legitimate reconnect

	heir := withMailbox(t, newPersistSession(t, store, ss, "proxy-victim"), ws, cc)
	if got := heir.messageHint(context.Background()); got == "" {
		t.Fatal("the reconnected session did not receive mail bound to the session it continues")
	}
}

// TestInherit_OverlappingReconnectInheritsNothing. A proxy can reconnect while
// its predecessor is still registered and still holding the name; the rename
// then fails with ErrNameTaken and the session keeps its generated name. It must
// not walk away with the identity either. Holding an identity for a name you do
// not have is not exploitable on its own — the claim also matches on the name —
// but the two are one fact, and letting them drift apart is how a later change
// turns a harmless mismatch into a live one.
func TestInherit_OverlappingReconnectInheritsNothing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	// First session records its identity, and STAYS LIVE.
	first := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(first.close)
	firstName := first.sessionName()

	// The same proxy reconnects before the predecessor is reaped.
	overlapping := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(overlapping.close)

	if got := overlapping.sessionName(); got == firstName {
		t.Fatalf("the overlapping reconnect took the live name %q; the rename should have been "+
			"refused, so this test is not exercising the branch it targets", firstName)
	}
	if got := overlapping.inheritedSessionIDs(); len(got) != 0 {
		t.Errorf("a session that could NOT take the name still inherited %v — inheritance must "+
			"follow the name it was granted for", got)
	}
}

// TestInherit_ChainIsBoundedToOnePredecessor states the policy and pins it.
// Each reconnect records its OWN session ID, so a session inherits the one
// before it and forgets the one before that. The set never grows, which is what
// stops a long-lived proxy accumulating identities.
func TestInherit_ChainIsBoundedToOnePredecessor(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	firstID := first.sessID
	first.close()

	second := newPersistSession(t, store, ss, "proxyX")
	secondID := second.sessID
	if got := second.inheritedSessionIDs(); len(got) != 1 || got[0] != firstID {
		t.Fatalf("second session inherited %v, want [%s]", got, firstID)
	}
	second.close()

	third := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(third.close)
	got := third.inheritedSessionIDs()
	if len(got) != 1 || got[0] != secondID {
		t.Fatalf("third session inherited %v, want exactly [%s] — one predecessor", got, secondID)
	}
	for _, id := range got {
		if id == firstID {
			t.Errorf("the chain still carries the first session %q; identities are accumulating "+
				"across restarts instead of being bounded at one", firstID)
		}
	}
}

// TestInherit_PersistRecordsThisSessionNotItsPredecessor is the mechanism behind
// the bound above, asserted directly and after a RENAME — the case where it can
// actually go wrong. rename_session re-persists the identity, and by then this
// session is already carrying its predecessor's ID; storing that instead of its
// own would hand the same predecessor to every future reconnect, and the chain
// would never let go of it.
func TestInherit_PersistRecordsThisSessionNotItsPredecessor(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	firstID := first.sessID
	first.close()

	second := newPersistSession(t, store, ss, "proxyX")
	secondID := second.sessID
	if got := second.inheritedSessionIDs(); len(got) != 1 || got[0] != firstID {
		t.Fatalf("second inherited %v, want [%s]", got, firstID)
	}
	// The agent renames itself after the restart, which re-persists the identity.
	if _, err := second.renameSession("renamed-after-restart"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	second.close()

	stored, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadIdentity = (%+v, %v, %v)", stored, ok, err)
	}
	if stored.SessionID != secondID {
		t.Fatalf("stored session ID = %q, want this session's own %q", stored.SessionID, secondID)
	}
	if stored.SessionID == firstID {
		t.Fatal("the rename re-persisted the INHERITED id, so the chain can never forget it")
	}

	third := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(third.close)
	if got := third.inheritedSessionIDs(); len(got) != 1 || got[0] != secondID {
		t.Fatalf("third inherited %v, want exactly [%s]", got, secondID)
	}
}
