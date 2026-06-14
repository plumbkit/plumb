package tui

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestSettingsRegistryDrift is the guard that makes the config field registry
// the single source of truth for the Settings screen: every row must resolve to
// a registry field (matching reload tier and, for non-per-language rows, the
// stamped help text), and every general registry field must have a row. Adding
// a field to one side without the other fails here.
func TestSettingsRegistryDrift(t *testing.T) {
	items := buildSettingItems(config.Defaults())
	seen := map[string]bool{} // normalised (template) registry keys a row maps to

	for _, it := range items {
		key := dottedKeyFor(it.key, it.lspLang)
		f, ok := config.Lookup(key)
		if !ok {
			t.Errorf("settings row %q (lspLang=%q) → key %q not in the registry", it.label, it.lspLang, key)
			continue
		}
		seen[f.Key] = true
		if reloadTierFor(it.key) != f.ReloadTier {
			t.Errorf("row %q tier %v != registry tier %v", it.label, reloadTierFor(it.key), f.ReloadTier)
		}
		// Non-per-language rows are stamped verbatim from the registry; per-language
		// rows substitute <lang> (or carry a dynamic dormant message), so skip them.
		if it.lspLang == "" && it.help != f.Description {
			t.Errorf("row %q help %q != registry Description %q", it.label, it.help, f.Description)
		}
	}

	// Every general registry field must be reachable by a row. Per-language
	// families backed by a TUI editor (lsp.*) are covered via their template;
	// tasks.* is agent-writable only and has no Settings row by design.
	for _, f := range config.Registry() {
		if strings.HasPrefix(f.Key, "tasks.") {
			continue
		}
		if !seen[f.Key] {
			t.Errorf("registry field %q has no Settings row", f.Key)
		}
	}
}

// TestSettingDottedKeys_MatchTOMLPaths keeps the registry-key map and the
// project-override path map from drifting: where a key is project-overridable,
// its dotted key must equal the joined TOML path.
func TestSettingDottedKeys_MatchTOMLPaths(t *testing.T) {
	for key, path := range settingTOMLPaths {
		dotted, ok := settingDottedKeys[key]
		if !ok {
			t.Errorf("settingKey %v has a TOML path but no dotted key", key)
			continue
		}
		if joined := strings.Join(path, "."); joined != dotted {
			t.Errorf("settingKey %v: TOML path %q != dotted key %q", key, joined, dotted)
		}
	}
}
