package cli

// conn_restore_test.go — the rules that keep a transient reconnect problem from
// becoming a permanent identity fork.
//
// The defect these exist for was not in any one function. restoreName,
// adoptSessionID and persistName each behaved reasonably alone, and together
// they arranged for the REFUSAL paths to write the temporary replacement
// identity over the proven one. The next reconnect then recovered the
// replacement, and the original ID, name and mailbox were durably gone. A
// momentary overlap — the predecessor connection taking a beat longer to detach
// — was enough.
//
// So every test below asserts the same shape: something goes wrong, the session
// degrades honestly, and the DURABLE RECORD IS UNCHANGED. The last clause is
// the one that matters; a test that only checked the live session would have
// passed against the defective code.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
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

// TestRestore_RecoversAfterTheOverlapClears is the other half of the test above,
// and the reason preserving the record is worth anything: once the predecessor
// goes, the very next reconnect comes back as the original session.
//
// Without it, "the record was preserved" is only half a claim — a record could
// be preserved and still never used.
func TestRestore_RecoversAfterTheOverlapClears(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	provenID, provenName := first.sessionID(), first.sessionName()

	overlapping := newPersistSession(t, store, ss, "proxyX")
	if overlapping.recovery() != recoveryDegraded {
		t.Fatalf("the overlap did not degrade; the test is not set up as intended")
	}
	overlapping.close()
	first.close() // the predecessor finally detaches

	recovered := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(recovered.close)
	if got := recovered.sessionID(); got != provenID {
		t.Errorf("after the overlap cleared the reconnect runs under %q, want the proven %q", got, provenID)
	}
	if got := recovered.sessionName(); got != provenName {
		t.Errorf("after the overlap cleared the reconnect is called %q, want the proven %q", got, provenName)
	}
	if got := recovered.recovery(); got != recoveryRestored {
		t.Errorf("recovery outcome = %q, want %q", got, recoveryRestored)
	}
}

