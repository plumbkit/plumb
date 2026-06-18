package web

import (
	"net/http"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/topology"
)

type topologyDTO struct {
	Workspace    string    `json:"workspace"`
	Available    bool      `json:"available"`
	IndexedFiles int       `json:"indexedFiles"`
	SkippedFiles int       `json:"skippedFiles"`
	EmptyFiles   int       `json:"emptyFiles"`
	TotalNodes   int       `json:"totalNodes"`
	TotalEdges   int       `json:"totalEdges"`
	DBSizeBytes  int64     `json:"dbSizeBytes"`
	LastSync     time.Time `json:"lastSync"`
	IndexerState string    `json:"indexerState"`
	Languages    []string  `json:"languages"`
	LastError    string    `json:"lastError"`
}

// handleTopology returns the topology index status for the requested (or
// default) workspace. A missing index is reported as available=false, not an
// error, so the SPA can render an empty state.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	ws := resolveWorkspace(r.URL.Query().Get("workspace"))
	out := topologyDTO{Workspace: ws}
	if ws == "" {
		writeJSON(w, out)
		return
	}

	st, err := topology.StatusForWorkspace(ws)
	if err != nil {
		writeJSON(w, out) // no index yet
		return
	}
	out.Available = true
	out.IndexedFiles = st.IndexedFiles
	out.SkippedFiles = st.SkippedFiles
	out.EmptyFiles = st.EmptyFiles
	out.TotalNodes = st.TotalNodes
	out.TotalEdges = st.TotalEdges
	out.DBSizeBytes = st.DBSizeBytes
	out.LastSync = st.LastSync
	out.IndexerState = st.IndexerState
	out.Languages = st.Languages
	out.LastError = st.LastError
	writeJSON(w, out)
}

// resolveWorkspace picks the workspace to report on: the explicit query value
// if non-empty, else the folder of the first active session.
func resolveWorkspace(explicit string) string {
	if explicit != "" {
		return explicit
	}
	infos, err := session.List()
	if err != nil {
		return ""
	}
	for _, info := range infos {
		if info.EndedAt.IsZero() && info.Folder != "" {
			return info.Folder
		}
	}
	return ""
}
