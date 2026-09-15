package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/collab"
)

// TestMailWaiting_CountsBoundMailAfterARename is the wake probe's half of the
// rename fix, and the reason it is asserted here rather than left to
// internal/collab: this is the call the idle-agent Stop hook actually makes, and
// a session whose identity recovery could not reapply its stored name is exactly
// the one that will sit idle waiting for a reply it was never woken for.
//
// The failure it guards is silent in the worst way — "no mail waiting" is the
// ordinary answer, so a probe blind to the session's own bound mail looks
// healthy forever.
func TestMailWaiting_CountsBoundMailAfterARename(t *testing.T) {
	ws := t.TempDir()
	const id = "sess-gentle-mink"
	// Written while the session answered to gentle-mink; it now answers to
	// icy-beaver after a degraded identity recovery, same internal ID.
	putBoundTestNote(t, ws, "ancient-stag", "gentle-mink", id, "bound before the rename")

	ages, err := mailWaiting(collab.Claimant{Name: "icy-beaver", ID: id, Workspace: ws})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(ages) != 1 {
		t.Fatalf("probe reported %d waiting, want 1 — mail bound to this session's own ID must "+
			"wake it whatever name it is currently answering to", len(ages))
	}

	// A stranger answering to the old name is still woken for nothing.
	ages, err = mailWaiting(collab.Claimant{Name: "gentle-mink", ID: "sess-stranger", Workspace: ws})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(ages) != 0 {
		t.Fatalf("probe woke a stranger for a bound note: %d", len(ages))
	}
}
