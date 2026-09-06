package cli

// conn_restore_fork_test.go — the durable identity record must never be
// overwritten by a session that is not the identity it proves.
//
// Split from conn_restore_test.go (which covers recovery, reservations and
// reporting) because this is one distinct and repeatedly-broken contract: three
// separate rounds of fixes each closed one path by which a TEMPORARY identity
// reached the record and left another open. Every test here ends the same way —
// something goes wrong, the session degrades honestly, and the record is
// byte-for-byte what it was. A test that only checked the live session would
// have passed against every one of those defects.

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// TestRestore_OverlapDoesNotOverwriteTheProvenRecord is the identity-fork
// regression, and the single most important test in this file.
//
// A proxy reconnects while its predecessor is still registered. Both the ID and
// the name are therefore held, recovery cannot complete, and the session runs
// under a temporary identity — all of which is correct and expected. What must
// NOT happen is the durable record being updated to name that temporary
// identity, because the record is the only thing that knows what to come back
// to.
//
// Deliberate red proof: make adoptStoredID's ErrIDTaken branch — or
// restoreStoredName's ErrNameTaken branch — call persistIdentity(), which is
// exactly what the shipped code did, and this fails on the final comparison
// while every other test in the package still passes.
func TestRestore_OverlapDoesNotOverwriteTheProvenRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	// The established session, which STAYS LIVE — the predecessor that has not
	// finished detaching.
	first := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(first.close)
	provenID, provenName := first.sessionID(), first.sessionName()

	before, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("the first session did not record an identity: (%+v, %v, %v)", before, ok, err)
	}

	// The same proxy reconnects on top of it.
	overlapping := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(overlapping.close)

	if got := overlapping.sessionID(); got == provenID {
		t.Fatal("the overlapping reconnect took the live ID; adoption should have been refused, " +
			"so this test is not exercising the branch it targets")
	}
	if got := overlapping.sessionName(); got == provenName {
		t.Fatal("the overlapping reconnect took the live name; the rename should have been " +
			"refused, so this test is not exercising the branch it targets")
	}
	if got := overlapping.recovery(); got != recoveryDegraded {
		t.Errorf("recovery outcome = %q, want %q — a refused restoration must be reported as "+
			"such, not silently as success", got, recoveryDegraded)
	}

	after, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("the durable record vanished: (%+v, %v, %v)", after, ok, err)
	}
	if after.SessionID != provenID || after.Name != provenName {
		t.Fatalf("a REFUSED restoration rewrote the durable record to (%q, %q); it must still name "+
			"the proven identity (%q, %q). This is the identity fork: the record is the only "+
			"thing that knows what to come back to, and overwriting it makes a momentary "+
			"overlap permanent.", after.SessionID, after.Name, provenID, provenName)
	}
}

// TestRestore_UnreadableStoreDoesNotMintAReplacement: when the durable record
// cannot be READ, the session must not conclude it is a first contact.
//
// That conclusion is catastrophic in a way a failed read is not: first contact
// COMMITS an identity, so a briefly busy or locked database would have the
// replacement identity written straight over the intact record it could not
// read a moment earlier. Degrading is the only safe answer.
func TestRestore_UnreadableStoreDoesNotMintAReplacement(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())

	// Record an identity, then make the handle unusable while the file keeps the
	// row — the shape of a transient failure, not a wiped store.
	ss := openStateStore(t)
	first := newPersistSession(t, store, ss, "proxyX")
	provenID, provenName := first.sessionID(), first.sessionName()
	first.close()

	broken, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	broken.Close() // every query on this handle now fails

	blind := newConnSession(context.Background(), detectTestPool(), nil, store, nil, broken, newSharedBudgets())
	t.Cleanup(blind.close)
	blind.onProxySession("proxyX")

	if got := blind.recovery(); got != recoveryDegraded {
		t.Errorf("recovery outcome = %q, want %q — an unreadable store is a failure to recover, "+
			"never a first contact", got, recoveryDegraded)
	}

	// The record on disk is untouched, so the next reconnect still recovers.
	after, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadIdentity after the failed read = (%+v, %v, %v)", after, ok, err)
	}
	if after.SessionID != provenID || after.Name != provenName {
		t.Fatalf("a failed READ rewrote the record to (%q, %q), want the untouched (%q, %q)",
			after.SessionID, after.Name, provenID, provenName)
	}
}