// TestRestore_DegradedOverlapStillInheritsMail: a session that could not adopt
// the ID but DID get the name still reads its predecessor's bound mail.
//
// This is the degraded path the inherit grant exists for, and it is why the
// grant was not deleted along with the chain it used to build. The grant is
// gated on holding the name because that is what makes it defensible: a session
// may read a predecessor's mail only while it is actually answering to the
// address that mail was sent to.
func TestRestore_DegradedOverlapStillInheritsMail(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	provenID := first.sessionID()
	provenName := first.sessionName()

	// End the session file so the NAME is free, but keep an impostor holding the
	// ID so adoption is refused. session.Register with an explicit ID is the
	// narrowest way to occupy exactly one of the two.
	first.close()
	holder, err := session.Register(session.Info{ID: provenID, Name: "id-holder"})
	if err != nil {
		t.Fatalf("occupying the proven ID: %v", err)
	}
	t.Cleanup(func() { session.Unregister(holder.ID) })

	next := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(next.close)
	if got := next.sessionID(); got == provenID {
		t.Fatal("the ID was adopted despite a live holder; the test is not exercising the degraded path")
	}
	if got := next.sessionName(); got != provenName {
		t.Fatalf("the name was not restored (%q, want %q), so the inherit grant is correctly "+
			"withheld and this test cannot check it", got, provenName)
	}
	if got := next.inheritedSessionIDs(); len(got) != 1 || got[0] != provenID {
		t.Fatalf("inherited = %v, want exactly [%s] — a session holding the predecessor's name "+
			"must still receive mail bound to it while it cannot resume the ID", got, provenID)
	}
	if got := next.recovery(); got != recoveryDegraded {
		t.Errorf("recovery outcome = %q, want %q", got, recoveryDegraded)
	}

	// And — the assertion this file's header demands of every test in it, and
	// which this one was missing — the record still names the PROVEN identity.
	//
	// A PARTIAL restoration is the dangerous case, not the total one. The name
	// restore succeeded here, and a successful rename re-records the identity;
	// recording the temporary ID it is running under would durably replace the
	// proven one, and the next reconnect would restore the temporary identity.
	// The inherit grant above papers over that for THIS connection only, which
	// is what makes the fork so easy to miss.
	after, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("the durable record vanished: (%+v, %v, %v)", after, ok, err)
	}
	if after.SessionID != provenID {
		t.Fatalf("a partially refused restoration rewrote the record's session ID to %q; it must "+
			"still be the proven %q. Adoption was refused, so the ID this connection is running "+
			"under is temporary — storing it is the identity fork, arriving through the rename "+
			"rather than through the refusal.", after.SessionID, provenID)
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
// SCOPE, stated rather than implied: this asserts the reporting contract, not
// the restoreIdentity branch that consumes it. Reaching that branch needs a
// store whose reads succeed and whose writes fail, and there is no way to build
// one through the public API — sessionstate.Open writes its schema, so a
// read-only database cannot be opened at all, and chmod-ing one out from under a
// live handle changes nothing (SQLite settles read-only-ness at open). The
// alternative would be a production-only fault-injection seam, which is a worse
// trade than an honestly narrower test.
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

// TestRestore_ExternalLinkageSurvivesTheEndedSessionFile: the authorised
// external-conversation linkage is recovered from the durable record when the
// predecessor's session JSON is gone.
//
// Before PLAN-426 the linkage lived only in that JSON, which the session
// directory collects 24 h after a session ends. So an outage longer than the
// grace window recovered the ID and the name — both held durably — and silently
// dropped the linkage, leaving `plumb mail --external-id` unable to resolve a
// session that had in fact recovered perfectly. The file is deleted here rather
// than waited out, because a test that sleeps 24 hours is not a test.
func TestRestore_ExternalLinkageSurvivesTheEndedSessionFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)
	const externalID = "conversation-abc"

	first := newPersistSession(t, store, ss, "proxyX")
	provenID := first.sessionID()
	session.SetExternalID(provenID, externalID)
	first.persistIdentity() // as the session_start external-ID linker does
	first.close()

	if rec, _, _ := ss.LoadIdentity("proxyX"); rec.ExternalID != externalID {
		t.Fatalf("the durable record did not capture the linkage: %+v", rec)
	}

	// The ended session file expires and is collected.
	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, provenID+".json")); err != nil {
		t.Fatalf("removing the ended session file: %v", err)
	}

	next := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(next.close)
	if got := next.sessionID(); got != provenID {
		t.Fatalf("the reconnect runs under %q, want the proven %q", got, provenID)
	}
	if got := next.externalID(); got != externalID {
		t.Fatalf("external linkage = %q, want %q — with the ended JSON collected, the durable "+
			"record is the only place it can come from, and losing it breaks external-ID "+
			"lookup for a session that recovered fine", got, externalID)
	}
}

