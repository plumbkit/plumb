package tui

// dashboard_alerts_test.go — issue #358: the blocked-session alert must show
// the session's own remedy instead of a fixed string, must never corrupt the
// Alerts box no matter how long the underlying path is, must cap runaway
// messages without eating the remedy at their end, and must never leak a
// raw control byte to either renderer that shows HealthMessage.
//
// Test-fixture discipline (this is where PR #356's attempt 1 fooled itself):
// every fixture below is built from a REAL producer — tools.WorkspaceBoundaryError,
// the literal issue-#318 message shape from conn_attach_oninit.go, and (for the
// escape test) an actual session.Patch/session.List round trip — never a string
// compared against the constant under test.

import (
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// TestDashboardWorkspaceStateAlert_RendersTheSessionsOwnRemedy is change 2:
// a blocked session's HealthMessage — not the fixed "start a new MCP
// connection" string — is what the alert shows, because Health: blocked now
// covers causes (a boundary violation, the #182 sticky re-pin refusal, the
// #318 wide-claim refusal) with different remedies, and reconnecting loops
// for at least one of them.
func TestDashboardWorkspaceStateAlert_RendersTheSessionsOwnRemedy(t *testing.T) {
	boundary := tools.WorkspaceBoundaryError{Workspace: "/a/ws", Path: "/other/proj/f.go"}.Error()
	claim318 := "the home-containing workspace /Users/x was claimed over the initialize _meta channel and refused, and no persisted pin restored it; call session_start({workspace: \"/Users/x\"}) to declare it deliberately (add force: true if this connection already holds another explicit pin) — issue #318"

	for name, msg := range map[string]string{
		"boundary violation (tools.WorkspaceBoundaryError)": boundary,
		"issue #318 wide-claim refusal":                     claim318,
	} {
		t.Run(name, func(t *testing.T) {
			m := Model{sessions: []session.Info{{Health: "blocked", HealthMessage: msg}}}
			got := m.dashboardWorkspaceStateAlert()
			if got != msg {
				t.Fatalf("dashboardWorkspaceStateAlert = %q, want the session's own HealthMessage %q", got, msg)
			}
			if !strings.Contains(got, "session_start") {
				t.Errorf("rendered alert should still mention session_start, got %q", got)
			}
		})
	}
}

// TestDashboardWorkspaceStateAlert_FallsBackWhenHealthMessageEmpty keeps the
// old fixed string as a fallback for a "blocked" session that (for whatever
// reason) recorded no message — the fallback path must still exist.
func TestDashboardWorkspaceStateAlert_FallsBackWhenHealthMessageEmpty(t *testing.T) {
	m := Model{sessions: []session.Info{{Health: "blocked"}}}
	got := m.dashboardWorkspaceStateAlert()
	if !strings.Contains(got, "start a new MCP connection") {
		t.Fatalf("dashboardWorkspaceStateAlert with no HealthMessage = %q, want the fallback string", got)
	}
}

// TestDashAlertsWidget_RemedySurvivesRendering is the widget-level twin of the
// test above: the remedy substring must still be present in the RENDERED
// widget output, not just in dashboardWorkspaceStateAlert's return value —
// dashAlertsWidget wraps and (before change 3) could corrupt a long line, and
// a helper-level assertion alone would not have caught that.
func TestDashAlertsWidget_RemedySurvivesRendering(t *testing.T) {
	msg := tools.WorkspaceBoundaryError{Workspace: "/a/ws", Path: "/other/proj/f.go"}.Error()
	m := Model{sessions: []session.Info{{Health: "blocked", HealthMessage: msg}}}
	rendered := strings.Join(m.dashAlertsWidget(100), "\n")
	plain := ansi.Strip(rendered)
	for _, want := range []string{"session_start", "force: true"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered Alerts widget missing %q\n%s", want, plain)
		}
	}
}

