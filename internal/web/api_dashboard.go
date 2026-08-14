package web

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// dashboardDTO is the dashboard snapshot the SPA renders: KPI cards, daemon
// vitals, the activity calendar, top tools, and the token-savings split.
type dashboardDTO struct {
	UptimeSeconds float64           `json:"uptimeSeconds"`
	StartedAt     time.Time         `json:"startedAt"`
	Sessions      int               `json:"sessions"`
	TotalCalls    int64             `json:"totalCalls"`
	Metrics       metricsDTO        `json:"metrics"`
	TopTools      []toolStatDTO     `json:"topTools"`
	Activity      activityDTO       `json:"activity"`
	Savings       savingsDTO        `json:"savings"`
	Conversations []conversationDTO `json:"conversations"`
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
}

type savingsToolDTO struct {
	Tool       string `json:"tool"`
	Capability int64  `json:"capability"`
	Efficiency int64  `json:"efficiency"`
}

type conversationDTO struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	Notes     int       `json:"notes"`
	Pending   int       `json:"pending"`
	LastAt    time.Time `json:"lastAt"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	out := dashboardDTO{StartedAt: s.deps.StartedAt}
	if !s.deps.StartedAt.IsZero() {
		out.UptimeSeconds = time.Since(s.deps.StartedAt).Seconds()
	}

	out.Metrics = readMetricsDTO(s.deps.MetricsPath)

	if sessions, err := session.List(); err == nil {
		out.Sessions = len(sessions)
		out.Conversations = activeConversationVolumes(sessions, 10, s.deps.Store.Current())
	}

	db, err := stats.SharedReadOnly()
	if err == nil && db != nil {
		// One full-table aggregation feeds both the top-tools list and the
		// savings breakdown; running Summary twice per request (and per SSE
		// refresh) doubled the stats-DB scan for no benefit (#64).
		rows, sumErr := db.Summary(stats.Filter{})
		if sumErr == nil {
			out.TopTools, out.TotalCalls = topTools(rows, 10)
			out.Savings = savingsBreakdown(db, rows)
		}
		out.Activity = activityWindow(db, 24*time.Hour, 48)
	}

	writeJSON(w, out)
}

func activeConversationVolumes(sessions []session.Info, limit int, base config.Config) []conversationDTO {
	var global *collab.Store
	if anyWorkspaceAllowsCrossProject(sessions, base) && collab.GlobalExists() {
		global, _ = collab.OpenGlobal()
		if global != nil {
			defer global.Close()
		}
	}
	return activeConversationVolumesWithGlobal(sessions, limit, global, base)
}

func anyWorkspaceAllowsCrossProject(sessions []session.Info, base config.Config) bool {
	for _, info := range sessions {
		if projectAllowsCrossProject(base, filepath.Clean(info.Folder)) {
			return true
		}
	}
	return false
}

func projectAllowsCrossProject(base config.Config, workspace string) bool {
	resolved, err := config.LoadProject(base, workspace)
	return err == nil && resolved.Collab.CrossProject
}

func activeConversationVolumesWithGlobal(
	sessions []session.Info,
	limit int,
	global *collab.Store,
	base config.Config,
) []conversationDTO {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	now := time.Now()
	seenWorkspace := make(map[string]bool)
	seenCrossProject := make(map[string]bool)
	var out []conversationDTO
	for _, info := range sessions {
		workspace := filepath.Clean(info.Folder)
		if workspace == "." || seenWorkspace[workspace] {
			continue
		}
		seenWorkspace[workspace] = true
		out = append(out, localConversationVolumes(ctx, workspace, now, limit)...)
		if projectAllowsCrossProject(base, workspace) {
			out = append(out, crossProjectConversationVolumes(
				ctx, global, workspace, now, limit, seenCrossProject,
			)...)
		}
	}
	out = mergeConversationVolumes(out)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Notes != out[j].Notes {
			return out[i].Notes > out[j].Notes
		}
		return out[i].LastAt.After(out[j].LastAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func mergeConversationVolumes(rows []conversationDTO) []conversationDTO {
	merged := make(map[string]conversationDTO, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		prior, ok := merged[row.ID]
		if !ok {
			merged[row.ID] = row
			order = append(order, row.ID)
			continue
		}
		prior.Notes += row.Notes
		prior.Pending += row.Pending
		if row.LastAt.After(prior.LastAt) {
			prior.LastAt = row.LastAt
		}
		if prior.Workspace != row.Workspace {
			prior.Workspace = "cross-project"
		}
		merged[row.ID] = prior
	}
	out := make([]conversationDTO, 0, len(order))
	for _, id := range order {
		out = append(out, merged[id])
	}
	return out
}

func localConversationVolumes(
	ctx context.Context,
	workspace string,
	now time.Time,
	limit int,
) []conversationDTO {
	if !collab.Exists(workspace) {
		return nil
	}
	store, err := collab.Open(workspace)
	if err != nil {
		return nil
	}
	defer store.Close()
	summaries, err := store.ConversationSummaries(ctx, now, limit)
	if err != nil {
		return nil
	}
	out := make([]conversationDTO, 0, len(summaries))
	for _, summary := range summaries {
		out = append(out, conversationDTO{
			ID: summary.ID, Workspace: filepath.Base(workspace),
			Notes: summary.Notes, Pending: summary.Pending, LastAt: summary.LastAt,
		})
	}
	return out
}

func crossProjectConversationVolumes(
	ctx context.Context,
	global *collab.Store,
	workspace string,
	now time.Time,
	limit int,
	seen map[string]bool,
) []conversationDTO {
	if global == nil {
		return nil
	}
	summaries, err := global.ConversationSummariesForWorkspace(ctx, workspace, now, limit)
	if err != nil {
		return nil
	}
	var out []conversationDTO
	for _, summary := range summaries {
		if seen[summary.ID] {
			continue
		}
		seen[summary.ID] = true
		out = append(out, conversationDTO{
			ID: summary.ID, Workspace: "cross-project",
			Notes: summary.Notes, Pending: summary.Pending, LastAt: summary.LastAt,
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
// per-tool savings from the precomputed Summary slice.
func savingsBreakdown(db *stats.DB, rows []stats.ToolStat) savingsDTO {
	axes := db.SavingsAxes(stats.Filter{})
	out := savingsDTO{Capability: axes.Capability, Efficiency: axes.Efficiency}
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
