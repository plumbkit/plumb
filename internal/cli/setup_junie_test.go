package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSetupJunieInto_FreshConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp", "mcp.json")

	added, preserved, err := setupClaudeDesktopInto(path, "/usr/local/bin/plumb")
	if err != nil {
		t.Fatalf("setupClaudeDesktopInto: %v", err)
	}
	if !added {
		t.Error("expected added=true for fresh config")
	}
	if len(preserved) != 0 {
		t.Errorf("expected no preserved servers, got %v", preserved)
	}

	cfg, _, err := readOrInitClaudeConfig(path)
	if err != nil {
		t.Fatalf("reading result: %v", err)
	}
	mcpServers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers is not an object: %T", cfg["mcpServers"])
	}
	plumb, ok := mcpServers["plumb"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers.plumb is not an object: %T", mcpServers["plumb"])
	}
	if plumb["command"] != "/usr/local/bin/plumb" {
		t.Errorf("command: got %v, want /usr/local/bin/plumb", plumb["command"])
	}
	if !reflect.DeepEqual(plumb["args"], []any{"serve"}) {
		t.Errorf("args: got %v, want [serve]", plumb["args"])
	}
}

func TestJunieInstalled_DirPresence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if junieInstalled() {
		t.Error("~/.junie absent must report not installed")
	}
	if err := os.MkdirAll(filepath.Join(home, ".junie"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !junieInstalled() {
		t.Error("~/.junie present must report installed")
	}
}

func TestJunieTargetWiring(t *testing.T) {
	var target *setupTarget
	for i, c := range allSetupClients() {
		if c.use == "junie" {
			target = &allSetupClients()[i]
			break
		}
	}
	if target == nil {
		t.Fatal("no junie entry in allSetupClients()")
	}
	if target.name != "Junie" {
		t.Errorf("name: got %q, want Junie", target.name)
	}
	if target.installedFn == nil {
		t.Error("junie must set installedFn — its config file only exists once a server is configured")
	}
	if target.skillsDirFn == nil {
		t.Error("junie must set skillsDirFn — it is a verified SKILL.md client")
	}
	if target.intoFn == nil || target.extractFn == nil {
		t.Error("junie must set intoFn and extractFn")
	}
}
