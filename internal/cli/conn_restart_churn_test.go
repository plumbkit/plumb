package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// conn_restart_churn_test.go — a @@BT@@plumb serve@@BT@@ restart adds no second claim on
// the name its predecessor held, and destroys nothing.
//
// The store rules are tested in internal/sessionstate. This tests the FACTORY: the
// sequence that produced the duplicates measured in the field, driven through the
// real connSession path rather than by calling SaveIdentity by hand. Without it a
// correct store-side collapse could still be fed something else, because nothing
// would pin which caller feeds it.
//
// It also pins the property that makes the fix safe: the predecessor's RECORD
// survives, so a serve still alive behind a dropped socket comes back as itself
// instead of as a stranger with a new session ID and its mail stranded.

// claimsOn returns the claims a name holds.
func claimsOn(t *testing.T, ss *sessionstate.Store, name string) []sessionstate.Reservation {
	t.Helper()
	res, err := ss.Reservations()
	if err != nil {
		t.Fatal(err)
	}
	var out []sessionstate.Reservation
	for _, r := range res {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// TestServeRestart_LeavesExactlyOneClaim reproduces the churn: a serve runs under
// one proxy secret and links its conversation; it restarts, which mints a FRESH
// proxy secret, so the identity upsert inserts a second row and the resume path
// renames it onto the name the predecessor held.
//
// Both rows hold that name, and identity rows are never expired by age, so on the
// machine this was measured on 11 of 16 conflicting names were exactly this. The
// claim must resolve to the live generation — and the predecessor's record must
// survive it.
func TestServeRestart_LeavesExactlyOneClaim(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	const conv = "conv-restart"

	// Generation 1: a serve under proxy-1 links the conversation and takes a name.
	first := newPersistSession(t, store, ss, "proxy-1")
	first.linkExternalID(conv)
	if _, err := first.renameSession("gentle-mink"); err != nil {
		t.Fatalf("first generation rename: %v", err)
	}
	rec, ok, err := ss.LoadIdentity("proxy-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || rec.Name != "gentle-mink" || rec.ExternalID != conv {
		t.Fatalf("generation 1 did not record its identity: ok=%v %+v", ok, rec)
	}
	firstID := first.sessionID()

	// The serve exits. Its session ends, which is what makes the name inheritable.
	first.close()
	session.Unregister(firstID)

	// Generation 2: the serve restarts. A fresh proxy secret means a NEW row.
	second := newPersistSession(t, store, ss, "proxy-2")
	t.Cleanup(second.close)
	if got := second.linkExternalID(conv); got != "gentle-mink" {
		t.Fatalf("the restarted serve did not inherit its own name, got %q — the churn this test is "+
			"about cannot happen, so it is testing nothing", got)
	}

	if claims := claimsOn(t, ss, "gentle-mink"); len(claims) != 1 || claims[0].SessionID != second.sessionID() {
		t.Errorf("gentle-mink holds %d claim(s) %+v, want exactly one held by the live generation — a "+
			"second row of the same conversation and name must not add a second claim", len(claims), claims)
	}
	// Collapsed out of the answer, not out of the table: the predecessor's own
	// record still resolves, so a proxy holding its secret is still itself.
	rec1, ok1, err := ss.LoadIdentity("proxy-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok1 || rec1.SessionID != firstID || rec1.Name != "gentle-mink" {
		t.Errorf("the predecessor's record was damaged: ok=%v %+v — deleting or blanking it turns its "+
			"next reconnect into first contact and strands the mail bound to %s", ok1, rec1, firstID)
	}
}

// TestServeRestart_PredecessorComesBackAsItself is the regression test for why
// this retires a CLAIM rather than deleting a row. The predecessor's socket
// dropped, but its process and its proxy secret are still valid; a newer serve of
// the same conversation has since taken the name. When the predecessor reconnects
// it must resume its own session ID — otherwise the fork is permanent and every
// note bound to that ID is unreachable.
func TestServeRestart_PredecessorComesBackAsItself(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	const conv = "conv-fork"

	first := newPersistSession(t, store, ss, "proxy-1")
	first.linkExternalID(conv)
	if _, err := first.renameSession("gentle-mink"); err != nil {
		t.Fatalf("first generation rename: %v", err)
	}
	proven := first.sessionID()
	first.close()
	session.Unregister(proven)

	// A newer serve of the same conversation takes the name; the predecessor's row
	// is its superseded generation from that moment on.
	second := newPersistSession(t, store, ss, "proxy-2")
	t.Cleanup(second.close)
	if got := second.linkExternalID(conv); got != "gentle-mink" {
		t.Fatalf("the newer serve did not inherit the name, got %q", got)
	}

	// The predecessor was never gone: it reconnects under its own proxy secret.
	revived := newPersistSession(t, store, ss, "proxy-1")
	t.Cleanup(revived.close)
	if got := revived.sessionID(); got != proven {
		t.Fatalf("the predecessor came back as a STRANGER: it holds %s, not its proven %s — the durable "+
			"record that authorises the resume was destroyed, so the fork is permanent and mail bound to "+
			"the old ID is stranded", got, proven)
	}
}

// TestServeRestart_LeavesAnotherConversationsClaimAlone is the guard beside it: a
// restart proves something about its OWN conversation and nothing about anybody
// else's, so a second agent holding the same name keeps its claim — that remains
// the ambiguous case the daemon still reports.
func TestServeRestart_LeavesAnotherConversationsClaimAlone(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	// A stranger's retained row, from a different conversation, claiming the name.
	if err := ss.SaveIdentity("proxy-stranger", sessionstate.Identity{Name: "gentle-mink", SessionID: "sess-stranger", ExternalID: "conv-other"}); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxy-mine")
	t.Cleanup(s.close)
	s.linkExternalID("conv-mine")
	// The rename is refused — the stranger's row reserves the name — which is the
	// pre-existing behaviour this change deliberately does not alter.
	_, _ = s.renameSession("gentle-mink")

	// Positive control: the caller's OWN second generation under its own name, which
	// the same Reservations call must collapse. Without it this test would also pass
	// for a store that collapses nothing at all.
	mine, ok, err := ss.LoadIdentity("proxy-mine")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || mine.Name == "" {
		t.Fatalf("the session recorded no identity, so this test cannot build its positive control: ok=%v %+v", ok, mine)
	}
	if err := ss.SaveIdentity("proxy-mine-old", sessionstate.Identity{Name: mine.Name, SessionID: "sess-mine-old", ExternalID: "conv-mine"}); err != nil {
		t.Fatal(err)
	}
	if claims := claimsOn(t, ss, mine.Name); len(claims) != 1 {
		t.Errorf("the caller's own two rows produced %d claims on %q, want 1 — the positive control for "+
			"this test never fired: %+v", len(claims), mine.Name, claims)
	}
	if claims := claimsOn(t, ss, "gentle-mink"); len(claims) != 1 || claims[0].SessionID != "sess-stranger" {
		t.Errorf("gentle-mink claims = %+v, want the stranger's claim alone: a restart proves nothing "+
			"about another conversation, and dropping its claim hands one agent's reserved name to another", claims)
	}
}