// TestRestore_BlankExternalIDNeverErasesAKnownOne: a save that does not know the
// linkage must not clear the linkage the record already proves.
//
// Every reconnect re-records the identity before session_start has run, so at
// that moment the session genuinely does not know its external ID. Treating
// "unknown" as "none" would erase the linkage on the first save after every
// single restart.
func TestRestore_BlankExternalIDNeverErasesAKnownOne(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ss := openStateStore(t)

	if err := ss.SaveIdentity("proxyX", sessionstate.Identity{
		Name: "calm-stag", SessionID: "id-1", ExternalID: "conversation-abc",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ss.SaveIdentity("proxyX", sessionstate.Identity{
		Name: "calm-stag", SessionID: "id-1",
	}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadIdentity = (%+v, %v, %v)", rec, ok, err)
	}
	if rec.ExternalID != "conversation-abc" {
		t.Fatalf("external linkage = %q after a save that did not know it; want it preserved", rec.ExternalID)
	}
}

// TestRestore_AgedRecordSurvivesStartupPruning is the startup-pruning
// regression, driven through the daemon's own prune entry point.
//
// The distinction it pins is why the live-session exemption could not fix this:
// pruneSessionState is called at daemon start, BEFORE any connection is
// accepted, so the exemption list is necessarily empty. A record older than the
// TTL therefore had nothing protecting it at exactly the moment a surviving
// serve was about to need it. The exemption is deliberately not passed here.
func TestRestore_AgedRecordSurvivesStartupPruning(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	provenID, provenName := first.sessionID(), first.sessionName()
	first.close()

	// Age the record well past any plausible TTL. Prune compares against
	// updated_at, so this is the real ageing mechanism rather than a stand-in.
	if err := ss.Prune(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// And again through the daemon's own startup path, with no live exemptions —
	// the configuration that actually ships.
	pruneSessionState(ss, 1)

	next := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(next.close)
	if got := next.sessionID(); got != provenID {
		t.Errorf("after startup pruning the reconnect runs under %q, want the proven %q — an "+
			"identity must not expire by age", got, provenID)
	}
	if got := next.sessionName(); got != provenName {
		t.Errorf("after startup pruning the reconnect is called %q, want the proven %q", got, provenName)
	}
}

// TestRestore_RetainedNameIsNotHandedToANewSession: while an identity is
// recoverable, its name is reserved and a NEW session drawing a name cannot be
// given it.
//
// Session-name uniqueness has always been checked against LIVE sessions, which
// is exactly wrong for a `plumb serve` that outlives its daemon: for the length
// of the outage its name belongs to nobody. Whoever drew it next would keep it,
// and the survivor would come back renamed with its mail orphaned.
func TestRestore_RetainedNameIsNotHandedToANewSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	provenName := first.sessionName()
	first.close() // recoverable, but no longer live

	// A different connection asks for that name explicitly — the strongest form
	// of the collision, since a generated draw would simply redraw.
	newcomer := newPersistSession(t, store, ss, "proxyY")
	t.Cleanup(newcomer.close)
	if _, err := newcomer.renameSession(provenName); err == nil {
		t.Fatalf("a new session took the retained name %q; the reservation must hold it for the "+
			"identity that is coming back to it", provenName)
	}

	// And the original still gets it back.
	back := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(back.close)
	if got := back.sessionName(); got != provenName {
		t.Errorf("the original reconnect is called %q, want its reserved %q", got, provenName)
	}
}

// TestRestore_ReservationDoesNotBlockTheSameConversationResuming: a client that
// restarts `plumb serve` and re-links the SAME conversation gets its name back.
//
// A restarted serve process has a NEW proxy secret, so it can never present the
// old record's key — yet the old record still reserves the name. Left unguarded,
// the reservation locks the name away from the only party entitled to it, and
// PR #189's resume-by-external-ID guarantee turns into "you come back randomly
// renamed" — the exact churn this work exists to prevent, moved to a new trigger.
//
// The external-conversation ID is the entitlement. It is the caller's own
// conversation, the same fact `session_start` links on, and the durable record
// now stores it — so a reservation whose record names this caller's conversation
// is not a stranger's.
func TestRestore_ReservationDoesNotBlockTheSameConversationResuming(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)
	const externalID = "conversation-abc"

	first := newPersistSession(t, store, ss, "proxy-old")
	provenName := first.sessionName()
	session.SetExternalID(first.sessionID(), externalID)
	first.persistIdentity()
	first.close()

	// A RESTARTED serve process: new secret, so no claim on the old record's key.
	resumed := newPersistSession(t, store, ss, "proxy-new")
	t.Cleanup(resumed.close)
	session.SetExternalID(resumed.sessionID(), externalID)

	if _, err := resumed.renameSessionResuming(provenName, externalID); err != nil {
		t.Fatalf("the same conversation could not take back its own name %q: %v — a reservation "+
			"must hold a name FOR its owner, not away from them", provenName, err)
	}
	if got := resumed.sessionName(); got != provenName {
		t.Fatalf("resumed session is called %q, want %q", got, provenName)
	}

	// A DIFFERENT conversation is still refused — the entitlement is the linkage,
	// not merely asking.
	stranger := newPersistSession(t, store, ss, "proxy-stranger")
	t.Cleanup(stranger.close)
	if _, err := stranger.renameSessionResuming(provenName, "conversation-other"); err == nil {
		t.Fatalf("an unrelated conversation took the reserved name %q", provenName)
	}
}

// TestRestore_ResumeThenRestartStillRecoversWithoutAnotherSessionStart is the
// case the sibling test above stops one step short of, and it is the one that
// actually breaks the headline guarantee.
//
// Resuming by external ID writes a SECOND identity record claiming the name,
// under the restarted serve's own proxy key. Records are never deleted, so the
// OLD row keeps reserving that name forever. If the handshake restore excludes
// only by session ID it drops one row and is refused by the other — on every
// restart, permanently, recoverable only by yet another session_start. That is
// precisely the "no extra session_start needed" claim the real-process test
// makes, defeated by the feature added to protect it.
func TestRestore_ResumeThenRestartStillRecoversWithoutAnotherSessionStart(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)
	const externalID = "conversation-abc"

	// Serve #1 establishes the identity and links the conversation.
	first := newPersistSession(t, store, ss, "proxy-old")
	provenName := first.sessionName()
	session.SetExternalID(first.sessionID(), externalID)
	first.persistIdentity()
	first.close()

	// Serve #2 (a restarted client): new proxy key, resumes the name by linkage.
	resumed := newPersistSession(t, store, ss, "proxy-new")
	session.SetExternalID(resumed.sessionID(), externalID)
	if _, err := resumed.renameSessionResuming(provenName, externalID); err != nil {
		t.Fatalf("resume by external ID: %v", err)
	}
	resumedID := resumed.sessionID()
	resumed.close()

	// Now a plain DAEMON restart under serve #2 — no session_start at all.
	after := newPersistSession(t, store, ss, "proxy-new")
	t.Cleanup(after.close)
	if got := after.sessionName(); got != provenName {
		t.Fatalf("after a resume and then a restart the session is called %q, want %q — the record "+
			"left behind by the previous serve is still reserving the name, and the restore has "+
			"no way past it without another session_start", got, provenName)
	}
	if got := after.sessionID(); got != resumedID {
		t.Errorf("after the restart the session runs under %q, want the resumed %q", got, resumedID)
	}
	if got := after.recovery(); got != recoveryRestored {
		t.Errorf("recovery outcome = %q, want %q", got, recoveryRestored)
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

// TestRestore_LegacyRecordGainsASessionID: applying a record that has a name but
// no session ID must UPGRADE it, not merely use it.
//
// Suppressing the write during a restore is right when there is a proven ID to
// protect and wrong when there is not: left alone such a row never gains an ID,
// so it never reserves its name (a reservation with no owner is skipped), bound
// mail never survives a restart, and recovery reports degraded forever. The
// same reasoning the invalid-name branch already applies.
func TestRestore_LegacyRecordGainsASessionID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	if err := ss.SaveIdentity("proxyX", sessionstate.Identity{Name: "legacy-stag"}); err != nil {
		t.Fatal(err)
	}

	first := newPersistSession(t, store, ss, "proxyX")
	if got := first.sessionName(); got != "legacy-stag" {
		t.Fatalf("the legacy name was not applied (%q); the test is not set up as intended", got)
	}
	healedID := first.sessionID()
	first.close()

	rec, ok, err := ss.LoadIdentity("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadIdentity = (%+v, %v, %v)", rec, ok, err)
	}
	if rec.SessionID != healedID {
		t.Fatalf("the record still has session ID %q after being applied; want it upgraded to %q, "+
			"or it reserves nothing and degrades on every reconnect forever", rec.SessionID, healedID)
	}

	// And the next reconnect is a full restore rather than another degrade.
	next := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(next.close)
	if got := next.recovery(); got != recoveryRestored {
		t.Errorf("the reconnect after healing reports %q, want %q", got, recoveryRestored)
	}
	if got := next.sessionID(); got != healedID {
		t.Errorf("the reconnect runs under %q, want the healed %q", got, healedID)
	}
}

