package web

import (
	"net/http"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/plumbkit/plumb/internal/config"
)

// settingsScopeDTO is the settings for one scope (Global or a workspace). Rows
// are the editable fields visible in that scope.
type settingsScopeDTO struct {
	Scope  string          `json:"scope"`  // "global" or the workspace folder
	Name   string          `json:"name"`   // display name ("Global" or session name)
	Global bool            `json:"global"` // true for the global scope
	Rows   []settingRowDTO `json:"rows"`
}

// settingRowDTO mirrors one config field for one scope: its resolved value,
// metadata, reload tier, and per-scope override status.
type settingRowDTO struct {
	Key        string   `json:"key"`
	Group      string   `json:"group"`   // top-level section, for grouping
	Type       string   `json:"type"`    // bool|int|duration|enum|string|list
	Value      any      `json:"value"`   // resolved value at this scope
	Options    []string `json:"options"` // for enum
	Help       string   `json:"help"`
	ReloadTier string   `json:"reloadTier"` // live|next-session|restart
	Overridden bool     `json:"overridden"` // workspace scope: project file sets this key
}

type settingsDTO struct {
	Scopes []settingsScopeDTO `json:"scopes"`
}

// globalOnlySections are the config sections that are daemon-global presentation
// or process-wide settings, never overridable per project. They appear only in
// the Global scope.
var globalOnlySections = map[string]bool{
	"ui": true, "web": true, "cache": true, "lsp_query": true,
	"session": true, "log_level": true, "log_format": true, "log_file": true,
	"agent_config_writes": true, "semantics": true, "lsp": true,
}

// handleSettings returns the per-scope settings model: the Global scope plus one
// scope per active workspace, each carrying the editable rows with resolved
// values and override flags.
func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	base := s.deps.Store.Current()
	out := settingsDTO{Scopes: []settingsScopeDTO{s.globalScope(base)}}
	for _, ref := range activeWorkspaces() {
		out.Scopes = append(out.Scopes, s.workspaceScope(base, ref))
	}
	writeJSON(w, out)
}

func (s *Server) globalScope(base config.Config) settingsScopeDTO {
	flat := flattenConfig(base)
	scope := settingsScopeDTO{Scope: "global", Name: "Global", Global: true}
	for _, f := range config.Registry() {
		if f.PerLanguage {
			continue
		}
		scope.Rows = append(scope.Rows, buildRow(f, flat[f.Key], false))
	}
	return scope
}

func (s *Server) workspaceScope(base config.Config, ref workspaceRef) settingsScopeDTO {
	merged, err := config.LoadProject(base, ref.Folder)
	if err != nil {
		merged = base
	}
	flat := flattenConfig(merged)
	raw, _ := config.LoadProjectRaw(ref.Folder)

	name := ref.Name
	if name == "" {
		name = ref.Folder
	}
	scope := settingsScopeDTO{Scope: ref.Folder, Name: name}
	for _, f := range config.Registry() {
		if f.PerLanguage || !projectOverridable(f.Key) {
			continue
		}
		path := strings.Split(f.Key, ".")
		scope.Rows = append(scope.Rows, buildRow(f, flat[f.Key], rawHasPath(raw, path)))
	}
	return scope
}

func buildRow(f config.Field, value any, overridden bool) settingRowDTO {
	if f.Secret {
		value = redactSecretValue(value)
	}
	return settingRowDTO{
		Key: f.Key, Group: topSection(f.Key), Type: f.Type.String(),
		Value: value, Options: config.EnumValues(f), Help: f.Description,
		ReloadTier: f.ReloadTier.String(), Overridden: overridden,
	}
}

// redactedSecret is the placeholder shown for a Secret field whose value is set.
// It must never reach stored config — handleSettingsSet treats an incoming value
// equal to it as "unchanged", so the mask cannot round-trip into the config file.
const redactedSecret = "••••••••" //nolint:gosec // G101 false positive: a fixed display mask, not a credential

// redactSecretValue masks a credential for display: a non-empty value becomes the
// redactedSecret sentinel (signalling "set" without leaking it); empty stays "".
func redactSecretValue(v any) any {
	if s, ok := v.(string); ok && s != "" {
		return redactedSecret
	}
	return ""
}

func topSection(key string) string {
	if i := strings.IndexByte(key, '.'); i >= 0 {
		return key[:i]
	}
	return key
}

func projectOverridable(key string) bool {
	return !globalOnlySections[topSection(key)]
}

// flattenConfig marshals a Config to TOML and re-decodes it into a flat map of
// dotted key → value, so a registry field can be resolved by its dotted key
// without per-field reflection. Durations encode as strings, matching the
// FieldDuration kind.
func flattenConfig(cfg config.Config) map[string]any {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return map[string]any{}
	}
	var nested map[string]any
	if err := toml.Unmarshal(data, &nested); err != nil {
		return map[string]any{}
	}
	flat := map[string]any{}
	flatten("", nested, flat)
	return flat
}

func flatten(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			flatten(key, child, out)
			continue
		}
		out[key] = v
	}
}

// rawHasPath reports whether the dotted path is present in a raw project config
// map (nested map[string]any from config.LoadProjectRaw).
func rawHasPath(m map[string]any, path []string) bool {
	if m == nil {
		return false
	}
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			return false
		}
		m = next
	}
	_, ok := m[path[len(path)-1]]
	return ok
}