// TestRestore_PersistIdentityReportsAFailedCommit pins the contract that keeps
// a first contact from advertising continuity it does not have.
//
// "Established" is a promise about the FUTURE — through the reconnect note it
// tells the agent its name and ID will come back after a restart. If the commit
// did not land there is no record for a reconnect to resolve, and the agent goes
// on using a name and ID that will be gone, with mail addressed to them
// orphaned. So restoreIdentity reports `unavailable` instead, and it can only do
// that if persistIdentity tells it the truth about the write.
//
// This asserts the reporting CONTRACT; the branch that consumes it is covered by
// TestRestore_UncommittedFirstContactReportsNoContinuity below.
func TestRestore_PersistIdentityReportsAFailedCommit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	s := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(s.close)
	if !s.persistIdentity() {
		t.Fatal("a healthy store reported a failed commit; the test would prove nothing")
	}

	// Now make every write fail. A closed handle is the bluntest form of the
	// failure and is enough for this assertion, which is about the return value.
	ss.Close()
	if s.persistIdentity() {
		t.Error("persistIdentity reported success against a store that cannot be written — the " +
			"caller then advertises durable continuity for an identity that was never recorded")
	}
}

// blockIdentityWrites makes every INSERT into session_names fail while leaving
// the table readable, and returns a function that lifts the block.
//
// This is the store shape the first-contact branch needs and that nothing else
// produces: reads working, writes failing. Permissions cannot do it —
// sessionstate.Open writes its schema, so a read-only database cannot be opened
// at all, and chmod-ing one out from under a live handle changes nothing because
// SQLite settles read-only-ness at open. A trigger can, and it needs no
// production seam: the fault lives entirely in the database the test owns.
//
// The second connection goes through internal/sqlitex like every other SQLite
// DSN in the tree, and WAL mode is what lets it coexist with the store's own.
func blockIdentityWrites(t *testing.T) func() {
	t.Helper()
	db, err := sqlitex.Open(sessionstate.DBPath(), sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("opening a second connection to install the fault: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER block_identity_writes BEFORE INSERT ON session_names
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		db.Close()
		t.Fatalf("installing the fault trigger: %v", err)
	}
	return func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS block_identity_writes`)
		db.Close()
	}
}

// TestRestore_UncommittedFirstContactReportsNoContinuity closes the branch the
// contract test above only feeds: a first contact whose commit does not land
// must not report an established identity.
//
// "Established" is a promise about the future — the reconnect note tells the
// agent its name and ID will come back after a restart. With nothing written
// there is no record for a reconnect to resolve, so the agent keeps using a name
// and ID that will be gone and mail addressed to them is orphaned. `unavailable`
// is both true and the safer error.
//
// The premise is guarded on both sides: the write must have been ATTEMPTED and
// must have FAILED, and the READ must have succeeded — a store that fails to read
// degrades on an earlier branch and would let this pass without reaching the
// commit at all.
func TestRestore_UncommittedFirstContactReportsNoContinuity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)
	t.Cleanup(blockIdentityWrites(t))

	if _, _, err := ss.LoadIdentity("proxyX"); err != nil {
		t.Fatalf("reads must still work for this test to reach the commit branch: %v", err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(s.close)

	if rec, ok, err := ss.LoadIdentity("proxyX"); err != nil || ok {
		t.Fatalf("the commit was not blocked (recorded %+v, ok=%v, err=%v); the test is not "+
			"exercising the branch it targets", rec, ok, err)
	}
	if got := s.recovery(); got == recoveryEstablished {
		t.Fatalf("recovery outcome = %q after a commit that failed — nothing was written, so a "+
			"reconnect has no record to resolve and the continuity claim is false", got)
	}
	if got := s.recovery(); got != recoveryUnavailable {
		t.Errorf("recovery outcome = %q, want %q", got, recoveryUnavailable)
	}
}

// TestRestore_DegradedSessionStartDoesNotOverwriteTheRecord closes the last
// path by which a temporary identity reached the durable record.
//
// The refusal paths and the restoring rename were each fixed in turn, and each
// time the write simply arrived through the next caller along. session_start's
// external-ID linker is that caller in practice — and it is the FIRST call most
// agents make, in exactly the window (a restart with a predecessor still
// detaching) where degradation happens. Hence the guard sits in persistIdentity
// itself rather than at a third call site.
func TestRestore_DegradedSessionStartDoesNotOverwriteTheRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(first.close) // STAYS LIVE, so the reconnect below degrades
	provenID, provenName := first.sessionID(), first.sessionName()

	degraded := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(degraded.close)
	if degraded.recovery() != recoveryDegraded {
		t.Fatalf("the reconnect did not degrade; the test is not set up as intended")
	}

	// The agent now calls session_start and links its conversation, exactly as
	// registerAllTools wires it.
	session.SetExternalID(degraded.sessionID(), "conversation-abc")
	degraded.persistIdentity()

	after, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("the durable record vanished: (%+v, %v, %v)", after, ok, err)
	}
	if after.SessionID != provenID || after.Name != provenName {
		t.Fatalf("session_start on a DEGRADED connection rewrote the record to (%q, %q); it must "+
			"still name the proven (%q, %q). A connection running under a temporary identity must "+
			"not record it, whichever caller happens to persist next.",
			after.SessionID, after.Name, provenID, provenName)
	}
}

// hideIdentityTable makes LoadIdentity fail while leaving the database
// otherwise healthy and WRITABLE, and returns a function that restores it.
//
// That combination is the point, and it is what a closed handle cannot model: a
// closed store fails reads AND writes, so a guard that wrongly permits the write
// still looks correct. A TRANSIENTLY unreadable store — SQLITE_BUSY during a
// restart storm is the real-world shape — fails the read and then happily
// accepts the overwrite.
func hideIdentityTable(t *testing.T) func() {
	t.Helper()
	db, err := sqlitex.Open(sessionstate.DBPath(), sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("opening a second connection to hide the table: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE session_names RENAME TO session_names_hidden`); err != nil {
		db.Close()
		t.Fatalf("hiding session_names: %v", err)
	}
	return func() {
		_, _ = db.Exec(`ALTER TABLE session_names_hidden RENAME TO session_names`)
		db.Close()
	}
}