// TestDashAlertsWidget_LongPathNeverExceedsWidth is change 3's widget-level
// assertion — the one attempt 2 (removing the truncation cap entirely) would
// have failed. An unbreakable token (a client-controlled path has no upper
// bound) must never make a rendered line wider than the box: dashBox pads
// each content line by lipgloss.Width, so an over-wide line gets NO padding
// and the box border on the following rows is lost.
func TestDashAlertsWidget_LongPathNeverExceedsWidth(t *testing.T) {
	longPath := "/Users/agent/" + strings.Repeat("very-long-unbreakable-directory-segment-", 20) + "leaf.go"
	msg := tools.WorkspaceBoundaryError{Workspace: "/a/ws", Path: longPath}.Error()
	if len([]rune(msg)) < 600 {
		t.Fatalf("fixture too short to exercise the bug: %d runes", len([]rune(msg)))
	}
	m := Model{sessions: []session.Info{{Health: "blocked", HealthMessage: msg}}}
	const width = 100
	for i, line := range m.dashAlertsWidget(width) {
		plain := ansi.Strip(line)
		// dashAlertsWidget returns full box rows (border included), so the
		// legal width here is the widget's own width, not the inner text
		// column — every returned row must still total to the SAME width, or
		// the box's borders no longer line up (the "4 body rows losing their
		// left border" symptom from the issue).
		w := lipgloss.Width(plain)
		if w != width {
			t.Errorf("line %d width = %d, want exactly %d (box misaligned)\n%q", i, w, width, plain)
		}
	}
}

// TestCapAlertLines_KeepsFinalSentence is change 4: eliding a runaway message
// in the middle (first lines, one "…" line, last two) must still surface its
// final sentence — the remedy sits at the END of a boundary-violation
// message, which is precisely what attempt 1's tail truncation destroyed.
func TestCapAlertLines_KeepsFinalSentence(t *testing.T) {
	words := make([]string, 0, 200)
	for i := range 200 {
		words = append(words, "filler"+strconv.Itoa(i))
	}
	msg := strings.Join(words, " ") + " call session_start again with force: true to finish"
	m := Model{sessions: []session.Info{{Health: "blocked", HealthMessage: msg}}}
	rendered := ansi.Strip(strings.Join(m.dashAlertsWidget(60), "\n"))
	if !strings.Contains(rendered, "force: true to finish") {
		t.Fatalf("capped Alerts widget lost the final sentence:\n%s", rendered)
	}
	if !strings.Contains(rendered, "…") {
		t.Fatalf("capped Alerts widget should show an elision marker for a message this long:\n%s", rendered)
	}
}

// TestBlockedSessionRendersEscapeFree_BothPanes is change 5's cross-cutting
// assertion — the one that catches "fixed in one of two". A prior attempt
// stripped escapes inside the dashboard alert renderer only, so the same
// HealthMessage still reached the terminal raw through the session detail
// pane (model_right.go). The fixture is a REAL session.Patch/session.List
// round trip (internal/session's write-boundary sanitisation, change 5), not
// a hand-built session.Info — so this test would fail if that boundary fix
// were reverted just as surely as if only one TUI renderer were patched.
func TestBlockedSessionRendersEscapeFree_BothPanes(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id, err := registerSessionForTest(session.Info{Folder: "/tmp/proj"})
	if err != nil {
		t.Fatalf("session.Register: %v", err)
	}
	defer session.Unregister(id)

	const dirty = "workspace boundary violation: /tmp/\x1b[31mevil\x1b[0m is in a different project"
	session.Patch(id, func(in *session.Info) {
		in.Health = "blocked"
		in.HealthMessage = dirty
	})

	infos, err := session.List()
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	var info session.Info
	found := false
	for _, i := range infos {
		if i.ID == id {
			info, found = i, true
		}
	}
	if !found {
		t.Fatalf("session %s not found", id)
	}
	if strings.Contains(info.HealthMessage, "\x1b") {
		t.Fatalf("precondition: session.List returned an unsanitised HealthMessage: %q", info.HealthMessage)
	}

	m := Model{sessions: []session.Info{info}, cursor: 0}
	dashboard := strings.Join(m.dashAlertsWidget(100), "\n")
	detail := strings.Join(m.rightLinesDetails(80), "\n")
	if strings.Contains(dashboard, "\x1b[31m") {
		t.Errorf("dashboard alert rendered a raw escape sequence:\n%s", dashboard)
	}
	if strings.Contains(detail, "\x1b[31m") {
		t.Errorf("session detail pane rendered a raw escape sequence:\n%s", detail)
	}
}

// registerSessionForTest is a tiny local wrapper mirroring internal/session's
// own registerID test helper, kept package-local since this test lives in tui,
// not session.
func registerSessionForTest(info session.Info) (string, error) {
	reg, err := session.Register(info)
	return reg.ID, err
}
