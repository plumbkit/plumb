package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// TestCollabDeps_RecordUnendedIsWiredToTheRealProbe is PLAN-517's wiring pin. The
// receipt's disconnected-session arm is only live if collabDeps actually carries
// session.RecordUnended: deleting that one field from collabDeps() left the whole
// internal/cli and internal/session suites green before this test existed, because
// the receipt's own tests inject a fake probe.
func TestCollabDeps_RecordUnendedIsWiredToTheRealProbe(t *testing.T) {
	s := newIdentitySession(t)
	deps := s.collabDeps()
	if deps.RecordUnended == nil {
		t.Fatal("collabDeps carries no RecordUnended probe, so the receipt's disconnected-session arm is dead code")
	}
	// The connection's own record is registered and un-ended, so the real probe
	// must answer true for it — this is what ties the field to real session files
	// rather than to any function of the right shape.
	if !deps.RecordUnended(s.sessionID()) {
		t.Errorf("the wired probe says this connection's own live record has ended (id %q)", s.sessionID())
	}
	session.Unregister(s.sessionID())
	if deps.RecordUnended(s.sessionID()) {
		t.Error("the wired probe still reports un-ended after Unregister marked the record ended")
	}
}
