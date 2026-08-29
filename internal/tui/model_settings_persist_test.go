package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// neverSetFragments lists config text a Global-scope settings toggle must never
// write into the file: the empty slices a full-struct re-encode materialises
// from compiled-in defaults (topology.exclude_patterns, lsp.*.args,
// lsp.*.root_markers, lsp.*.weak_root_markers, workspace.extra_roots,
// workspace.read_roots), the defaults it freezes by VALUE where omitempty
// cannot reach (git.protected_branches, quality.analysers), and the
// [[command]] allow-list. A sparse write touches only the edited key, so none
// of these may appear after a single toggle — an explicitly-written value
// out-ranks the compiled-in default forever after, so writing one the user
// never chose freezes that default.
var neverSetFragments = []string{
	"[[command]]",
	"command = []",
	"exclude_patterns",
	"root_markers",
	"weak_root_markers",
	"args =",
	"extra_roots",
	"read_roots",
	"protected_branches",
	"analysers",
}

// defaultEmitterFragments is neverSetFragments minus the [[command]] entries:
// a file that legitimately holds user-authored [[command]] data contains that
// marker because the user wrote it, not because a save materialised it.
var defaultEmitterFragments = neverSetFragments[2:]

func readGlobalConfigFile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(config.GlobalConfigPath())
	if err != nil {
		t.Fatalf("reading global config: %v", err)
	}
	return string(data)
}

// assertNoNeverSetFragments fails when the saved config contains any
// never-set field a sparse write must leave out.
func assertNoNeverSetFragments(t *testing.T, got string) {
	t.Helper()
	for _, frag := range neverSetFragments {
		if strings.Contains(got, frag) {
			t.Errorf("toggle persisted never-set fragment %q into the config (full-struct re-encode):\n%s", frag, got)
		}
	}
}

// TestSettingsGlobalToggle_PersistsOnlyEditedKey proves a single Global-scope
// toggle persists exactly its own key: the edited key is written and none of
// the never-set fields are.
func TestSettingsGlobalToggle_PersistsOnlyEditedKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skStrict)

	m, _ = m.adjustSetting(1)

	if !m.settingsCfg.Edits.Strict {
		t.Fatal("Strict should be on after toggle")
	}
	got := readGlobalConfigFile(t)
	if !strings.Contains(got, "strict = true") {
		t.Errorf("edited key missing from saved config:\n%s", got)
	}
	assertNoNeverSetFragments(t, got)
}

// TestSettingsGlobalToggle_PreservesExistingKeys proves a second Global-scope
// toggle leaves every key the file already had intact — the [[command]] entries
// and hand-set values survive untouched, and the new toggle joins them without
// materialising any never-set field.
func TestSettingsGlobalToggle_PreservesExistingKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := config.GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	seed := "# hand-tuned\n" +
		"log_level = \"warn\"\n" +
		"\n[ui]\n" +
		"theme = \"tomorrow-night\"\n" +
		"\n[[command]]\n" +
		"name = \"deploy\"\n" +
		"exec = [\"echo\", \"deployed\"]\n" +
		"\n[[command]]\n" +
		"name = \"lint\"\n" +
		"exec = [\"make\", \"lint\"]\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seeding config: %v", err)
	}

	m := newSettingsModel()
	m.settingsCursor = cursorFor(m.settingsItems, skGitPush)
	_, _ = m.adjustSetting(1)

	got := readGlobalConfigFile(t)
	// Quote-free fragments: go-toml re-encodes string values with single quotes,
	// so the assertion must not pin the seeding file's quoting style.
	for _, want := range []string{"deployed", "lint", "tomorrow-night", "warn", "allow_push = true"} {
		if !strings.Contains(got, want) {
			t.Errorf("saved config lost pre-existing key %q:\n%s", want, got)
		}
	}
	// [[command]] here is the user's own seeded data; only the default
	// emitters are forbidden.
	for _, frag := range defaultEmitterFragments {
		if strings.Contains(got, frag) {
			t.Errorf("toggle persisted never-set fragment %q into the config (full-struct re-encode):\n%s", frag, got)
		}
	}
}
