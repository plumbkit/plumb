package cli

// daemon_sessionstate_test.go — the daemon-start session-state maintenance.
//
// These exercise sweepLegacyWidePins with its PRODUCTION predicate binding.
// internal/sessionstate's own tests inject a stub, so without this file the
// whole feature could be turned off (`SweepWidePinsOnce(func(string) bool
// { return false })`) with every test still green — a surviving mutant an
// adversarial review demonstrated.

import (
	"context"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// The daemon's sweep really does remove a wide pin, using the real containment
// predicate rather than a test stub — and really does spare an ordinary project.
func TestSweepLegacyWidePins_UsesTheRealContainmentPredicate(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}
	proj := freshTempDir(t)

	_, ss := newOriginStore(t)
	if err := ss.UpsertPin("forged", wide, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if err := ss.UpsertPin("honest", proj, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}

	sweepLegacyWidePins(ss)

	if _, _, _, ok, err := ss.LoadPin("forged"); err != nil || ok {
		t.Errorf("the wide pin survived the daemon sweep (ok=%v err=%v) — the production predicate is not wired", ok, err)
	}
	if _, _, _, ok, err := ss.LoadPin("honest"); err != nil || !ok {
		t.Errorf("an ordinary project pin was swept (ok=%v err=%v)", ok, err)
	}
}

// Nil-safe: a daemon whose session-state store failed to open passes nil, and
// the sweep must not panic on the startup path.
func TestSweepLegacyWidePins_NilStore(t *testing.T) {
	sweepLegacyWidePins(nil) // must not panic
}

// seedAgedSession writes every kind of expendable row a long-lived serve
// accumulates — a connection-level read and pin, a per-agent read and pin, a
// logical-agent record and a conversation's declaration (#513) — then ages them
// all a month past the default TTL.
func seedAgedSession(t *testing.T, ss *sessionstate.Store, proxyID, ws string) {
	t.Helper()
	file := filepath.Join(ws, "a.go")
	for _, agent := range []string{"", "agent-a"} {
		if err := ss.UpsertReadForAgent(proxyID, agent, ws, file, time.Unix(1, 0), "sha"); err != nil {
			t.Fatal(err)
		}
		if err := ss.UpsertPinForAgent(proxyID, agent, ws, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
			t.Fatal(err)
		}
	}
	if err := ss.RecordLogicalAgent(proxyID, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if err := ss.RecordDeclaredLinkage(proxyID, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if err := ss.BackdateSession(proxyID, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// expendableRows counts what survives of seedAgedSession's rows, in its order:
// two reads, two pins, one logical agent, one declared conversation.
func expendableRows(t *testing.T, ss *sessionstate.Store, proxyID, ws string) (reads, pins, agents, declared int) {
	t.Helper()
	for _, agent := range []string{"", "agent-a"} {
		recs, err := ss.LoadReadsForAgent(proxyID, agent, ws)
		if err != nil {
			t.Fatal(err)
		}
		reads += len(recs)
		if _, _, _, ok, err := ss.LoadPinForAgent(proxyID, agent); err != nil {
			t.Fatal(err)
		} else if ok {
			pins++
		}
	}
	ids, err := ss.LogicalAgentIDsFor(proxyID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	linkages, err := ss.DeclaredLinkagesFor(proxyID)
	if err != nil {
		t.Fatal(err)
	}
	return reads, pins, len(ids), len(linkages)
}

// Issue #525: the daemon's start-up maintenance must not age out expendable
// state. It runs before any `plumb serve` has reconnected, so it cannot tell a
// live session from a dead one, and rows are refreshed only when rewritten — a
// serve kept open for days had its pins and read records deleted by a restart
// while it was still in use, and "changed since you read it" then failed open.
func TestStartupMaintenance_KeepsAgedStateOfSessionsThatMayReconnect(t *testing.T) {
	_, ss := newOriginStore(t)
	ws := freshTempDir(t)
	seedAgedSession(t, ss, "proxyX", ws)

	maintainSessionStateAtStart(ss)

	if reads, pins, agents, declared := expendableRows(t, ss, "proxyX", ws); reads != 2 || pins != 2 || agents != 1 || declared != 1 {
		t.Fatalf("after start-up maintenance: %d reads, %d pins, %d logical agents, %d declared conversations; want 2, 2, 1, 1 — "+
			"start-up cannot know whether a serve is about to reconnect, so it must leave aged state to the reaper",
			reads, pins, agents, declared)
	}
}

// The other half of #525's retention rule: deferring to the reaper must not make
// dead sessions' state unbounded. A reaper pass reclaims every aged expendable
// row of a session that is not connected, keeps a connected one's however old,
// and never touches an identity record.
func TestReaper_PrunesAgedStateOfDeadSessionsOnly(t *testing.T) {
	store, ss := newOriginStore(t)
	ws := freshTempDir(t)
	for _, id := range []string{"live", "dead"} {
		seedAgedSession(t, ss, id, ws)
		if err := ss.SaveIdentity(id, sessionstate.Identity{Name: "swift-falcon-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	registry := newConnRegistry()
	registry.add("conn-live", connHandle{cancel: func() {}, proxySessionID: func() string { return "live" }})

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		runIdleReaper(context.Background(), store, registry, ss, nil, ticks)
		close(done)
	}()
	ticks <- time.Now()
	close(ticks)
	<-done

	if reads, pins, agents, declared := expendableRows(t, ss, "live", ws); reads != 2 || pins != 2 || agents != 1 || declared != 1 {
		t.Errorf("connected session after a reaper pass: %d reads, %d pins, %d logical agents, %d declared conversations; want 2, 2, 1, 1",
			reads, pins, agents, declared)
	}
	if reads, pins, agents, declared := expendableRows(t, ss, "dead", ws); reads != 0 || pins != 0 || agents != 0 || declared != 0 {
		t.Errorf("disconnected session after a reaper pass: %d reads, %d pins, %d logical agents, %d declared conversations; want none — "+
			"a dead session's state must still be reclaimed", reads, pins, agents, declared)
	}
	for _, id := range []string{"live", "dead"} {
		if _, ok, err := ss.LoadIdentity(id); err != nil || !ok {
			t.Errorf("identity record of %q was pruned (ok=%v err=%v); it is retained regardless of age", id, ok, err)
		}
	}
}
