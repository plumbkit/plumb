package web

import (
	"net/http"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
)

// fullToolStatDTO carries the per-tool figures the charts need (latency
// boxplot/bubble scatter, savings split): calls, latency, errors, token axes.
type fullToolStatDTO struct {
	Tool             string    `json:"tool"`
	Calls            int64     `json:"calls"`
	AvgMs            float64   `json:"avgMs"`
	P95Ms            int64     `json:"p95Ms"`
	Errors           int64     `json:"errors"`
	TokensSaved      int64     `json:"tokensSaved"`
	CapabilityTokens int64     `json:"capabilityTokens"`
	EfficiencyTokens int64     `json:"efficiencyTokens"`
	TotalInputKB     float64   `json:"totalInputKB"`
	TotalOutputKB    float64   `json:"totalOutputKB"`
	LastCalledAt     time.Time `json:"lastCalledAt"`
}

type statsDTO struct {
	Tools []fullToolStatDTO `json:"tools"`
}

// handleStats returns the full per-tool statistics table, optionally narrowed
// by ?workspace= or ?session=.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	filter := stats.Filter{
		Workspace:   r.URL.Query().Get("workspace"),
		SessionName: r.URL.Query().Get("session"),
	}

	db, err := stats.SharedReadOnly()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stats unavailable: "+err.Error())
		return
	}
	out := statsDTO{Tools: []fullToolStatDTO{}}
	if db == nil { // database not created yet — empty, not an error
		writeJSON(w, out)
		return
	}

	rows, err := db.Summary(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "querying stats: "+err.Error())
		return
	}
	for _, t := range rows {
		out.Tools = append(out.Tools, fullToolStatDTO{
			Tool: t.Tool, Calls: t.Calls, AvgMs: t.AvgMs, P95Ms: t.P95Ms,
			Errors: t.Errors, TokensSaved: t.TokensSaved,
			CapabilityTokens: t.CapabilityTokens, EfficiencyTokens: t.EfficiencyTokens,
			TotalInputKB: t.TotalInputKB, TotalOutputKB: t.TotalOutputKB,
			LastCalledAt: t.LastCalledAt,
		})
	}
	writeJSON(w, out)
}
