package web

import (
	"net/http"
	"time"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/session"
)

type memoryDTO struct {
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Confidence   string    `json:"confidence"`
	UserAuthored bool      `json:"userAuthored"`
	Paths        []string  `json:"paths"`
	SizeBytes    int64     `json:"sizeBytes"`
	ModTime      time.Time `json:"modTime"`
}

type memoryListDTO struct {
	Workspace  string         `json:"workspace"`
	Workspaces []workspaceRef `json:"workspaces"`
	Memories   []memoryDTO    `json:"memories"`
}

type workspaceRef struct {
	Folder string `json:"folder"`
	Name   string `json:"name"`
}

// handleMemoryList lists memories for the requested (or default) workspace, plus
// the set of active workspaces so the SPA can offer a workspace switcher.
func (s *Server) handleMemoryList(w http.ResponseWriter, r *http.Request) {
	ws, ok := resolveWorkspace(r.URL.Query().Get("workspace"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown workspace: "+r.URL.Query().Get("workspace"))
		return
	}
	out := memoryListDTO{Workspace: ws, Workspaces: activeWorkspaces(), Memories: []memoryDTO{}}
	if ws == "" {
		writeJSON(w, out)
		return
	}

	mems, err := memory.List(ws)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing memories: "+err.Error())
		return
	}
	for _, m := range mems {
		out.Memories = append(out.Memories, memoryDTO{
			Name: m.Name, Description: m.Description, Confidence: string(m.Confidence),
			UserAuthored: m.UserAuthored(), Paths: m.Paths,
			SizeBytes: m.SizeBytes, ModTime: m.ModTime,
		})
	}
	writeJSON(w, out)
}

// handleMemoryRead returns the full markdown body of one memory.
func (s *Server) handleMemoryRead(w http.ResponseWriter, r *http.Request) {
	ws, ok := resolveWorkspace(r.URL.Query().Get("workspace"))
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown workspace: "+r.URL.Query().Get("workspace"))
		return
	}
	name := r.PathValue("name")
	if ws == "" || name == "" {
		writeError(w, http.StatusBadRequest, "workspace and name required")
		return
	}
	body, err := memory.Read(ws, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "reading memory: "+err.Error())
		return
	}
	writeJSON(w, map[string]string{"name": name, "workspace": ws, "content": body})
}

// activeWorkspaces returns the distinct active session folders, de-duplicated.
func activeWorkspaces() []workspaceRef {
	infos, err := session.List()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []workspaceRef
	for _, info := range infos {
		if !info.EndedAt.IsZero() || info.Folder == "" || seen[info.Folder] {
			continue
		}
		seen[info.Folder] = true
		out = append(out, workspaceRef{Folder: info.Folder, Name: info.Name})
	}
	return out
}
