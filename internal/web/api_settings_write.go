package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
)

type settingsSetRequest struct {
	Scope string `json:"scope"` // "global" or a workspace folder
	Key   string `json:"key"`   // dotted config key
	Value any    `json:"value"`
}

// handleSettingsSet writes a single setting at the requested scope. Global scope
// writes the global config and reloads the store; a workspace scope writes a
// sparse override to that workspace's .plumb/config.toml. The value is validated
// and coerced against the field registry first.
func (s *Server) handleSettingsSet(w http.ResponseWriter, r *http.Request) {
	var req settingsSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Key == "" {
		writeError(w, http.StatusBadRequest, "key required")
		return
	}
	f, ok := config.Lookup(req.Key)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown setting: "+req.Key)
		return
	}
	// A redacted secret echoed back from the UI must never overwrite the stored
	// credential with the mask sentinel — treat it as an unchanged no-op.
	if f.Secret {
		if s, isStr := req.Value.(string); isStr && s == redactedSecret {
			writeJSON(w, map[string]any{"ok": true, "scope": req.Scope, "key": req.Key, "unchanged": true})
			return
		}
	}
	if err := config.ValidateKeyValue(req.Key, req.Value); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Scope == "" || req.Scope == "global" {
		s.writeGlobalSetting(w, req)
		return
	}
	s.writeProjectSetting(w, req)
}

func (s *Server) writeGlobalSetting(w http.ResponseWriter, req settingsSetRequest) {
	path := strings.Split(req.Key, ".")
	if err := config.SetGlobalValue(path, req.Value); err != nil {
		writeError(w, http.StatusInternalServerError, "saving setting: "+err.Error())
		return
	}
	if err := s.deps.Store.Reload(); err != nil {
		writeError(w, http.StatusInternalServerError, "reloading config: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "scope": "global", "key": req.Key})
}

func (s *Server) writeProjectSetting(w http.ResponseWriter, req settingsSetRequest) {
	if !projectOverridable(req.Key) {
		writeError(w, http.StatusBadRequest, "setting is global-only: "+req.Key)
		return
	}
	// The scope must be a currently-attached workspace. Without this an arbitrary
	// req.Scope would write a .plumb/config.toml to any path on disk.
	if !isActiveWorkspace(req.Scope) {
		writeError(w, http.StatusBadRequest, "unknown workspace scope: "+req.Scope)
		return
	}
	path := strings.Split(req.Key, ".")
	if err := config.SetProjectValue(req.Scope, path, req.Value); err != nil {
		writeError(w, http.StatusInternalServerError, "saving project setting: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "scope": req.Scope, "key": req.Key})
}

// isActiveWorkspace reports whether folder is one of the currently-attached
// workspaces, so a settings write cannot target an arbitrary on-disk path.
func isActiveWorkspace(folder string) bool {
	for _, ref := range activeWorkspaces() {
		if ref.Folder == folder {
			return true
		}
	}
	return false
}
