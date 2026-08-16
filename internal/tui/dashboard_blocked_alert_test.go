package tui

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// The blocked alert must carry the session's OWN remedy.
//
// "blocked" covers causes with different fixes — a boundary violation, a
// refused sticky re-pin (issue #182), and a refused wide workspace claim
// (issue #318). The alert used to prescribe "start a new MCP connection" for
// all of them; for the #318 case that advice LOOPS, because a fresh connection
// replays the same pin and is refused again. An independent review caught a
// legitimate user being told to do the one thing that cannot help.
func TestBlockedAlert_PrefersTheSessionsOwnMessage(t *testing.T) {
	const msg = "the home-containing workspace /Users was claimed over the initialize _meta " +
		"channel and refused, and no persisted pin restored it; call session_start(...)"

	got := blockedAlert(msg)
	if got == "" {
		t.Fatal("blocked alert is empty")
	}
	if strings.Contains(got, "new MCP connection") {
		t.Fatalf("the alert still prescribes reconnecting, which loops for a refused wide claim: %s", got)
	}
	if !strings.Contains(got, "session_start") {
		t.Errorf("the alert drops the remedy the session recorded: %s", got)
	}
}

// A mark with no message still says something useful, and still does not
// prescribe the reconnect.
func TestBlockedAlert_FallsBackWithoutAMessage(t *testing.T) {
	got := blockedAlert("   ")
	if got == "" {
		t.Fatal("a blocked session with no message produced no alert at all")
	}
	if strings.Contains(got, "new MCP connection") {
		t.Errorf("the fallback prescribes reconnecting: %s", got)
	}
}

// ...and the dashboard must actually USE it. Testing blockedAlert alone leaves
// the call site free to go back to the fixed string — a mutant doing exactly
// that survived until this test existed.
func TestDashboardWorkspaceStateAlert_UsesTheSessionsBlockedMessage(t *testing.T) {
	m := Model{sessions: []session.Info{{
		Health:        "blocked",
		HealthMessage: "the home-containing workspace /Users was refused; call session_start({workspace: \"/Users\"})",
	}}}

	got := m.dashboardWorkspaceStateAlert()
	if strings.Contains(got, "new MCP connection") {
		t.Fatalf("the dashboard still prescribes reconnecting: %s", got)
	}
	if !strings.Contains(got, "session_start") {
		t.Errorf("the dashboard drops the session's own remedy: %s", got)
	}
}

// A long message is truncated rather than overrunning the single-line row.
func TestBlockedAlert_TruncatesALongMessage(t *testing.T) {
	got := blockedAlert(strings.Repeat("workspace boundary ", 40))
	if n := len([]rune(got)); n > maxBlockedAlertRunes {
		t.Errorf("alert is %d runes, want <= %d", n, maxBlockedAlertRunes)
	}
}