// TestRestore_TransientlyUnreadableStoreDoesNotOverwriteTheRecord is the
// regression for a fork that a previous round REOPENED, and that its own tests
// could not see.
//
// restoreIdentity's LoadIdentity error branch degrades and returns BEFORE it
// records the proven identity on the view. A guard that asks only "does a record
// prove a different session ID?" therefore sees an empty proven ID, permits the
// write, and session_start's external-ID linker — the first call most agents
// make — overwrites a record that was intact all along. Card §4.2 item 4 states
// exactly this: a transient load failure must not become a new durable identity.
//
// The store here is readable-again by the time the write is attempted, which is
// what makes the test decisive: nothing except the guard prevents the overwrite.
func TestRestore_TransientlyUnreadableStoreDoesNotOverwriteTheRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	provenID, provenName := first.sessionID(), first.sessionName()
	first.close()

	// The read fails; the database is otherwise fine.
	restore := hideIdentityTable(t)
	blind := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(blind.close)
	restore()

	if got := blind.recovery(); got != recoveryDegraded {
		t.Fatalf("recovery outcome = %q after an unreadable read, want %q; the test is not set "+
			"up as intended", got, recoveryDegraded)
	}
	if blind.sessionID() == provenID {
		t.Fatal("the blind session somehow holds the proven ID; nothing would be at risk")
	}

	// The agent now links its conversation, exactly as session_start does.
	session.SetExternalID(blind.sessionID(), "conversation-abc")
	if blind.persistIdentity() {
		t.Error("a session that could not READ the record was allowed to WRITE over it")
	}

	after, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("the durable record vanished: (%+v, %v, %v)", after, ok, err)
	}
	if after.SessionID != provenID || after.Name != provenName {
		t.Fatalf("a transient READ failure let the stand-in overwrite the record: got (%q, %q), "+
			"want the untouched (%q, %q)", after.SessionID, after.Name, provenID, provenName)
	}
}

// TestRestore_ResumedIDButRefusedNameDoesNotOverwriteTheName is the second fork
// the same round reopened, and the one an ID-only comparison can never catch.
//
// When adoption SUCCEEDS and only the name is refused, the session is running
// under the proven ID — so "is the proven ID different from mine?" answers no,
// and the write proceeds, replacing the proven NAME with the generated stand-in.
// A record has two halves; guarding one is not guarding it.
//
// The setup is the duplicate-name case sessionstate.LegacyNameConflicts exists
// to report, so it is reachable with data already in the wild.
func TestRestore_ResumedIDButRefusedNameDoesNotOverwriteTheName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	// Our record, and a second retained record reserving the same name for
	// somebody else — so the ID can be resumed while the name cannot.
	if err := ss.SaveIdentity("proxyX", sessionstate.Identity{Name: "velvet-bison", SessionID: "id-A"}); err != nil {
		t.Fatal(err)
	}
	if err := ss.SaveIdentity("proxyOther", sessionstate.Identity{Name: "velvet-bison", SessionID: "id-B"}); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(s.close)
	if got := s.sessionID(); got != "id-A" {
		t.Fatalf("the ID was not resumed (%q); the test is not set up as intended", got)
	}
	if got := s.sessionName(); got == "velvet-bison" {
		t.Fatal("the name was restored; the reservation should have refused it, so this test " +
			"is not exercising the branch it targets")
	}

	session.SetExternalID(s.sessionID(), "conversation-abc")
	if s.persistIdentity() {
		t.Error("a session that was REFUSED the proven name was allowed to record its own over it")
	}

	after, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadIdentity = (%+v, %v, %v)", after, ok, err)
	}
	if after.Name != "velvet-bison" {
		t.Fatalf("the proven NAME was replaced by the temporary %q; mail addressed to "+
			"velvet-bison is now orphaned", after.Name)
	}
}
