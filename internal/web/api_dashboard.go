package web

import (
	"net/http"
	"time"

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
