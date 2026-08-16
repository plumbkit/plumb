package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
)

// dashboard_collab.go renders the ONE daemon-wide view of agent-to-agent
// mailbox traffic: how many cross-project conversations are live, and how busy
// each one is. Every other collab surface (workspace_sessions' "conversation
// volume" section, internal/tools/workspace_sessions_collab.go) is scoped to
// one workspace, asking that workspace's own consent. A dashboard has no
// single workspace to be — it is watching the whole daemon — so there is no
// single recipient whose [collab] cross_project opt-in could stand in for
// everyone's. The rule applied here is unanimous consent instead: a
// conversation is shown only when EVERY workspace that has participated in it
// has independently opted in. "Any one participant consents" would let one
// project's opt-in expose another project's traffic without that project's
// consent; "no display at all" would make the feature pointless. See
// collab.(*Store).FilterDaemonWideConversations for the enforcement.
//
// The TUI is a separate process from the daemon (tui.Run talks to it only over
// a control socket for a handful of live commands — see model_core.go's
// ctrlPath), so unlike internal/cli's connSession it has no access to the
// daemon's in-memory collabPool. It reads collab-xproject.db directly, the
// same way it already reads stats.db and the session directory.

// dashCollabDisplayLimit bounds how many conversations the panel shows. This
// is a glance-able dashboard row, not a paginated listing.
const dashCollabDisplayLimit = 8

// dashCollabRefreshTimeout bounds the read so a slow or locked collab-xproject.db
// never stalls a dashboard poll.
const dashCollabRefreshTimeout = 2 * time.Second

// refreshDashboardCollab refreshes the daemon-wide conversation panel. Safe to
// call every poll: it never creates collab-xproject.db (GlobalExists is
// checked first) and opens the read-only handle at most once per TUI run.
func (m *Model) refreshDashboardCollab() {
	if m.dashCollabStore == nil {
		if !collab.GlobalExists() {
			m.dashCollabConversations = nil
			return
		}
		store, err := collab.OpenGlobalReadOnly()
		if err != nil {
			m.dashCollabConversations = nil
			return
		}
		m.dashCollabStore = store
	}

	ctx, cancel := context.WithTimeout(context.Background(), dashCollabRefreshTimeout)
	defer cancel()
	// settingsCfg is the global config snapshot loaded once at startup
	// (NewModel) — the same base LoadProject would fall back to for a
	// workspace with no project override of its own.
	allow := func(workspace string) bool {
		return config.TargetAllowsCrossProject(m.settingsCfg, workspace)
	}
	convs, err := m.dashCollabStore.FilterDaemonWideConversations(ctx, time.Now(), dashCollabDisplayLimit, allow)
	if err != nil {
		return
	}
	m.dashCollabConversations = convs
}

// dashCollabWidget renders the daemon-wide conversation panel, or nil when
// there is nothing to show — a niche, off-by-default feature earns screen
// space only once it has something to report, the same rule dashProjectWidget
// follows for a workspace with no recorded calls.
func (m Model) dashCollabWidget(width int) []string {
	if len(m.dashCollabConversations) == 0 {
		return nil
	}
	inner := width - 2
	now := time.Now()

	content := []string{
		"   " + MutedStyle.Render("Cross-project mailbox traffic — shown only where every "+
			"participating workspace opted in to [collab] cross_project."),
		"",
	}
	for _, c := range m.dashCollabConversations {
		content = append(content, dashCollabRow(c, now))
	}
	// dashBox pads every content line out to inner width itself (via
	// lipgloss.Width), so rows here need no manual padding.
	return dashBox(" Cross-Project Conversations ", inner, content)
}

func dashCollabRow(c collab.ConversationSummary, now time.Time) string {
	pending := ""
	if c.Pending > 0 {
		pending = fmt.Sprintf(", %d unread", c.Pending)
	}
	label := DashLabelStyle.Width(dashKW).Render(c.ID)
	detail := DetailStyle.Render(fmt.Sprintf("%d note(s)%s, last %s ago", c.Notes, pending, dashAgeString(now.Sub(c.LastAt))))
	return "   " + label + " " + detail
}

// dashAgeString renders a duration as a compact age string, matching the
// "5s"/"3m"/"2h" shape internal/tools' humaniseAge uses for the equivalent
// mailbox listing — kept local rather than shared across the Presentation/
// Application layer boundary for one three-line helper.
func dashAgeString(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	}
}
