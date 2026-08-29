package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// seedGlobalConfig writes a hand-tuned global config file and returns it.
func seedGlobalConfig(t *testing.T) string {
	t.Helper()
	path := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	seed := "log_level = \"warn\"\n" +
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
	return seed
}

// decodeGlobalConfigFile parses the on-disk global config into a raw key map.
func decodeGlobalConfigFile(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(GlobalConfigPath())
	if err != nil {
		t.Fatalf("reading global config: %v", err)
	}
	m := map[string]any{}
	if err := toml.Unmarshal(data, &m); err != nil {
		t.Fatalf("decoding global config: %v", err)
	}
	return m
}

// TestSaveSparse_WritesExactlyTheChangedKey pins the core property: a
// single-settings mutation persists that key and NOTHING else. The full-struct
// Save materialises every compiled-in default into the file (empty slices as
// `= []`, git.protected_branches and quality.analysers frozen by value), which
// makes any later default dead on arrival for everyone who ever saved a
// setting; the sparse write must not.
func TestSaveSparse_WritesExactlyTheChangedKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := SaveSparse(func(c *Config) { c.LogFormat = "json" }); err != nil {
		t.Fatalf("SaveSparse: %v", err)
	}

	got := decodeGlobalConfigFile(t)
	want := map[string]any{"log_format": "json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sparse save wrote %v, want exactly %v", got, want)
	}
}

// TestSaveSparse_PreservesExistingKeys proves a second sparse save leaves every
// key the file already had intact — the [[command]] entries and hand-set values
// survive (no slice truncation, no default materialisation), and only the newly
// edited key joins them.
func TestSaveSparse_PreservesExistingKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	seedGlobalConfig(t)

	if err := SaveSparse(func(c *Config) { c.LogLevel = "debug" }); err != nil {
		t.Fatalf("SaveSparse: %v", err)
	}

	got := decodeGlobalConfigFile(t)
	if got["log_level"] != "debug" {
		t.Errorf("log_level = %v, want \"debug\"", got["log_level"])
	}
	cmds, ok := got["command"].([]any)
	if !ok || len(cmds) != 2 {
		t.Fatalf("command allow-list lost or truncated: %v", got["command"])
	}
	ui, ok := got["ui"].(map[string]any)
	if !ok || ui["theme"] != "tomorrow-night" {
		t.Errorf("[ui] theme lost or altered: %v", got["ui"])
	}
	// None of the never-set sections may materialise.
	for _, absent := range []string{"git", "quality", "topology", "workspace", "lsp", "edits", "tasks"} {
		if _, present := got[absent]; present {
			t.Errorf("sparse save materialised never-set section %q: %v", absent, got)
		}
	}
}

// TestSaveSparse_RemovesClearedCommandList covers the removal leg of the diff:
// a mutation that clears an omitempty field must delete the key from the file
// (not silently leave the stale value), since an absent key falls back to the
// compiled-in default — the same outcome a full-struct encode produces.
func TestSaveSparse_RemovesClearedCommandList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	seedGlobalConfig(t)

	if err := SaveSparse(func(c *Config) { c.Commands = nil }); err != nil {
		t.Fatalf("SaveSparse: %v", err)
	}

	got := decodeGlobalConfigFile(t)
	if _, present := got["command"]; present {
		t.Errorf("cleared command allow-list still present in file: %v", got["command"])
	}
	if got["log_level"] != "warn" {
		t.Errorf("removal disturbed log_level: %v", got["log_level"])
	}
}

// TestSaveSparse_NoChangeWritesNothing pins the no-op behaviour: a mutation
// that changes nothing must not create or rewrite the config file.
func TestSaveSparse_NoChangeWritesNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := SaveSparse(func(c *Config) {}); err != nil {
		t.Fatalf("SaveSparse: %v", err)
	}

	if _, err := os.Stat(GlobalConfigPath()); !os.IsNotExist(err) {
		t.Errorf("no-change SaveSparse created or touched the config file: %v", err)
	}
}

// TestSaveSparse_DoesNotBakeEnvOverride mirrors TestSave_DoesNotBakeEnvOverride
// for the sparse writer: the diff baseline is defaults+file without the PLUMB_*
// env overlay, and the write is the raw file map, so a transient environment
// override can never be persisted as if the user had chosen it.
func TestSaveSparse_DoesNotBakeEnvOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PLUMB_LOG_LEVEL", "debug") // a transient environment override

	if err := SaveSparse(func(c *Config) { c.LogFormat = "json" }); err != nil {
		t.Fatalf("SaveSparse: %v", err)
	}

	data, err := os.ReadFile(GlobalConfigPath())
	if err != nil {
		t.Fatalf("reading written config: %v", err)
	}
	if strings.Contains(string(data), "debug") {
		t.Errorf("SaveSparse baked the PLUMB_LOG_LEVEL=debug env override into config.toml:\n%s", data)
	}
}

// TestSaveSparse_RefusesUnreadableFile mirrors Save's refusal semantics: an
// unparseable existing config must error out and be left untouched rather than
// clobbered with recoverable settings lost.
func TestSaveSparse_RefusesUnreadableFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	broken := []byte("[ui\ntheme = ")
	if err := os.WriteFile(path, broken, 0o644); err != nil {
		t.Fatalf("seeding broken config: %v", err)
	}

	if err := SaveSparse(func(c *Config) { c.LogFormat = "json" }); err == nil {
		t.Fatal("SaveSparse succeeded on an unparseable config, want refusal")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config after refused save: %v", err)
	}
	if string(data) != string(broken) {
		t.Errorf("refused save modified the file:\n%s", data)
	}
}
