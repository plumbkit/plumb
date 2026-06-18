package web

import (
	"encoding/json"
	"net/http"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/theme"
)

type themeDTO struct {
	Name    string        `json:"name"`
	Names   []string      `json:"names"`
	Palette theme.Palette `json:"palette"`
}

// handleThemeGet returns the active theme's palette (hex colours) plus the
// catalogue of available theme names. The SPA feeds the palette into CSS custom
// properties so the web UI tracks the selected plumb theme.
func (s *Server) handleThemeGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.themeState())
}

func (s *Server) themeState() themeDTO {
	name := s.deps.Store.Current().UI.Theme
	if name == "" {
		name = theme.Default
	}
	pal, ok := theme.Get(name)
	if !ok {
		name = theme.Default
	}
	return themeDTO{Name: name, Names: theme.Names(), Palette: pal}
}

type themeSetRequest struct {
	Name string `json:"name"`
}

// handleThemeSet switches the active theme. It persists via config.SaveTheme and
// reloads the store so the change is live, then returns the new palette.
func (s *Server) handleThemeSet(w http.ResponseWriter, r *http.Request) {
	var req themeSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := theme.Get(req.Name); !ok {
		writeError(w, http.StatusBadRequest, "unknown theme: "+req.Name)
		return
	}
	if err := config.SaveTheme(req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "saving theme: "+err.Error())
		return
	}
	if err := s.deps.Store.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, "reloading config: "+err.Error())
		return
	}
	writeJSON(w, s.themeState())
}
