package web

import (
	"net/http"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/topology"
)

type topologyDTO struct {
	Workspace    string         `json:"workspace"`
	Available    bool           `json:"available"`
	IndexedFiles int            `json:"indexedFiles"`
	SkippedFiles int            `json:"skippedFiles"`
	EmptyFiles   int            `json:"emptyFiles"`
	TotalNodes   int            `json:"totalNodes"`
	TotalEdges   int            `json:"totalEdges"`
	DBSizeBytes  int64          `json:"dbSizeBytes"`
	LastSync     time.Time      `json:"lastSync"`
	IndexerState string         `json:"indexerState"`
	Languages    []string       `json:"languages"`
	LastError    string         `json:"lastError"`
	FileErrors   []fileErrorDTO `json:"fileErrors"`
}

// fileErrorDTO is one skipped file and the reason the indexer recorded.
type fileErrorDTO struct {
	Path    string `json:"path"`
	Message string `json:"message"`
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
	writeJSON(w, topologyDTOFromStatus(ws, st))
}

// topologyDTOFromStatus maps a topology.Status into the wire DTO. Kept pure so
// the JSON shape is testable without standing up a database or a server.
func topologyDTOFromStatus(ws string, st topology.Status) topologyDTO {
	out := topologyDTO{
		Workspace:    ws,
		Available:    true,
		IndexedFiles: st.IndexedFiles,
		SkippedFiles: st.SkippedFiles,
		EmptyFiles:   st.EmptyFiles,
		TotalNodes:   st.TotalNodes,
		TotalEdges:   st.TotalEdges,
		DBSizeBytes:  st.DBSizeBytes,
		LastSync:     st.LastSync,
		IndexerState: st.IndexerState,
		Languages:    st.Languages,
		LastError:    st.LastError,
	}
	for _, fe := range st.FileErrors {
		out.FileErrors = append(out.FileErrors, fileErrorDTO{Path: fe.Path, Message: fe.Message})
	}
	return out
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
