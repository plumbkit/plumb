package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// The other half of PLAN-440's "honest session_start text plus a doctor
// warning". session_start tells the AGENT, at orientation, that its per-call
// identity channel is dead; doctor tells the OPERATOR, from outside any
// connection, that agents are being refused right now — which is the question
// someone asks after an agent reports being unable to write and they have no
// agent transcript in front of them.

func TestSharedConnectionCheckPassesWhenNothingIsShared(t *testing.T) {
	got := sharedConnectionCheck([]session.Info{
		{Name: "lone-otter", Folder: "/ws/a"},
		{Name: "quiet-stream", Folder: "/ws/b"},
	})
	if !got.ok || got.warn {
		t.Errorf("no shared connection should be a clean pass, got ok=%v warn=%v detail=%q", got.ok, got.warn, got.detail)
	}
}

func TestSharedConnectionCheckWarnsAndNamesTheSessions(t *testing.T) {
	got := sharedConnectionCheck([]session.Info{
		{Name: "lone-otter", Folder: "/ws/a"},
		{Name: "sage-robin", Folder: "/ws/b", Health: sharedConnectionHealth},
		{Name: "idle-maple", Folder: "/ws/c", Health: sharedConnectionHealth},
	})
	// A warning, never a failure: a shared connection is a supported topology
	// whose guard is working, not a broken installation. Exiting non-zero would
	// make `plumb doctor` red on a machine with nothing wrong with it.
	if !got.ok {
		t.Error("a shared connection must warn, not fail — the guard is working as designed")
	}
	if !got.warn {
		t.Error("a shared connection must be flagged as a caveat")
	}
	if !strings.Contains(got.detail, "sage-robin") || !strings.Contains(got.detail, "idle-maple") {
		t.Errorf("detail must name the affected sessions so the operator can find them, got %q", got.detail)
	}
	if strings.Contains(got.detail, "lone-otter") {
		t.Errorf("an unaffected session must not be named, got %q", got.detail)
	}
	// The remedy that does not depend on the client honouring anything. The
	// hook is worth naming too, but it must not be the only advice — that is
	// exactly the misdirection the refusal text already gives.
	if !strings.Contains(got.fix, "one plumb serve per") {
		t.Errorf("fix must name the transport remedy, got %q", got.fix)
	}
}

// A session that carries a DIFFERENT health note is not this check's business:
// Health is a single field, and a contested pin or a boundary violation is a
// separate condition with a separate remedy.
func TestSharedConnectionCheckIgnoresOtherHealthNotes(t *testing.T) {
	got := sharedConnectionCheck([]session.Info{
		{Name: "cross-vole", Folder: "/ws/a", Health: "blocked"},
	})
	if got.warn {
		t.Errorf("an unrelated health note must not raise the shared-connection warning, got %q", got.detail)
	}
}