// TestRestore_ProjectConfigCannotDisableReservations: `session.persist_state` is
// a ClassPreference key, so an UNTRUSTED project config may set it.
//
// That is defensible while it only makes a connection's own state more or less
// sticky. It stops being defensible once it also decides whether a guard
// protecting OTHER sessions' names applies: a repository could otherwise ship
// three lines of config that let the session attached to it take a name
// reserved for somebody else, and receive the mail addressed to that name. So
// the reservation gate reads the GLOBAL setting.
func TestRestore_ProjectConfigCannotDisableReservations(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ss := openStateStore(t)
	if err := ss.SaveIdentity("proxy-victim", sessionstate.Identity{
		Name: "calm-stag", SessionID: "id-victim",
	}); err != nil {
		t.Fatal(err)
	}

	// Global says persistence is ON; the session's merged view says off, as a
	// project config would make it.
	store := config.NewStore(config.Defaults())
	s := newPersistSession(t, store, ss, "proxy-attacker")
	t.Cleanup(s.close)
	s.mutate(func(v *sessionView) { v.session.PersistState = false })

	if _, err := s.renameSession("calm-stag"); err == nil {
		t.Fatal("a project-level persist_state=false let a session take a name reserved for " +
			"another identity; the reservation guard protects other sessions and must not be " +
			"switchable from an untrusted repository")
	}
}

