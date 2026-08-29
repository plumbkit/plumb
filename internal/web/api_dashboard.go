package web

import (
	"context"
	"net/http"
	"time"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// dashboardDTO is the dashboard snapshot the SPA renders: KPI cards, daemon
// vitals, the activity calendar, top tools, and the token-savings split.
type dashboardDTO struct {
	UptimeSeconds float64       `json:"uptimeSeconds"`
	StartedAt     time.Time     `json:"startedAt"`
	Sessions      int           `json:"sessions"`
	TotalCalls    int64         `json:"totalCalls"`
	Metrics       metricsDTO    `json:"metrics"`
	TopTools      []toolStatDTO `json:"topTools"`
	Activity      activityDTO   `json:"activity"`
	Savings       savingsDTO    `json:"savings"`
	// CrossProjectConversations is the daemon-wide agent-to-agent mailbox
	// view: live conversations shown only where EVERY participating
	// workspace has independently opted in to [collab] cross_project. See
	// daemonWideConversationsDTO. nil (omitted) when the feature has never
	// been used on this daemon.
	CrossProjectConversations []conversationDTO `json:"crossProjectConversations,omitempty"`
}

type metricsDTO struct {
	CPUPercent     float64 `json:"cpuPercent"`
	CPUAvailable   bool    `json:"cpuAvailable"`
	RSSBytes       uint64  `json:"rssBytes"`
	RSSAvailable   bool    `json:"rssAvailable"`
	HeapAllocBytes uint64  `json:"heapAllocBytes"`
	HeapInuseBytes uint64  `json:"heapInuseBytes"`
	HeapSysBytes   uint64  `json:"heapSysBytes"`
	NumGC          uint32  `json:"numGC"`
	Goroutines     int     `json:"goroutines"`
	PID            int     `json:"pid"`
}

type toolStatDTO struct {
	Tool        string  `json:"tool"`
	Calls       int64   `json:"calls"`
	AvgMs       float64 `json:"avgMs"`
	P95Ms       int64   `json:"p95Ms"`
	Errors      int64   `json:"errors"`
	TokensSaved int64   `json:"tokensSaved"`
}

type activityDTO struct {
	WindowHours float64 `json:"windowHours"`
	Calls       int64   `json:"calls"`
	Buckets     []int64 `json:"buckets"`
}

type savingsDTO struct {
	Capability int64            `json:"capability"`
	Efficiency int64            `json:"efficiency"`
	ByTool     []savingsToolDTO `json:"byTool"`
	// ModelVersion is the savings-model version (clientcaps.ModelVersion) these
	// totals are netted to — PLAN-367: a consumer of this JSON must never sum
	// it against a value fetched before a model-version bump and present the
	// result as one figure.
	ModelVersion int `json:"modelVersion"`
}

type savingsToolDTO struct {
	Tool       string `json:"tool"`
	Capability int64  `json:"capability"`
	Efficiency int64  `json:"efficiency"`
}

// conversationDTO is one daemon-wide mailbox conversation: enough to show
// volume and staleness, nothing that discloses a body or participant identity
// (matching ConversationSummary's own disclosure rule).
type conversationDTO struct {
	ID         string  `json:"id"`
	Notes      int     `json:"notes"`
	Pending    int     `json:"pending"`
	AgeSeconds float64 `json:"ageSeconds"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	out := dashboardDTO{StartedAt: s.deps.StartedAt}
	if !s.deps.StartedAt.IsZero() {
		out.UptimeSeconds = time.Since(s.deps.StartedAt).Seconds()
	}

	out.Metrics = readMetricsDTO(s.deps.MetricsPath)

	if sessions, err := session.List(); err == nil {
		out.Sessions = len(sessions)
	}

	db, err := stats.SharedReadOnly()
	if err == nil && db != nil {
		// Top-tools ranking wants EVERY call regardless of savings-model
		// version (it's a usage count, not an estimate); the savings breakdown
		// must NOT blend an earlier model's scoring into today's (PLAN-367
		// review round 1 — see stats.DB.SummarySinceVersion's doc comment). The
		// #64 single-scan optimisation this used to share no longer applies:
		// a version-netted aggregate is a genuinely different query, not the
		// same rows sliced two ways.
		rows, sumErr := db.Summary(stats.Filter{})
		if sumErr == nil {
			out.TopTools, out.TotalCalls = topTools(rows, 10)
		}
		versionedRows, vErr := db.SummarySinceVersion(stats.Filter{}, clientcaps.ModelVersion)
		if vErr == nil {
			out.Savings = savingsBreakdown(db, versionedRows)
		}
		out.Activity = activityWindow(db, 24*time.Hour, 48)
	}

	out.CrossProjectConversations = daemonWideConversationsDTO(r.Context(), s.deps)

	writeJSON(w, out)
}

// dashboardCollabLimit bounds how many conversations the dashboard shows —
// a glance-able panel, not a paginated listing.
const dashboardCollabLimit = 8

// daemonWideConversationsDTO renders the daemon-wide, consent-filtered
// conversation list: a conversation appears only when EVERY workspace that
// has participated in it has independently opted in to [collab]
// cross_project. There is no single recipient to ask on a daemon-wide
// dashboard — unlike workspace_sessions' per-workspace "conversation volume"
// section — so consent must be unanimous rather than any-one's; see
// collab.(*Store).FilterDaemonWideConversations for the full reasoning.
//
// Read-only: CollabGlobalStore is the if-exists accessor, so a daemon that
// has never carried cross-project traffic is never the reason
// collab-xproject.db gets created.
func daemonWideConversationsDTO(ctx context.Context, deps Deps) []conversationDTO {
	if deps.CollabGlobalStore == nil {
		return nil
	}
	g := deps.CollabGlobalStore()
	if g == nil {
		return nil
	}
	base := deps.Store.Current()
	allow := func(workspace string) bool {
		return config.TargetAllowsCrossProject(base, workspace)
	}
	now := time.Now()
	convs, err := g.FilterDaemonWideConversations(ctx, now, dashboardCollabLimit, allow)
	if err != nil {
		return nil
	}
	out := make([]conversationDTO, 0, len(convs))
	for _, c := range convs {
		out = append(out, conversationDTO{
			ID: c.ID, Notes: c.Notes, Pending: c.Pending,
			AgeSeconds: now.Sub(c.LastAt).Seconds(),
		})
	}
	return out
}

func readMetricsDTO(path string) metricsDTO {
	m, err := monitor.ReadSnapshot(path)
	if err != nil {
		return metricsDTO{}
	}
	return metricsDTO{
		CPUPercent: m.CPUPercent, CPUAvailable: m.CPUAvailable,
		RSSBytes: m.RSSBytes, RSSAvailable: m.RSSAvailable,
		HeapAllocBytes: m.HeapAllocBytes, HeapInuseBytes: m.HeapInuseBytes,
		HeapSysBytes: m.HeapSysBytes, NumGC: m.NumGC,
		Goroutines: m.Goroutines, PID: m.PID,
	}
}

// topTools returns the n busiest tools and the total call count across all,
// from a precomputed Summary slice.
func topTools(rows []stats.ToolStat, n int) ([]toolStatDTO, int64) {
	var total int64
	out := make([]toolStatDTO, 0, n)
	for i, t := range rows {
		total += t.Calls
		if i < n {
			out = append(out, toolStatDTO{
				Tool: t.Tool, Calls: t.Calls, AvgMs: t.AvgMs, P95Ms: t.P95Ms,
				Errors: t.Errors, TokensSaved: t.TokensSaved,
			})
		}
	}
	return out, total
}

func activityWindow(db *stats.DB, window time.Duration, buckets int) activityDTO {
	a, err := db.Activity(window, buckets, stats.Filter{})
	if err != nil {
		return activityDTO{WindowHours: window.Hours()}
	}
	return activityDTO{WindowHours: window.Hours(), Calls: a.Calls, Buckets: a.Buckets}
}

// savingsBreakdown splits token savings by axis (a cheap aggregate) and lists
// per-tool savings from the precomputed, version-netted Summary slice (see
// handleDashboard). Both the axis total and the per-tool rows are netted to
// the SAME savings-model version, so they can never contradict each other.
func savingsBreakdown(db *stats.DB, rows []stats.ToolStat) savingsDTO {
	axes := db.SavingsAxes(stats.Filter{SavingsModelVersion: clientcaps.ModelVersion})
	out := savingsDTO{Capability: axes.Capability, Efficiency: axes.Efficiency, ModelVersion: clientcaps.ModelVersion}
	for _, t := range rows {
		if t.CapabilityTokens == 0 && t.EfficiencyTokens == 0 {
			continue
		}
		out.ByTool = append(out.ByTool, savingsToolDTO{
			Tool: t.Tool, Capability: t.CapabilityTokens, Efficiency: t.EfficiencyTokens,
		})
	}
	return out
}
