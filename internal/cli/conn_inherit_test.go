package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
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

	// The positive case changed shape in PLAN-426 and is now STRONGER: the
	// reconnecting session resumes the predecessor's internal ID outright rather
	// than running under a fresh one with a grant to read the old one's mail. It
	// reads its own mail under its own ID, so no grant is issued at all. The
	// grant path below still exists for the degraded case (see
	// TestInherit_OverlappingReconnectInheritsNothing and
	// TestRestore_DegradedOverlapInheritsMailWithoutForkingTheRecord).
	t.Run("the same proxy resumes the identity", func(t *testing.T) {
		heir := newPersistSession(t, store, ss, "proxy-victim")
		t.Cleanup(heir.close)
		if got := heir.sessionName(); got != victimName {
			t.Fatalf("reconnect came back as %q, want the persisted %q", got, victimName)
		}
		if got := heir.sessionID(); got != victimID {
			t.Fatalf("reconnect runs under %q, want the proven %q — recovery must resume the "+
				"identity, not mint a successor to it", got, victimID)
		}
		if got := heir.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("inherited = %v, want none — a session that IS the predecessor needs no "+
				"grant to read its own mail, and a redundant grant only widens what it may read", got)
		}
	})

	t.Run("a different proxy gets nothing", func(t *testing.T) {
		attacker := newPersistSession(t, store, ss, "proxy-attacker")
		t.Cleanup(attacker.close)
		if got := attacker.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("a session under its own proxy inherited %v — the identity is being handed "+
				"out on something other than the proxy ID", got)
		}
		if got := attacker.sessionID(); got == victimID {
			t.Fatal("a session under its own proxy RESUMED the victim's ID — recovery is keyed " +
				"on something other than the proxy secret")
		}
		if got := attacker.sessionName(); got == victimName {
			t.Fatalf("a session under its own proxy took the victim's name %q", victimName)
		}
	})

	t.Run("no proxy at all gets nothing", func(t *testing.T) {
		bare := newConnSession(context.Background(), detectTestPool(), nil, store, nil, ss, newSharedBudgets())
		t.Cleanup(bare.close)
		if got := bare.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("a session that presented no proxy ID inherited %v", got)
		}
		if got := bare.sessionID(); got == victimID {
			t.Fatal("a session that presented no proxy ID resumed the victim's identity")
		}
	})

	// The headline attack: take the name, by the one call built to take names.
	// It no longer even gets that far — the victim's identity is recoverable, so
	// its name is RESERVED and the rename is refused. That is a strengthening,
	// not a weakening: the attack is now blocked one step earlier than it was.
	// TestInherit_ImpostorCannotReadBoundMailEndToEnd still proves the deeper
	// guarantee, against an attacker that bypasses this refusal entirely.
	t.Run("the name cannot be taken from a recoverable identity", func(t *testing.T) {
		impostor := newPersistSession(t, store, ss, "proxy-impostor")
		t.Cleanup(impostor.close)
		if _, err := impostor.renameSession(victimName); !errors.Is(err, session.ErrNameTaken) {
			t.Fatalf("rename to a recoverable session's name %q = %v, want ErrNameTaken — the "+
				"reservation is what stops the name being reassigned during an outage", victimName, err)
		}
		if got := impostor.sessionName(); got == victimName {
			t.Fatalf("impostor is called %q despite the refusal", got)
		}
		if got := impostor.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("a session inherited %v while trying to take the name %q — this is the "+
				"original impersonation hole, reopened through the inheritance path", got, victimName)
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
//
// The impostor takes the name through session.Rename DIRECTLY, bypassing the
// durable name reservation that would refuse it (see the sibling test). That is
// deliberate and makes this the stronger test of the two: it models an attacker
// for whom the reservation does not exist — a name taken before this release, a
// record that was never written, a future path that forgets to pass the
// reservations — and asserts that the binding still holds on its own. Routing
// this through renameSession instead would make the test pass because the
// rename failed, quietly retiring the mailbox assertion it exists for.
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
	if _, err := session.Rename(impostor.sessionID(), victimName); err != nil {
		t.Fatalf("rename: %v", err)
	}
	impostor.mutate(func(v *sessionView) { v.sessName = victimName })
	if got := impostor.sessionName(); got != victimName {
		t.Fatalf("impostor is called %q, want %q — the attack is not being exercised", got, victimName)
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

// TestInherit_IdentityIsContinuousAcrossConsecutiveRecoveries replaces the old
// "the inheritance chain is bounded at one predecessor" test, because PLAN-426
// removed the chain rather than bounding it: each reconnect now RESUMES the
// proven identity, so there is one ID for the life of the proxy and no set of
// grants to grow.
//
// Three consecutive recoveries, because two cannot distinguish continuity from
// a one-step carry-forward: an implementation that resumed the predecessor but
// then recorded the wrong thing would pass at two and fork at three. The
// assertion is on the ID and the name TOGETHER — a fork in either is a fork,
// and mail addressed to the name is bound to the ID.
func TestInherit_IdentityIsContinuousAcrossConsecutiveRecoveries(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	wantID, wantName := first.sessionID(), first.sessionName()
	if wantID == "" {
		t.Fatal("the first session did not register; the test would prove nothing")
	}
	first.close()

	for i := 2; i <= 4; i++ {
		next := newPersistSession(t, store, ss, "proxyX")
		if got := next.sessionID(); got != wantID {
			t.Fatalf("recovery %d runs under %q, want the original %q — the identity forked", i, got, wantID)
		}
		if got := next.sessionName(); got != wantName {
			t.Fatalf("recovery %d is called %q, want the original %q", i, got, wantName)
		}
		if got := next.inheritedSessionIDs(); len(got) != 0 {
			t.Fatalf("recovery %d holds grants %v — a session that resumed its own identity "+
				"needs none, and accumulating them widens what it may read", i, got)
		}
		next.close()
	}

	// The record still names exactly that identity: a chain that truncated its
	// predecessor, or wrote a successor, would show up here.
	stored, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadIdentity = (%+v, %v, %v)", stored, ok, err)
	}
	if stored.SessionID != wantID || stored.Name != wantName {
		t.Fatalf("durable record = (%q, %q), want (%q, %q)", stored.SessionID, stored.Name, wantID, wantName)
	}
}

// TestInherit_RenameAfterRecoveryKeepsTheResumedID pins the write side of
// recovery: an agent that renames itself AFTER a restart re-records the
// identity, and it must record the resumed ID with the new name.
//
// This is where it can actually go wrong. The rename runs when the session has
// just taken on an ID it did not generate, so a implementation that recorded
// "the ID this connection was born with" would store a stranger's, and the next
// reconnect would recover an identity that never existed.
func TestInherit_RenameAfterRecoveryKeepsTheResumedID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	firstID := first.sessionID()
	first.close()

	second := newPersistSession(t, store, ss, "proxyX")
	if got := second.sessionID(); got != firstID {
		t.Fatalf("the reconnect runs under %q, want the proven %q", got, firstID)
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
	if stored.SessionID != firstID {
		t.Fatalf("stored session ID = %q, want the resumed %q — a rename must not re-record "+
			"the ID this connection happened to be born with", stored.SessionID, firstID)
	}
	if stored.Name != "renamed-after-restart" {
		t.Fatalf("stored name = %q, want the renamed %q — a legitimate rename must move the "+
			"durable record, or the next reconnect restores the name the agent just changed",
			stored.Name, "renamed-after-restart")
	}
	if stored.NameRevision == 0 {
		t.Error("the name changed but name_revision did not — nothing then orders this rename " +
			"against a proxy snapshot that still holds the older name")
	}

	// And the NEXT reconnect comes back as the renamed session, not the original.
	third := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(third.close)
	if got := third.sessionID(); got != firstID {
		t.Fatalf("the reconnect after the rename runs under %q, want %q", got, firstID)
	}
	if got := third.sessionName(); got != "renamed-after-restart" {
		t.Fatalf("the reconnect after the rename is called %q, want %q — a durable rename must "+
			"survive the restart that follows it", got, "renamed-after-restart")
	}
}