// TestRestore_ReservationsAreNotEnforcedWithPersistenceOff: an explicit opt-out
// must disable the whole feature, reads included.
//
// Reservations were read unconditionally while only writes were gated, so with
// `persist_state = false` every historically recorded name stayed locked out
// forever — and with the feature off, no session can ever reclaim one. An opt-out
// that leaves the costs in place and removes only the benefit is worse than not
// having the feature.
func TestRestore_ReservationsAreNotEnforcedWithPersistenceOff(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ss := openStateStore(t)
	if err := ss.SaveIdentity("proxy-ghost", sessionstate.Identity{
		Name: "calm-stag", SessionID: "id-ghost",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Session.PersistState = false
	s := newPersistSession(t, config.NewStore(cfg), ss, "proxyX")
	t.Cleanup(s.close)

	if _, err := s.renameSession("calm-stag"); err != nil {
		t.Fatalf("a reservation was enforced with persistence disabled: %v — with the feature off "+
			"nothing can ever reclaim the name, so enforcing it locks it away permanently", err)
	}
}

// TestRestore_IdentityMetaStatesTheOutcome pins the initialize-result snapshot:
// the daemon must state who the connection is AND whether that was recovered,
// because a proxy that cannot tell those apart cannot report them honestly.
func TestRestore_IdentityMetaStatesTheOutcome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	first := newPersistSession(t, store, ss, "proxyX")
	if got := first.identityMeta()[identityMetaRecovery]; got != string(recoveryEstablished) {
		t.Errorf("first contact reports %v, want %q", got, recoveryEstablished)
	}
	provenID, provenName := first.sessionID(), first.sessionName()
	first.close()

	next := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(next.close)
	meta := next.identityMeta()
	if got := meta[identityMetaSessionID]; got != provenID {
		t.Errorf("identity meta session_id = %v, want %q", got, provenID)
	}
	if got := meta[identityMetaName]; got != provenName {
		t.Errorf("identity meta name = %v, want %q", got, provenName)
	}
	if got := meta[identityMetaRecovery]; got != string(recoveryRestored) {
		t.Errorf("identity meta recovery = %v, want %q", got, recoveryRestored)
	}

	// A connection with no proxy credential has no continuity to offer, and must
	// say so rather than imply a recovery it cannot perform.
	bare := newConnSession(context.Background(), detectTestPool(), nil, store, nil, ss, newSharedBudgets())
	t.Cleanup(bare.close)
	if got := bare.identityMeta()[identityMetaRecovery]; got != string(recoveryUnavailable) {
		t.Errorf("a connection with no proxy credential reports %v, want %q", got, recoveryUnavailable)
	}
	// It is still told its own name and ID — that costs nothing and is what
	// PLAN-425 item 1 asks for, one exchange earlier than session_start.
	if got := bare.identityMeta()[identityMetaSessionID]; got != bare.sessionID() {
		t.Errorf("identity meta session_id = %v, want %q", got, bare.sessionID())
	}
}

// TestRestore_LegacyRecordWithNoSessionIDIsNotReportedAsRestored: a schema-v3
// record carries a name and no session ID, so there is nothing to resume — which
// is not the same as having resumed something.
//
// The distinction is not pedantry. `recoveryRestored` makes the reconnect note
// say "Your session identity was restored: you are still <name> (<id>)", and
// with a legacy record that <id> is one the session has never held: mail bound
// to the predecessor is unreachable while the note reports success. Degraded is
// the honest answer — the name came back, the identity did not.
func TestRestore_LegacyRecordWithNoSessionIDIsNotReportedAsRestored(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)

	// A pre-v4 row: name only, exactly what an upgraded database holds.
	if err := ss.SaveIdentity("proxyX", sessionstate.Identity{Name: "legacy-stag"}); err != nil {
		t.Fatal(err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(s.close)
	if got := s.sessionName(); got != "legacy-stag" {
		t.Fatalf("the legacy name was not restored (%q); the test is not set up as intended", got)
	}
	if got := s.recovery(); got == recoveryRestored {
		t.Fatalf("recovery outcome = %q for a record with no session ID — nothing was resumed, so "+
			"the note would claim continuity for an ID this session has never held", got)
	}
	if got := s.recovery(); got != recoveryDegraded {
		t.Errorf("recovery outcome = %q, want %q", got, recoveryDegraded)
	}
}

// TestRestore_PersistenceDisabledOffersNoContinuity: an explicit opt-out is
// honoured and reported, never quietly overridden to make recovery work.
func TestRestore_PersistenceDisabledOffersNoContinuity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.Session.PersistState = false
	store := config.NewStore(cfg)
	ss := openStateStore(t)

	s := newPersistSession(t, store, ss, "proxyX")
	t.Cleanup(s.close)
	if got := s.recovery(); got != recoveryUnavailable {
		t.Errorf("recovery outcome = %q with persistence off, want %q", got, recoveryUnavailable)
	}
	if _, ok, _ := ss.LoadIdentity("proxyX"); ok {
		t.Error("persistence is off, yet an identity record was written — a disabled feature " +
			"must not write to the store it was disabled for")
	}
}

// TestDaemonInstanceID: the marker must be stable within a process and differ
// between starts, or the reconnect note's restart/transport distinction is
// built on nothing.
func TestDaemonInstanceID(t *testing.T) {
	start := time.Now()
	a, b := daemonInstanceID(start), daemonInstanceID(start)
	if a == "" {
		t.Fatal("daemonInstanceID returned empty for a real start time")
	}
	if a != b {
		t.Errorf("the marker is not stable within a process: %q vs %q", a, b)
	}
	if c := daemonInstanceID(start.Add(time.Nanosecond)); c == a {
		t.Error("two different starts produced the same marker; a restart would read as a " +
			"transport reconnect")
	}
	if got := daemonInstanceID(time.Time{}); got != "" {
		t.Errorf("an unknown start time must yield no marker (so the note says \"unknown\"), got %q", got)
	}
}
