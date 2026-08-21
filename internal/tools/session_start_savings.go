package tools

// session_start_savings.go — the session_start economics banner (PLAN-367),
// split out from session_start_sections.go so the file-size cap has room.

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/stats"
)

// writeSessionStats renders the tool-usage summary and, below it, three
// honest economics lines (PLAN-367) in place of the single "tokens saved"
// headline the daemon used to lead with: what plumb's own tool surface costs
// this client per request (surcharge), what the counterfactual read/tool
// model estimates it saved (netted of the ranged-read arithmetic a native
// tool reproduces on its own — see clientcaps.ModelVersion v4), and how many
// times a write guard actually caught something — a real count, not an
// estimate. None of the three is fabricated into a single net total: the
// surcharge is a per-request client-side cost the daemon cannot multiply by
// call volume, and the savings figure is scoped to the CURRENT model version
// only (older rows measured a different counterfactual — see
// stats.Filter.SavingsModelVersion).
func (t *SessionStart) writeSessionStats(sb *strings.Builder, ws string) {
	db, err := stats.SharedReadOnly()
	if err != nil || db == nil {
		return
	}
	toolStats, err := db.Summary(stats.Filter{Workspace: ws})
	if err != nil || len(toolStats) == 0 {
		return
	}
	sb.WriteString("## Most-used tools (this workspace)\n\n")
	limit := min(len(toolStats), 5)
	for _, s := range toolStats[:limit] {
		fmt.Fprintf(sb, "- %s: %d calls, avg %dms, p95 %dms\n", s.Tool, s.Calls, int64(s.AvgMs), s.P95Ms)
	}
	sb.WriteString("\n")

	if t.surchargeFn != nil {
		if tokens, n := t.surchargeFn(); n > 0 {
			fmt.Fprintf(sb, "profile surcharge: ~%s tokens of tool schemas served to this client per request (%d tools)\n",
				stats.FormatSavings(tokens), n)
		}
	}

	// Scoped to the CURRENT model version only — see ModelVersion's doc
	// comment and the Filter field it names. Summing across versions would
	// blend v4's honest exclusion with v3 rows that still credited a plain
	// ranged read, quietly overstating the total.
	axes := db.SavingsAxes(stats.Filter{Workspace: ws, SavingsModelVersion: clientcaps.ModelVersion})
	if axes.Total() > 0 {
		fmt.Fprintf(sb, "estimated read savings: ~%s tokens (heuristic, model v%d — ranged reads a native tool could also skip are EXCLUDED; since this version only)\n",
			stats.FormatSavings(int(axes.Total())), clientcaps.ModelVersion)
	}

	if n := db.PreventedIncidents(stats.Filter{Workspace: ws}); n > 0 {
		fmt.Fprintf(sb, "prevented incidents: %d (stale/unread-write and dirty-file guards that refused a call this workspace)\n", n)
	}
	sb.WriteString("\n")
}
