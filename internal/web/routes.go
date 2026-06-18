package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// routes builds the ServeMux: the JSON/SSE API under /api, then the SPA for
// everything else. The whole tree is wrapped in authMiddleware so no path is
// reachable without the token.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Read-only snapshots.
	mux.HandleFunc("GET /api/dashboard", s.handleDashboard)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/topology", s.handleTopology)
	mux.HandleFunc("GET /api/memory", s.handleMemoryList)
	mux.HandleFunc("GET /api/memory/{name}", s.handleMemoryRead)
	mux.HandleFunc("GET /api/theme", s.handleThemeGet)
	mux.HandleFunc("GET /api/settings", s.handleSettings)

	// Live push streams (SSE).
	mux.HandleFunc("GET /api/stream/metrics", s.handleMetricsStream)
	mux.HandleFunc("GET /api/stream/logs", s.handleLogsStream)

	// Write paths (config + theme only — bounded, like the TUI).
	mux.HandleFunc("POST /api/theme", s.handleThemeSet)
	mux.HandleFunc("POST /api/settings", s.handleSettingsSet)

	// SPA — must be last (matches "/").
	mux.Handle("/", spaHandler())

	return s.authMiddleware(mux)
}

// writeJSON encodes v as JSON with a 200 status. On encode failure it logs and
// emits a 500, since headers may already be partly written.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("web: encoding response", "err", err)
	}
}

// writeError emits a JSON error body with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
