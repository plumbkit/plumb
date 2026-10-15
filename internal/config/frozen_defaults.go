package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// FindFrozenDefaults scans raw TOML config bytes for keys that were explicitly
// written into the file and whose values match the compiled-in defaults that
// pre-PLAN-382 whole-struct serialization froze into user configs (e.g. empty
// command list, default protected branches, default quality analysers, empty
// extra/read roots, empty exclude patterns, empty lsp args).
//
// Keys returned are dotted TOML paths, sorted alphabetically.
func FindFrozenDefaults(rawTOML []byte) []string {
	var m map[string]any
	if err := toml.Unmarshal(rawTOML, &m); err != nil || len(m) == 0 {
		return nil
	}

	def := Defaults()
	frozen := make([]string, 0, 16)
	frozen = append(frozen, checkCommand(m)...)
	frozen = append(frozen, checkGit(m, def)...)
	frozen = append(frozen, checkQuality(m, def)...)
	frozen = append(frozen, checkTopology(m, def)...)
	frozen = append(frozen, checkWorkspace(m, def)...)
	frozen = append(frozen, checkLSP(m, def)...)

	sort.Strings(frozen)
	return frozen
}

func checkCommand(m map[string]any) []string {
	v, ok := m["command"]
	if !ok {
		return nil
	}
	if s, ok := v.([]any); ok && len(s) == 0 {
		return []string{"command"}
	}
	return nil
}

func checkGit(m map[string]any, def Config) []string {
	gitTable, ok := m["git"].(map[string]any)
	if !ok {
		return nil
	}
	if v, ok := gitTable["protected_branches"]; ok {
		if matchesStringSlice(v, def.Git.ProtectedBranches) {
			return []string{"git.protected_branches"}
		}
	}
	return nil
}

func checkQuality(m map[string]any, def Config) []string {
	qTable, ok := m["quality"].(map[string]any)
	if !ok {
		return nil
	}
	if v, ok := qTable["analysers"]; ok {
		if matchesStringSlice(v, def.Quality.Analysers) {
			return []string{"quality.analysers"}
		}
	}
	return nil
}

func checkTopology(m map[string]any, def Config) []string {
	topTable, ok := m["topology"].(map[string]any)
	if !ok {
		return nil
	}
	if v, ok := topTable["exclude_patterns"]; ok {
		if s, ok := v.([]any); ok && len(s) == 0 && len(def.Topology.ExcludePatterns) == 0 {
			return []string{"topology.exclude_patterns"}
		}
	}
	return nil
}

func checkWorkspace(m map[string]any, def Config) []string {
	wsTable, ok := m["workspace"].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	if v, ok := wsTable["extra_roots"]; ok {
		if s, ok := v.([]any); ok && len(s) == 0 && len(def.Workspace.ExtraRoots) == 0 {
			out = append(out, "workspace.extra_roots")
		}
	}
	if v, ok := wsTable["read_roots"]; ok {
		if s, ok := v.([]any); ok && len(s) == 0 && len(def.Workspace.ReadRoots) == 0 {
			out = append(out, "workspace.read_roots")
		}
	}
	return out
}

func checkLSP(m map[string]any, def Config) []string {
	lspTable, ok := m["lsp"].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for lang, serverVal := range lspTable {
		serverTable, ok := serverVal.(map[string]any)
		if !ok {
			continue
		}
		defServer, hasDef := def.LSP[lang]
		out = append(out, checkLSPServer(lang, serverTable, defServer, hasDef)...)
	}
	return out
}

func checkLSPServer(lang string, table map[string]any, def LSPConfig, hasDef bool) []string {
	var out []string
	if v, ok := table["args"]; ok {
		if s, ok := v.([]any); ok && len(s) == 0 && (!hasDef || len(def.Args) == 0) {
			out = append(out, "lsp."+lang+".args")
		}
	}
	if v, ok := table["root_markers"]; ok && hasDef {
		if matchesStringSlice(v, def.RootMarkers) {
			out = append(out, "lsp."+lang+".root_markers")
		}
	}
	if v, ok := table["weak_root_markers"]; ok && hasDef {
		if matchesStringSlice(v, def.WeakRootMarkers) {
			out = append(out, "lsp."+lang+".weak_root_markers")
		}
	}
	// A zero idle_timeout or max_workspaces was the compiled default before
	// PLAN-534, so the old whole-struct save wrote it into every [lsp.x] table it
	// touched — where it now pins hibernation or the cap off. A zero is frozen
	// only beside other frozen keys in the same table (that save's signature): on
	// its own it may well be a deliberate "off".
	if len(out) > 0 && hasDef {
		out = append(out, frozenZeroKnobs(lang, table, def)...)
	}
	return out
}

// frozenZeroKnobs names the zero idle_timeout / max_workspaces in a table the
// whole-struct save wrote, where the compiled default is now non-zero.
func frozenZeroKnobs(lang string, table map[string]any, def LSPConfig) []string {
	var out []string
	if v, ok := table["idle_timeout"]; ok && isZeroTOML(v) && def.IdleTimeout.Duration > 0 {
		out = append(out, "lsp."+lang+".idle_timeout")
	}
	if v, ok := table["max_workspaces"]; ok && isZeroTOML(v) && def.MaxWorkspaces > 0 {
		out = append(out, "lsp."+lang+".max_workspaces")
	}
	return out
}

// isZeroTOML reports whether a decoded TOML value is a zero duration or number.
func isZeroTOML(v any) bool {
	switch x := v.(type) {
	case string:
		d, err := time.ParseDuration(x)
		return x == "0" || (err == nil && d == 0)
	case int64:
		return x == 0
	case float64:
		return x == 0
	}
	return false
}

// PruneFrozenDefaults removes any keys detected as frozen defaults from rawTOML,
// returning the updated TOML bytes and the list of keys removed. Keys whose
// values differ from defaults are preserved.
func PruneFrozenDefaults(rawTOML []byte) ([]byte, []string, error) {
	var m map[string]any
	if err := toml.Unmarshal(rawTOML, &m); err != nil {
		return nil, nil, fmt.Errorf("parsing TOML for pruning: %w", err)
	}

	frozen := FindFrozenDefaults(rawTOML)
	if len(frozen) == 0 {
		return rawTOML, nil, nil
	}

	for _, keyPath := range frozen {
		parts := strings.Split(keyPath, ".")
		deleteNested(m, parts)
	}

	out, err := toml.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding pruned TOML: %w", err)
	}
	return out, frozen, nil
}

func matchesStringSlice(actual any, expected []string) bool {
	s, ok := actual.([]any)
	if !ok {
		return false
	}
	if len(s) != len(expected) {
		return false
	}
	for i, v := range s {
		str, ok := v.(string)
		if !ok || str != expected[i] {
			return false
		}
	}
	return true
}
