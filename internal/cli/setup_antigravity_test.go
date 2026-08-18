package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// readAntigravityEntry reads a standalone Antigravity config file back as a
// generic map for assertions.
func readAntigravityEntry(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshalling %s: %v", path, err)
	}
	return m
}

// noBackup fails when the directory holds any backup of path — used to pin
// that an already-current config is neither rewritten nor backed up.
func noBackup(t *testing.T, path string) {
	t.Helper()
	if matches, _ := filepath.Glob(path + ".*.bak*"); len(matches) != 0 {
		t.Errorf("expected no backup of %s, found %v", path, matches)
	}
}

func TestSetupAntigravityInto_FreshConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp", "plumb.json")

	added, preserved, err := setupAntigravityInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if len(preserved) != 0 {
		t.Errorf("expected no preserved servers, got %v", preserved)
	}

	// Antigravity uses a standalone {command, args} entry — NOT an mcpServers wrapper.
	m := readAntigravityEntry(t, path)
	if m["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v, want /usr/local/bin/plumb", m["command"])
	}
	if !reflect.DeepEqual(m["args"], []any{"serve"}) {
		t.Errorf("args: got %v, want [serve]", m["args"])
	}
	if _, hasWrapper := m["mcpServers"]; hasWrapper {
		t.Error("entry must be a standalone {command,args} object, not an mcpServers wrapper")
	}
}

// TestSetupAntigravityInto_IdempotentNoRewrite pins the quiet path: a repeat
// registration at the same binary reports added=false and leaves the file
// byte-untouched — no rewrite, and therefore no backup.
func TestSetupAntigravityInto_IdempotentNoRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp", "plumb.json")

	if _, _, err := setupAntigravityInto(path, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	added, _, err := setupAntigravityInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if added {
		t.Error("expected added=false on second run (already registered)")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("an already-current config must not be rewritten")
	}
	noBackup(t, path)
}

func TestSetupAntigravityInto_RepointsAfterBinaryChange(t *testing.T) {
	dir := t.TempDir()
	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mcpDir, "plumb.json")
	if err := os.WriteFile(path, []byte(`{"command":"/old/plumb","args":["serve"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	added, _, err := setupAntigravityInto(path, "/new/plumb")
	if err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}
	if !added {
		t.Error("expected added=true when repointing a stale plumb.json")
	}
	if m := readAntigravityEntry(t, path); m["command"] != "/new/plumb" {
		t.Errorf("command not updated: got %v", m["command"])
	}
	assertHasBackup(t, path)
}

func TestSetupAntigravityInto_PreservesSiblingsAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	mcpDir := filepath.Join(dir, "mcp")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling server file and an existing (stale) plumb.json.
	if err := os.WriteFile(filepath.Join(mcpDir, "other.json"), []byte(`{"command":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mcpDir, "plumb.json")
	if err := os.WriteFile(path, []byte(`{"command":"/old/plumb","args":["serve"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	added, preserved, err := setupAntigravityInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}
	if !added {
		t.Error("expected added=true when overwriting a stale plumb.json")
	}
	if len(preserved) != 1 || preserved[0] != "other" {
		t.Errorf("expected preserved=[other], got %v", preserved)
	}

	if m := readAntigravityEntry(t, path); m["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command not updated: got %v", m["command"])
	}
	if _, err := os.Stat(filepath.Join(mcpDir, "other.json")); err != nil {
		t.Errorf("sibling other.json was removed: %v", err)
	}
	assertHasBackup(t, path)
}

// TestSetupAntigravityInto_WritesOnlyTheStandalone pins the registration
// surface: setup writes exactly the target's standalone mcp/plumb.json and
// never a legacy flat mcp_config.json or the antigravity-ide mirror, even from
// the Desktop layout the old mirror logic keyed on.
func TestSetupAntigravityInto_WritesOnlyTheStandalone(t *testing.T) {
	base := t.TempDir()
	desktopPath := filepath.Join(base, "antigravity", "mcp", "plumb.json")

	added, _, err := setupAntigravityInto(desktopPath, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if m := readAntigravityEntry(t, desktopPath); m["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v", m["command"])
	}

	for _, d := range []string{"config", "antigravity-cli", "antigravity-ide", "antigravity"} {
		if _, err := os.Stat(filepath.Join(base, d, "mcp_config.json")); !os.IsNotExist(err) {
			t.Errorf("%s/mcp_config.json must not be written by setup (err=%v)", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "antigravity-ide", "mcp", "plumb.json")); !os.IsNotExist(err) {
		t.Errorf("the antigravity-ide mirror must not be written by setup (err=%v)", err)
	}
}
