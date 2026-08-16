package tui

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// The real messages every markBoundaryViolation caller writes. Kept verbatim
// (and long) on purpose: an earlier version of this test used a short stand-in
// and so missed that a 160-rune cap chopped the remedy off the two longest —
// the boundary violation and the #318 wide claim, the latter losing the
// `force: true` clause the alert exists to deliver.
const (
	boundaryMsg = "workspace boundary violation: this connection is pinned to /Users/dev/proj; " +
		"/Users/dev/.ssh/id_ed25519 is in a different project. To work there, call session_start " +
		"with workspace set to that project's root — it will re-pin this connection (if the re-pin " +
		"is refused because an explicit session_start pin already holds this connection, retry with force: true)."
	wideClaimMsg = "the home-containing workspace /Users was claimed over the initialize _meta channel " +
		"and refused, and no persisted pin restored it; call session_start({workspace: \"/Users\"}) to " +
		"declare it deliberately (add force: true if this connection already holds another explicit pin) — issue #318"
)

// The blocked alert must carry the session's OWN remedy, in full.
//
// "blocked" covers causes with different fixes — a boundary violation, a
// refused sticky re-pin (issue #182), and a refused wide workspace claim
// (issue #318). The alert used to prescribe "start a new MCP connection" for
// all of them; for the #318 case that advice LOOPS, because a fresh connection
// replays the same pin and is refused again.
func TestBlockedAlert_CarriesEachCausesRemedyInFull(t *testing.T) {
	for _, tc := range []struct {
		name, msg, mustContain string
	}{
		{"boundary violation", boundaryMsg, "force: true)."},
		{"wide claim (#318)", wideClaimMsg, "issue #318"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := blockedAlert(tc.msg)
			if strings.Contains(got, "new MCP connection") {
				t.Fatalf("the alert prescribes reconnecting, which loops for a refused wide claim: %s", got)
			}
			// The tail is what a cap would eat, and the tail is where the remedy is.
			if !strings.HasSuffix(got, tc.mustContain) {
				t.Errorf("the alert is truncated before its remedy (want it to end %q): %s", tc.mustContain, got)
			}
		})
	}
}

// ...and the dashboard must actually USE it. Testing blockedAlert alone leaves
// the call site free to go back to the fixed string — a mutant doing exactly
// that survived until this test existed.
func TestDashboardWorkspaceStateAlert_UsesTheSessionsBlockedMessage(t *testing.T) {
	m := Model{sessions: []session.Info{{Health: "blocked", HealthMessage: wideClaimMsg}}}

	got := m.dashboardWorkspaceStateAlert()
	if strings.Contains(got, "new MCP connection") {
		t.Fatalf("the dashboard still prescribes reconnecting: %s", got)
	}
	if !strings.Contains(got, `session_start({workspace: "/Users"})`) {
		t.Errorf("the dashboard drops the session's own remedy: %s", got)
	}
}

// HealthMessage embeds a client-supplied path, and ESC is a legal path byte.
// This text is rendered on the always-visible dashboard, so an escape sequence
// could corrupt the frame or spoof an adjacent alert row.
func TestBlockedAlert_StripsTerminalEscapes(t *testing.T) {
	got := blockedAlert("boundary violation: /tmp/\x1b[31mPWNED\x1b[0m\x1b[2J/x.go is elsewhere")
	if strings.Contains(got, "\x1b") {
		t.Errorf("the alert passes a terminal escape through to the dashboard: %q", got)
	}
	if !strings.Contains(got, "PWNED") {
		t.Errorf("stripping escapes should keep the visible text: %q", got)
	}
}

// A mark with no message still says something useful, and still does not
// prescribe the reconnect. Whitespace-only counts as none: without the trim,
// wrapText returns no lines and the blocked session vanishes from the dashboard.
func TestBlockedAlert_FallsBackWithoutAMessage(t *testing.T) {
	for _, in := range []string{"", "   ", "\x1b[0m"} {
		got := blockedAlert(in)
		if strings.TrimSpace(got) == "" {
			t.Fatalf("input %q produced no alert at all — the blocked session would disappear", in)
		}
		if strings.Contains(got, "new MCP connection") {
			t.Errorf("the fallback prescribes reconnecting: %s", got)
		}
	}
}
