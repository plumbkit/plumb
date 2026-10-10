package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// The refresh is throttled: N admitted state-changing calls inside one
// interval write the durable row ONCE, not N times. The row itself is the
// counter: after the first call refreshed it, it is aged again, and only a
// further write could bring it back from the prune.
func TestDeclarationRefreshWritesOncePerInterval(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-513-throttle"
	before := newPersistSession(t, store, ss, proxyID)
	before.linkExternalID(context.Background(), "conv-a")
	before.close()
	aged := time.Now().Add(-48 * time.Hour)
	if err := ss.BackdateLogicalAgents(proxyID, aged); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	after := newPersistSession(t, store, ss, proxyID) // restored, never refreshed
	after.onBeforeTool(idCtx("conv-a"), "write_file", json.RawMessage(`{}`))
	if got, _ := ss.DeclaredLinkagesFor(proxyID); len(got) != 1 {
		t.Fatalf("precondition: %v", got)
	}
	if err := ss.Prune(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got, _ := ss.DeclaredLinkagesFor(proxyID); len(got) != 1 {
		t.Fatal("positive control: the first admitted write must refresh the row")
	}

	if err := ss.BackdateLogicalAgents(proxyID, aged); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	for range 20 {
		after.onBeforeTool(idCtx("conv-a"), "write_file", json.RawMessage(`{}`))
	}
	if err := ss.Prune(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got, _ := ss.DeclaredLinkagesFor(proxyID); len(got) != 0 {
		t.Errorf("20 writes inside one refresh interval touched the row again: %v", got)
	}
}

func TestRefreshDueClaimsOneSlotPerInterval(t *testing.T) {
	var l logicalAgentState
	l.declare("conv")
	now := time.Now()
	due := 0
	for range 50 {
		if _, ok := l.refreshDue("conv", now, time.Hour); ok {
			due++
		}
	}
	if due != 1 {
		t.Fatalf("%d refreshes claimed inside one interval, want 1", due)
	}
	if _, ok := l.refreshDue("conv", now.Add(time.Hour), time.Hour); !ok {
		t.Error("the next interval must be due again")
	}
	if _, ok := l.refreshDue("undeclared", now, time.Hour); ok {
		t.Error("an undeclared linkage is never due")
	}
}

// A failed refresh hands its slot back, so the next call retries instead of
// waiting out the interval.
func TestFailedRefreshReleasesItsSlot(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-513-release"
	s := newPersistSession(t, store, ss, proxyID)
	s.logicalAgents.restoreDeclared([]string{"conv-a"}) // never refreshed: due
	ss.Close()                                          // every write now fails

	s.onBeforeTool(idCtx("conv-a"), "write_file", json.RawMessage(`{}`))
	if _, ok := s.logicalAgents.refreshDue("conv-a", time.Now(), time.Hour); !ok {
		t.Error("a failed refresh kept its slot; the next call would not retry for an interval")
	}
}

// Round-2 review of #535: the LEGACY-HEAL branch of the bounded retry (a v3
// record with a name and no session ID, whose name was held by a live session
// at reconnect) converges too, and must restore declarations the same way.
func TestDeclarationsRestoreWhenALegacyHealConverges(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	ss := openStateStore(t)
	// A live session holds the name; it persists nothing, so it reserves nothing.
	holder := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(holder.close)
	if _, err := holder.renameSession("legacy-stag"); err != nil {
		t.Fatalf("holder rename: %v", err)
	}
	const proxyID = "proxy-513-legacy"
	if err := ss.SaveIdentity(proxyID, sessionstate.Identity{Name: "legacy-stag"}); err != nil {
		t.Fatalf("save legacy record: %v", err)
	}

	s, retry := newPersistSessionGated(t, store, ss, proxyID)
	if s.recovery() != recoveryDegraded {
		t.Fatalf("precondition: the held legacy name must degrade the restore, got %q", s.recovery())
	}
	if err := ss.RecordDeclaredLinkage(proxyID, "conv-late"); err != nil {
		t.Fatalf("record declaration: %v", err)
	}
	holder.close()

	retry.awaitConverged(t)
	if got := s.recovery(); got != recoveryEstablished {
		t.Fatalf("the legacy heal converged to %q, want established", got)
	}
	s.recordLogicalAgentCall("conv-late")
	s.recordLogicalAgentCall("other")
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "conv-late"); err != nil {
		t.Errorf("a declaration written while degraded was not restored by the legacy heal: %v", err)
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "other"); err == nil {
		t.Error("control: an undeclared identity must be refused")
	}
}
