package web

import (
	"net/http"
	"time"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/stats"
)

// fullToolStatDTO carries the per-tool figures the charts need (latency
// boxplot/bubble scatter, savings split): calls, latency, errors, token axes.
// TokensSaved/CapabilityTokens/EfficiencyTokens are netted to statsDTO's
// ModelVersion (see handleStats) — Calls/AvgMs/P95Ms/Errors/bytes are not,
// and reflect every row matching the request's filter regardless of which
// savings-model version scored it.
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
	// ModelVersion is the savings-model version (clientcaps.ModelVersion) the
	// TokensSaved/CapabilityTokens/EfficiencyTokens fields above are netted
	// to — PLAN-367 review round 2: a consumer must never plot this figure
	// next to one fetched before a model-version bump as though they were the
	// same measurement (the live DB has calls scored under earlier versions
	// that credited a different, since-retired counterfactual).
	ModelVersion int `json:"modelVersion"`
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
	out := statsDTO{Tools: []fullToolStatDTO{}, ModelVersion: clientcaps.ModelVersion}
	if db == nil { // database not created yet — empty, not an error
		writeJSON(w, out)
		return
	}

	// Netted to the current savings-model version (PLAN-367 review round 2):
	// Calls/AvgMs/P95Ms/Errors/bytes below still reflect every row matching
	// filter, whatever version it was scored under, but the token axes are
	// scoped to ModelVersion only — see SummarySinceVersion's doc comment.
	rows, err := db.SummarySinceVersion(filter, clientcaps.ModelVersion)
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
