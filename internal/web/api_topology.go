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
	ws, ok := resolveWorkspace(r.URL.Query().Get("workspace"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown workspace: "+r.URL.Query().Get("workspace"))
		return
	}
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

// resolveWorkspace picks the workspace to report on: the explicit query value —
// validated to be a currently-attached workspace, so a read endpoint cannot be
// pointed at an arbitrary on-disk path's .plumb/ index or memories (the read-side
// counterpart to the settings-write isActiveWorkspace guard) — if non-empty, else
// the folder of the first active session. ok is false only when an explicit
// workspace was given that is not active; an empty explicit value defaults and is
// always ok.
func resolveWorkspace(explicit string) (ws string, ok bool) {
	if explicit != "" {
		if !isActiveWorkspace(explicit) {
			return "", false
		}
		return explicit, true
	}
	infos, err := session.List()
	if err != nil {
		return "", true
	}
	for _, info := range infos {
		if info.EndedAt.IsZero() && info.Folder != "" {
			return info.Folder, true
		}
	}
	return "", true
}
